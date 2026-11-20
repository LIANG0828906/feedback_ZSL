from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import threading
import time
from pathlib import Path
from typing import Any

from .binary import atomic_write_bytes, file_sha256, load_mesh
from .boundary import boundary_report
from .coordinator import RepairConfig, RepairService
from .errors import (
    BusinessError,
    E_BOUNDARY_PENDING,
    E_BUDGET_EXCEEDED,
    E_BUSY,
    E_CANCELLED,
    E_MESH_INVALID,
    E_RECOVERY_INVALID,
)
from .render import RenderLoop, write_static_preview
from .samples import generate_sample_bundle
from .snapshot import SnapshotStore
from .topology import build_topology
from .weld import weld_vertices


EXIT_CODES = {
    E_MESH_INVALID: 2,
    E_BUDGET_EXCEEDED: 3,
    E_BUSY: 4,
    E_BOUNDARY_PENDING: 5,
    E_RECOVERY_INVALID: 6,
    E_CANCELLED: 130,
}


def parse_size(value: str) -> int:
    text = value.strip().upper().replace("_", "")
    multiplier = 1
    if text.endswith("KIB"):
        multiplier, text = 1024, text[:-3]
    elif text.endswith("MIB"):
        multiplier, text = 1024**2, text[:-3]
    elif text.endswith("GIB"):
        multiplier, text = 1024**3, text[:-3]
    elif text.endswith("KB"):
        multiplier, text = 1000, text[:-2]
    elif text.endswith("MB"):
        multiplier, text = 1000**2, text[:-2]
    elif text.endswith("GB"):
        multiplier, text = 1000**3, text[:-2]
    elif text.endswith("K"):
        multiplier, text = 1024, text[:-1]
    elif text.endswith("M"):
        multiplier, text = 1024**2, text[:-1]
    elif text.endswith("G"):
        multiplier, text = 1024**3, text[:-1]
    try:
        return int(float(text) * multiplier)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"invalid byte size: {value}") from exc


def parse_blocks(value: str) -> tuple[int, int, int]:
    pieces = value.lower().replace("x", ",").split(",")
    if len(pieces) != 3:
        raise argparse.ArgumentTypeError("--blocks expects X,Y,Z")
    try:
        return tuple(int(piece) for piece in pieces)  # type: ignore[return-value]
    except ValueError as exc:
        raise argparse.ArgumentTypeError("--blocks values must be integers") from exc


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mesh-repair",
        description="Chunked, budgeted triangular mesh repair for asset ingestion.",
    )
    parser.add_argument("--version", action="version", version="mesh-repair 0.1.0")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic sample bundle")
    generate.add_argument("--outdir", default="samples")

    inspect = subparsers.add_parser("inspect", help="inspect binary header and topology")
    inspect.add_argument("mesh")

    compare = subparsers.add_parser(
        "compare-tolerance", help="re-weld at two tolerances and diff boundary loops"
    )
    compare.add_argument("mesh")
    compare.add_argument("--old", type=float, default=1e-6)
    compare.add_argument("--new", type=float, default=1e-3)
    compare.add_argument("--json-out")

    repair = subparsers.add_parser("repair", help="run budgeted chunked repair")
    repair.add_argument("mesh")
    repair.add_argument("-o", "--output", required=True)
    repair.add_argument("--report")
    repair.add_argument("--work-dir", default=".mesh-repair-work")
    repair.add_argument("--tolerance", type=float, default=1e-6)
    repair.add_argument("--blocks", type=parse_blocks, default=(2, 2, 1))
    repair.add_argument("--parallelism", type=int, default=2)
    repair.add_argument("--global-budget", type=parse_size, default=8 * 1024 * 1024)
    repair.add_argument("--task-budget", type=parse_size, default=4 * 1024 * 1024)
    repair.add_argument("--min-fragment-faces", type=int, default=3)
    repair.add_argument("--area-epsilon", type=float, default=1e-12)
    repair.add_argument("--normal-tolerance", type=float, default=30.0)
    repair.add_argument(
        "--render",
        action="store_true",
        help="run an OpenGL preview thread; automatically falls back to SVG",
    )
    repair.add_argument(
        "--preview",
        help="preview path (PNG for OpenGL, SVG for static fallback)",
    )
    repair.add_argument(
        "--json",
        action="store_true",
        help="print the final report JSON to stdout",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            summary = generate_sample_bundle(args.outdir)
            print(json.dumps(summary, indent=2, sort_keys=True))
            return 0
        if args.command == "inspect":
            return _inspect(args)
        if args.command == "compare-tolerance":
            return _compare_tolerance(args)
        if args.command == "repair":
            return _repair(args)
        parser.error("unknown command")
        return 2
    except BusinessError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True), file=sys.stderr)
        return EXIT_CODES.get(exc.code, 1)
    except KeyboardInterrupt:
        print(json.dumps({"ok": False, "code": E_CANCELLED, "message": "interrupted"}, sort_keys=True), file=sys.stderr)
        return 130


