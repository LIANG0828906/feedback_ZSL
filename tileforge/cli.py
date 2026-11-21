"""Command-line entry points for local verification and operations."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .binary import parse_sequence
from .config import JobConfig
from .errors import TileForgeError
from .pipeline import convert_incremental, offline_convert
from .preview import PreviewCursor, load_offline_report, preview_frame
from .samplegen import generate_samples
from .store import VersionStore


def _print_json(value: Any) -> None:
    print(json.dumps(value, sort_keys=True, indent=2, ensure_ascii=False))


def _config(path: str) -> JobConfig:
    return JobConfig.load(path)


def _default_workspace(args: argparse.Namespace) -> Path:
    return Path(args.workspace)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="tileforge", description="Deterministic tiled HDR conversion workbench")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=190)

    inspect = subparsers.add_parser("inspect", help="validate and inspect a sequence")
    inspect.add_argument("sequence")

    convert = subparsers.add_parser("convert", help="incrementally convert and publish a sequence")
    convert.add_argument("--input", required=True)
    convert.add_argument("--config", required=True)
    convert.add_argument("--workspace", default="artifacts/workbench")
    convert.add_argument("--expected-version-hash")
    convert.add_argument("--no-resume", action="store_true")
    convert.add_argument("--restart", action="store_true")
    convert.add_argument("--trace")
    convert.add_argument("--crash-after-commits", type=int, default=None, help=argparse.SUPPRESS)

    offline = subparsers.add_parser("offline", help="convert a whole sequence to a standalone output")
    offline.add_argument("--input", required=True)
    offline.add_argument("--output", required=True)
    offline.add_argument("--config", required=True)
    offline.add_argument("--trace")

    preview = subparsers.add_parser("preview", help="read published preview frames")
    preview.add_argument("--workspace", default="artifacts/workbench")
    preview.add_argument("--expected-version-hash")
    preview.add_argument("--offline-report")
    preview.add_argument("--once", action="store_true")
    preview.add_argument("--steps", type=int, default=None)

    versions = subparsers.add_parser("versions", help="list published versions")
    versions.add_argument("--workspace", default="artifacts/workbench")

    rollback = subparsers.add_parser("rollback", help="atomically point CURRENT at a historical version")
    rollback.add_argument("--workspace", default="artifacts/workbench")
    rollback.add_argument("--version", type=int, required=True)
    rollback.add_argument("--expected-current-hash")

    workbench = subparsers.add_parser("workbench", help="start the PySide6 workbench")
    workbench.add_argument("--input", required=True)
    workbench.add_argument("--config", required=True)
    workbench.add_argument("--workspace", default="artifacts/workbench")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            _print_json(generate_samples(args.out, args.seed))
            return 0
        if args.command == "inspect":
            sequence = parse_sequence(args.sequence)
            _print_json(
                {
                    "ok": True,
                    "file_sha256": sequence.file_sha256,
                    "header": {
                        "width": sequence.header.width,
                        "height": sequence.header.height,
                        "tiles_x": sequence.header.tiles_x,
                        "tiles_y": sequence.header.tiles_y,
                        "tile_width": sequence.header.tile_width,
                        "tile_height": sequence.header.tile_height,
                        "bit_depth": sequence.header.bit_depth,
                        "channels": sequence.header.channels,
                        "source_transfer": sequence.header.source_transfer.name,
                        "target_transfer": sequence.header.target_transfer.name,
                        "conversion_id": sequence.header.conversion_id,
                        "block_count": sequence.header.block_count,
                        "data_length": sequence.header.data_length,
                    },
                    "tiles": [
                        {
                            "tile_id": block.tile_id,
                            "exposure": block.exposure,
                            "content_hash": block.content_hash.hex(),
                            "payload_bytes": block.payload_size,
                        }
                        for block in sequence.ordered_blocks()
                    ],
                }
            )
            return 0
        if args.command == "convert":
            summary = convert_incremental(
                args.input,
                _config(args.config),
                args.workspace,
                expected_base_hash=args.expected_version_hash,
                resume=not args.no_resume,
                restart=args.restart,
                trace_path=args.trace,
                crash_after_commits=args.crash_after_commits,
            )
            _print_json(summary.to_dict())
            return 0
        if args.command == "offline":
            summary = offline_convert(args.input, args.output, _config(args.config), trace_path=args.trace)
            _print_json(summary.to_dict())
            return 0
        if args.command == "preview":
            if args.offline_report:
                cursor = PreviewCursor(args.workspace, expected_version_hash=args.expected_version_hash)
                frame = cursor.refresh()
                cursor.advance_to_end(max_steps=args.steps)
                if cursor.at_end:
                    _print_json(cursor.compare_offline(load_offline_report(args.offline_report)))
                else:
                    _print_json({**frame, "advanced": cursor.cursor, "at_end": False})
                return 0
            _print_json(preview_frame(args.workspace, args.expected_version_hash))
            return 0
        if args.command == "versions":
            store = VersionStore(args.workspace)
            versions = store.list_versions()
            current = None
            try:
                current = store.load_current().version_hash
            except TileForgeError:
                pass
            _print_json(
                {
                    "ok": True,
                    "current_version_hash": current,
                    "versions": [
                        {
                            "version_id": item["version_id"],
                            "version_hash": item["version_hash"],
                            "parent_version_id": item.get("parent_version_id"),
                            "affected_tiles": item.get("affected_tiles"),
                            "reused_tiles": item.get("reused_tiles"),
                        }
                        for item in versions
                    ],
                }
            )
            return 0
        if args.command == "rollback":
            manifest = VersionStore(args.workspace).rollback(args.version, args.expected_current_hash)
            _print_json(
                {
                    "ok": True,
                    "version_id": manifest["version_id"],
                    "version_hash": manifest["version_hash"],
                    "rolled_back": True,
                }
            )
            return 0
        if args.command == "workbench":
            from .gui import run_gui

            return run_gui(args.input, args.config, args.workspace)
        parser.error("unknown command")
        return 2
    except TileForgeError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True, indent=2, ensure_ascii=False), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(json.dumps({"ok": False, "code": "E_CANCELLED", "message": "interrupted"}), file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
