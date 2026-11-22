from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
import threading
from pathlib import Path
from typing import Any

from .compare import assert_results_equal, diff_results
from .engine import EngineResult, process_full, process_glyph
from .errors import WorkbenchError
from .export import coverage_pixel_hash, export_result
from .models import Glyph, load_document
from .params import RenderParams
from .samplegen import generate_samples

EXIT_CODES = {
    "E_SCHEMA": 2,
    "E_CURVE": 3,
    "E_DECODE": 4,
    "E_PARA": 5,
    "E_STATE": 6,
    "E_DIFF": 7,
    "E_IO": 8,
}


def _print_json(payload: dict[str, Any], *, stream: Any = None) -> None:
    (stream or sys.stdout).write(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=False) + "\n"
    )


def _params(args: argparse.Namespace) -> RenderParams:
    return RenderParams(
        pixels_per_unit=args.ppu,
        tolerance=args.tolerance,
        grid=args.grid,
        max_workers=args.workers,
        max_buffer_bytes=int(args.buffer_mb * 1024 * 1024),
    )


def _load_glyph(args: argparse.Namespace) -> Glyph:
    document = load_document(args.model)
    return document.find_glyph(args.glyph)


def _result_payload(result: EngineResult) -> dict[str, Any]:
    payload = result.summary()
    payload["coverage_pixel_sha256"] = coverage_pixel_hash(result)
    payload["pixel_count"] = result.raster.width * result.raster.height
    payload["states"] = result.states
    return payload


def command_generate(args: argparse.Namespace) -> int:
    paths = generate_samples(args.output)
    _print_json(
        {
            "ok": True,
            "samples": str(paths["samples"]),
            "truth": str(paths["truth"]),
            "corrupt_dir": str(paths["corrupt_dir"]),
        }
    )
    return 0


def command_validate(args: argparse.Namespace) -> int:
    document = load_document(args.model)
    glyphs = []
    for glyph in document.glyphs:
        glyphs.append(
            {
                "glyph_id": glyph.glyph_id,
                "contours": len(glyph.contours),
                "segments": sum(len(contour.segments) for contour in glyph.contours),
                "curve_types": sorted(
                    {
                        segment.kind
                        for contour in glyph.contours
                        for segment in contour.segments
                    }
                ),
                "bbox": list(glyph.truth.bbox) if glyph.truth else None,
                "area": glyph.truth.area if glyph.truth else None,
            }
        )
    corrupt = []
    corrupt_dir = Path(args.corrupt_dir)
    manifest_path = corrupt_dir.parent / "manifest.json"
    expected: dict[str, str] = {}
    if manifest_path.exists():
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        expected = dict(manifest.get("corrupt_cases", {}))
    for path in sorted(corrupt_dir.glob("*.json")):
        try:
            load_document(path)
            actual = "NO_ERROR"
            context = {}
        except WorkbenchError as exc:
            actual = exc.code
            context = exc.context
        expected_code = expected.get(path.name)
        corrupt.append(
            {
                "file": path.name,
                "expected": expected_code,
                "actual": actual,
                "pass": expected_code == actual,
                "context": context,
            }
        )
    ok = all(item["pass"] for item in corrupt)
    _print_json(
        {
            "ok": ok,
            "schema": document.schema,
            "glyphs": glyphs,
            "corrupt_cases": corrupt,
        }
    )
    return 0 if ok else EXIT_CODES["E_SCHEMA"]


def command_render(args: argparse.Namespace) -> int:
    glyph = _load_glyph(args)
    params = _params(args)
    crash_counter = {"value": 0}
    callback = None
    if args.crash_after is not None:
        if args.crash_after <= 0:
            raise WorkbenchError("E_PARA", "--crash-after must be positive")

        def callback(index: int, result: EngineResult) -> None:
            crash_counter["value"] += 1
            if crash_counter["value"] >= args.crash_after:
                os._exit(17)

    pause_event = threading.Event()
    if args.pause_after is not None:
        if args.pause_after < 0:
            raise WorkbenchError("E_PARA", "--pause-after must be non-negative")
        if args.pause_after == 0:
            pause_event.set()

        def pause_callback(index: int, result: EngineResult) -> None:
            if result.states["committed_count"] >= args.pause_after:
                pause_event.set()
    else:
        pause_callback = None

    result = process_glyph(
        glyph,
        params,
        persist_root=args.persist,
        force_full=args.full,
        pause_event=pause_event,
        on_commit=callback or pause_callback,
    )
    payload = _result_payload(result)
    if args.export:
        payload["exports"] = export_result(result, args.export)
    _print_json(payload)
    return 0 if result.complete or result.paused else 1