def _inspect(args: argparse.Namespace) -> int:
    mesh, header = load_mesh(args.mesh)
    topology = build_topology(mesh)
    weld = weld_vertices(mesh, 1e-6)
    payload = {
        "ok": True,
        "path": args.mesh,
        "sha256": file_sha256(args.mesh),
        "header": header.to_dict(),
        "topology": topology.summary(),
        "boundaries": boundary_report(mesh, topology),
        "weld_1e-6": weld.summary(),
    }
    print(json.dumps(payload, indent=2, sort_keys=True))
    return 0


def _compare_tolerance(args: argparse.Namespace) -> int:
    mesh, _ = load_mesh(args.mesh)
    service = RepairService(".mesh-repair-work")
    result = service.compare_tolerance(
        args.mesh,
        old_tolerance=args.old,
        new_tolerance=args.new,
    )
    text = json.dumps(result, indent=2, sort_keys=True)
    if args.json_out:
        atomic_write_bytes(args.json_out, (text + "\n").encode("utf-8"))
    print(text)
    return 0


def _repair(args: argparse.Namespace) -> int:
    config = RepairConfig(
        tolerance=args.tolerance,
        block_shape=args.blocks,
        parallelism=args.parallelism,
        global_budget_bytes=args.global_budget,
        task_budget_bytes=args.task_budget,
        min_fragment_faces=args.min_fragment_faces,
        area_epsilon=args.area_epsilon,
        normal_tolerance_deg=args.normal_tolerance,
    ).validate()
    preview_path = args.preview
    if args.render and preview_path is None:
        preview_path = str(Path(args.output).with_suffix(".preview.png"))
    service = RepairService(args.work_dir)
    background = service.start_background(
        args.mesh,
        args.output,
        config=config,
        report_path=args.report,
    )
    cancel_requested = threading.Event()

    previous_handler = None

    def handle_interrupt(signum: int, frame: Any) -> None:
        cancel_requested.set()
        background.cancel()

    try:
        previous_handler = signal.signal(signal.SIGINT, handle_interrupt)
    except ValueError:
        previous_handler = None

    render_stop = threading.Event()
    render_thread: threading.Thread | None = None
    render_loop: RenderLoop | None = None
    if args.render:
        render_loop = RenderLoop(
            background.snapshot_store,
            stop_event=render_stop,
            preview_path=preview_path,
        )
        render_thread = threading.Thread(
            target=render_loop.run, name="mesh-render", daemon=True
        )
        render_thread.start()

    error: BaseException | None = None
    report: dict[str, Any] | None = None
    try:
        report = background.join()
    except BaseException as exc:
        error = exc
    finally:
        render_stop.set()
        if render_thread is not None:
            render_thread.join(timeout=10.0)
        if previous_handler is not None:
            signal.signal(signal.SIGINT, previous_handler)

    if error is not None:
        if args.preview and not args.render:
            snapshot = background.snapshot_store.latest()
            write_static_preview(snapshot, args.preview)
        raise error

    assert report is not None
    render_status = None
    if render_loop is not None and render_loop.status is not None:
        render_status = render_loop.status.to_dict()
    elif preview_path:
        rendered = write_static_preview(
            background.snapshot_store.latest(), preview_path
        )
        render_status = {
            "mode": "static",
            "preview_path": rendered,
            "message": "static preview requested",
            "frames": 0,
        }
    if render_status:
        report["render"] = render_status
        report_file = Path(args.report) if args.report else Path(args.output + ".report.json")
        atomic_write_bytes(
            report_file,
            (
                json.dumps(report, indent=2, sort_keys=True, allow_nan=False)
                + "\n"
            ).encode("utf-8"),
        )
        render_status_path = Path(str(report_file) + ".render.json")
        atomic_write_bytes(
            render_status_path,
            (json.dumps(render_status, indent=2, sort_keys=True) + "\n").encode("utf-8"),
        )
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        print(
            f"repaired {args.mesh} -> {args.output} "
            f"({report['artifact_vertices']} vertices, {report['artifact_faces']} faces, "
            f"status={report['status']})"
        )
        print(f"report: {args.report or args.output + '.report.json'}")
        print(f"trace: {report['trace_path']}")
        if render_status:
            print(f"render: {render_status['mode']} {render_status.get('preview_path')}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
