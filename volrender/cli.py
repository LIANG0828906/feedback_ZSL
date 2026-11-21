"""Command line entry points for generation, demo, verification and recovery."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .buffers import BufferStore
from .pipeline import generate_project_assets, project_directories, run_demo
from .snapshot import load_snapshot, recover_from_snapshot
from .verification import run_acceptance
from .volume import load_volume


def _summary(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=True, sort_keys=True, indent=2)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="volrender",
        description="Deterministic, hash-bound local volume rendering toolkit",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate samples, buffers, snapshots and traces")
    generate.add_argument("--root", default="output")
    generate.add_argument("--seed", type=int, default=19093)
    generate.add_argument("--sizes", type=int, nargs="+", default=[64, 128])

    demo = subparsers.add_parser("demo", help="run the fixed interaction/determinism scenario")
    demo.add_argument("--root", default="output")
    demo.add_argument("--size", type=int, default=64, choices=(64, 128))

    verify = subparsers.add_parser("verify", help="run acceptance checks and write a report")
    verify.add_argument("--root", default="output")
    verify.add_argument("--generate", action="store_true", help="regenerate artifacts first")

    gui = subparsers.add_parser("gui", help="start the PyQt5 example")
    gui.add_argument("--root", default="output")
    gui.add_argument("--size", type=int, default=64, choices=(64, 128))

    recover = subparsers.add_parser("recover", help="restore only from an on-disk snapshot")
    recover.add_argument("--root", default="output")
    recover.add_argument("--snapshot", required=True)
    recover.add_argument("--volume", required=True)

    inspect = subparsers.add_parser("inspect", help="print validated volume header metadata")
    inspect.add_argument("volume")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "generate":
        report = generate_project_assets(args.root, sizes=tuple(args.sizes), seed=args.seed)
        print(_summary(report))
        return 0
    if args.command == "demo":
        report = run_demo(args.root, size=args.size)
        print(_summary(report))
        return 0
    if args.command == "verify":
        if args.generate:
            generate_project_assets(args.root)
        report = run_acceptance(args.root)
        print(_summary({"passed": report["passed"], "checks": report["check_count"], "failures": report["failed_count"]}))
        return 0 if report["passed"] else 1
    if args.command == "gui":
        from .app import main as gui_main

        return gui_main(["--root", str(args.root), "--size", str(args.size)])
    if args.command == "recover":
        directories = project_directories(args.root)
        volume = load_volume(args.volume)
        snapshot = load_snapshot(args.snapshot)
        buffer_store = BufferStore(directories["buffers"])
        outcome = recover_from_snapshot(args.snapshot, volume, buffer_store)
        print(
            _summary(
                {
                    "parameter_hash": outcome.result.parameter_hash,
                    "buffer_hash": outcome.result.buffer_hash,
                    "snapshot_id": snapshot.snapshot_id,
                    "reused_buffer": outcome.reused_buffer,
                    "rerendered": outcome.rerendered,
                    "source": "disk_snapshot_only",
                }
            )
        )
        return 0
    if args.command == "inspect":
        volume = load_volume(args.volume)
        print(_summary(volume.header.to_dict() | {"content_sha256": volume.content_sha256}))
        return 0
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