def _consistency_live(
    glyph: Glyph,
    params: RenderParams,
    persist_root: str,
    pause_after: int,
) -> EngineResult:
    event = threading.Event()
    if pause_after <= 0:
        event.set()

    def pause_callback(index: int, result: EngineResult) -> None:
        if result.states["committed_count"] >= pause_after:
            event.set()

    result = process_glyph(
        glyph,
        params,
        persist_root=persist_root,
        pause_event=event,
        on_commit=pause_callback,
    )
    if result.paused:
        result = process_glyph(
            glyph,
            params,
            persist_root=persist_root,
            pause_event=threading.Event(),
        )
    if not result.complete:
        raise WorkbenchError(
            "E_STATE",
            "live incremental path did not complete",
            failures=result.failures,
        )
    return result


def _run_crash_subprocess(
    model: str,
    glyph_id: str,
    params: RenderParams,
    persist_root: str,
    crash_after: int,
) -> None:
    command = [
        sys.executable,
        "-m",
        "glyph_workbench.cli",
        "render",
        "--model",
        model,
        "--glyph",
        glyph_id,
        "--ppu",
        str(params.pixels_per_unit),
        "--tolerance",
        str(params.tolerance),
        "--grid",
        str(params.grid),
        "--workers",
        str(params.max_workers),
        "--buffer-mb",
        str(params.max_buffer_bytes / (1024 * 1024)),
        "--persist",
        persist_root,
        "--full",
        "--crash-after",
        str(crash_after),
    ]
    completed = subprocess.run(
        command,
        capture_output=True,
        text=True,
        check=False,
    )
    if completed.returncode != 17:
        raise WorkbenchError(
            "E_STATE",
            "crash injection subprocess did not exit with the expected code",
            returncode=completed.returncode,
            stdout=completed.stdout,
            stderr=completed.stderr,
        )


def command_consistency(args: argparse.Namespace) -> int:
    glyph = _load_glyph(args)
    params = _params(args)
    with tempfile.TemporaryDirectory(prefix="glyph-consistency-") as temporary:
        root = Path(temporary)
        full = process_full(glyph, params)
        if not full.complete:
            raise WorkbenchError("E_STATE", "full recompute did not complete")
        live = _consistency_live(
            glyph,
            params,
            str(root / "live-checkpoints"),
            pause_after=min(1, max(1, len(glyph.contours) - 1)),
        )
        crash_root = str(root / "crash-checkpoints")
        crash_after = 1
        _run_crash_subprocess(
            args.model,
            glyph.glyph_id,
            params,
            crash_root,
            crash_after,
        )
        recovered = process_glyph(glyph, params, persist_root=crash_root)
        if not recovered.complete:
            raise WorkbenchError(
                "E_STATE",
                "crash recovery path did not complete",
                failures=recovered.failures,
            )
        assert_results_equal(full, live)
        assert_results_equal(full, recovered)
        full_vs_live = diff_results(full, live).to_dict()
        full_vs_resume = diff_results(full, recovered).to_dict()

    assert not glyph.truth or (
        abs(full.raster.area - glyph.truth.area) / glyph.truth.area <= 0.01
    )
    _print_json(
        {
            "ok": True,
            "glyph_id": glyph.glyph_id,
            "pixel_exact": True,
            "width": full.raster.width,
            "height": full.raster.height,
            "grid": params.grid,
            "tolerance": params.tolerance,
            "pixels_per_unit": params.pixels_per_unit,
            "coverage_pixel_sha256": coverage_pixel_hash(full),
            "commit_sequence": full.commit_sequence,
            "full_vs_live": full_vs_live,
            "full_vs_resume": full_vs_resume,
            "live_loaded_from_checkpoint": live.loaded_from_checkpoint,
            "resume_loaded_from_checkpoint": recovered.loaded_from_checkpoint,
            "area": full.raster.area,
            "truth_area": glyph.truth.area if glyph.truth else None,
            "area_relative_error": (
                abs(full.raster.area - glyph.truth.area) / glyph.truth.area
                if glyph.truth
                else None
            ),
        }
    )
    return 0


def command_demo(args: argparse.Namespace) -> int:
    sample_path = Path(args.model)
    if not sample_path.exists():
        paths = generate_samples(args.output)
        sample_path = paths["samples"]
    document = load_document(sample_path)
    params = _params(args)
    summaries = []
    for glyph in document.glyphs:
        with tempfile.TemporaryDirectory(prefix=f"glyph-demo-{glyph.glyph_id}-") as temporary:
            root = Path(temporary)
            full = process_full(glyph, params)
            live = _consistency_live(
                glyph,
                params,
                str(root / "live"),
                pause_after=1,
            )
            assert_results_equal(full, live)
            summaries.append(
                {
                    "glyph_id": glyph.glyph_id,
                    "pixel_exact": True,
                    "width": full.raster.width,
                    "height": full.raster.height,
                    "area": full.raster.area,
                    "truth_area": glyph.truth.area if glyph.truth else None,
                    "commit_sequence": full.commit_sequence,
                }
            )
    _print_json({"ok": True, "model": str(sample_path), "glyphs": summaries})
    return 0


def command_doctor(args: argparse.Namespace) -> int:
    import numpy
    import PySide6

    persist = Path(args.persist)
    persist.mkdir(parents=True, exist_ok=True)
    probe = persist / ".write_probe"
    probe.write_text("ok", encoding="utf-8")
    probe_text = probe.read_text(encoding="utf-8")
    probe.unlink()
    _print_json(
        {
            "ok": probe_text == "ok",
            "python": sys.version.split()[0],
            "numpy": numpy.__version__,
            "pyside6": PySide6.__version__,
            "persist": str(persist.resolve()),
            "writable": os.access(persist, os.W_OK),
            "display": os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY"),
            "qt_platform": os.environ.get("QT_QPA_PLATFORM"),
        }
    )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui import launch_gui

    return launch_gui(
        model_path=args.model,
        persist_root=args.persist,
        state_model=args.state_model,
        glyph_id=args.glyph,
        ppu=args.ppu,
        tolerance=args.tolerance,
        grid=args.grid,
        workers=args.workers,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="glyph-workbench",
        description="Deterministic glyph outline editor and raster workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--output", default="samples")
    generate.set_defaults(func=command_generate)

    validate = subparsers.add_parser("validate", help="validate samples and corrupt cases")
    validate.add_argument("--model", default="samples/glyph_samples.json")
    validate.add_argument("--corrupt-dir", default="samples/corrupt")
    validate.set_defaults(func=command_validate)

    render = subparsers.add_parser("render", help="rasterize one glyph")
    render.add_argument("--model", default="samples/glyph_samples.json")
    render.add_argument("--glyph", required=True)
    render.add_argument("--ppu", type=float, default=0.25)
    render.add_argument("--tolerance", type=float, default=0.35)
    render.add_argument("--grid", type=int, default=8)
    render.add_argument("--workers", type=int, default=2)
    render.add_argument("--buffer-mb", type=float, default=64.0)
    render.add_argument("--persist", default="persist")
    render.add_argument("--full", action="store_true", help="ignore checkpoints")
    render.add_argument("--pause-after", type=int)
    render.add_argument("--crash-after", type=int)
    render.add_argument("--export", help="output base path without suffix")
    render.set_defaults(func=command_render)

    consistency = subparsers.add_parser(
        "consistency", help="compare full, live-incremental, and crash-resume paths"
    )
    consistency.add_argument("--model", default="samples/glyph_samples.json")
    consistency.add_argument("--glyph", required=True)
    consistency.add_argument("--ppu", type=float, default=0.25)
    consistency.add_argument("--tolerance", type=float, default=0.35)
    consistency.add_argument("--grid", type=int, default=8)
    consistency.add_argument("--workers", type=int, default=2)
    consistency.add_argument("--buffer-mb", type=float, default=64.0)
    consistency.set_defaults(func=command_consistency)

    demo = subparsers.add_parser("demo", help="run local deterministic verification")
    demo.add_argument("--model", default="samples/glyph_samples.json")
    demo.add_argument("--output", default="samples")
    demo.add_argument("--ppu", type=float, default=0.25)
    demo.add_argument("--tolerance", type=float, default=0.35)
    demo.add_argument("--grid", type=int, default=8)
    demo.add_argument("--workers", type=int, default=2)
    demo.add_argument("--buffer-mb", type=float, default=64.0)
    demo.set_defaults(func=command_demo)

    doctor = subparsers.add_parser("doctor", help="check runtime and persistence")
    doctor.add_argument("--persist", default="persist")
    doctor.set_defaults(func=command_doctor)

    gui = subparsers.add_parser("gui", help="launch the PySide6 desktop workbench")
    gui.add_argument("--model", default="samples/glyph_samples.json")
    gui.add_argument("--state-model", default="persist/runtime/glyph_model.json")
    gui.add_argument("--persist", default="persist")
    gui.add_argument("--glyph", default=None)
    gui.add_argument("--ppu", type=float, default=0.25)
    gui.add_argument("--tolerance", type=float, default=0.35)
    gui.add_argument("--grid", type=int, default=8)
    gui.add_argument("--workers", type=int, default=2)
    gui.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _print_json(exc.as_dict(), stream=sys.stderr)
        return EXIT_CODES.get(exc.code, 1)
    except KeyboardInterrupt:
        _print_json(
            {"ok": False, "code": "E_INTERRUPTED", "message": "interrupted"},
            stream=sys.stderr,
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

