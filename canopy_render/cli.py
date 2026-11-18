from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any

from .compare import compare_outputs
from .renderer import SceneValidationError, render_scene
from .scene import generate_scene
from .util import write_json_atomic
from .validation import validate_scene


class _GracefulStop:
    def __init__(self) -> None:
        self.requested = False

    def handler(self, _signum: int, _frame: Any) -> None:
        self.requested = True


def _read_scene(path: Path) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ValueError("scene root must be an object")
    return value


def _write_validation_failure(
    path: Path,
    issues: list[Any],
) -> None:
    payload = {
        "schema_version": "1.0",
        "status": "rejected",
        "business_code": "INVALID_SCENE",
        "issue_count": len(issues),
        "issues": [issue.as_dict() for issue in issues],
    }
    write_json_atomic(path, payload)


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="canopy-render",
        description="Deterministic offline canopy renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate-scene",
        help="write the deterministic validation scene",
    )
    generate.add_argument("--out", required=True, type=Path)
    generate.add_argument("--width", type=int, default=192)
    generate.add_argument("--height", type=int, default=160)
    generate.add_argument("--tile-width", type=int, default=64)
    generate.add_argument("--tile-height", type=int, default=40)

    validate_parser = subparsers.add_parser(
        "validate",
        help="validate a scene without rendering",
    )
    validate_parser.add_argument("--scene", required=True, type=Path)
    validate_parser.add_argument("--report", required=True, type=Path)

    render_parser = subparsers.add_parser(
        "render",
        help="render a scene, resuming a committed snapshot when possible",
    )
    render_parser.add_argument("--scene", required=True, type=Path)
    render_parser.add_argument("--out", required=True, type=Path)
    render_parser.add_argument("--snapshot", type=Path, default=None)
    render_parser.add_argument(
        "--interrupt-after-tiles",
        type=int,
        default=None,
        help="finish the current tile, commit, write a recovery report and exit 0",
    )
    render_parser.add_argument(
        "--hard-kill-after-tiles",
        type=int,
        default=None,
        help="test hook: uncatchable process exit after the snapshot commit",
    )
    render_parser.add_argument(
        "--reference",
        type=Path,
        default=None,
        help="reference output directory to compare after completion",
    )

    compare_parser = subparsers.add_parser(
        "compare",
        help="compare a completed output directory with a reference",
    )
    compare_parser.add_argument("--scene", required=True, type=Path)
    compare_parser.add_argument("--output", required=True, type=Path)
    compare_parser.add_argument("--reference", required=True, type=Path)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)

    if args.command == "generate-scene":
        scene = generate_scene(
            args.width,
            args.height,
            args.tile_width,
            args.tile_height,
        )
        write_json_atomic(args.out, scene)
        return 0

    try:
        scene = _read_scene(args.scene)
    except Exception as exc:
        parser.exit(64, f"cannot read scene JSON: {exc}\n")
        return 64

    if args.command == "validate":
        validation = validate_scene(scene)
        write_json_atomic(args.report, validation.report())
        if validation.issues:
            print(
                f"rejected: {len(validation.issues)} structured validation issue(s)",
                file=sys.stderr,
            )
            return 2
        print("valid")
        return 0

    if args.command == "compare":
        validation = validate_scene(scene)
        if not validation.ok or validation.data is None:
            _write_validation_failure(
                args.output / "validation_errors.json",
                validation.issues,
            )
            return 2
        layout = validation.data
        from .snapshot import TileLayout

        tile_layout = TileLayout(
            layout.width,
            layout.height,
            layout.tile_width,
            layout.tile_height,
        )
        result = compare_outputs(args.output, args.reference, tile_layout)
        if result["status"] != "match":
            print(
                f"mismatch: {result['difference_count']} channel difference(s), "
                f"{result['projection_mismatch_count']} projection mismatch(es)",
                file=sys.stderr,
            )
            return 3
        print("match")
        return 0

    if args.command == "render":
        validation = validate_scene(scene)
        if not validation.ok or validation.data is None:
            args.out.mkdir(parents=True, exist_ok=True)
            _write_validation_failure(
                args.out / "validation_errors.json",
                validation.issues,
            )
            print(
                f"rejected: {len(validation.issues)} structured validation issue(s)",
                file=sys.stderr,
            )
            return 2
        stop = _GracefulStop()
        previous_int = signal.signal(signal.SIGINT, stop.handler)
        previous_term = signal.signal(signal.SIGTERM, stop.handler)
        try:
            result = render_scene(
                scene,
                args.out,
                snapshot_path=args.snapshot,
                stop_requested=lambda: stop.requested,
                interrupt_after_tiles=args.interrupt_after_tiles,
                hard_kill_after_tiles=args.hard_kill_after_tiles,
            )
        finally:
            signal.signal(signal.SIGINT, previous_int)
            signal.signal(signal.SIGTERM, previous_term)
        if result.status == "interrupted":
            print(
                f"interrupted after {result.report['tile_layout']['completed_tile_count']} "
                f"of {result.layout.total} tiles"
            )
            return 0
        if args.reference is not None:
            comparison = compare_outputs(
                args.out,
                args.reference,
                result.layout,
            )
            if comparison["status"] != "match":
                print(
                    f"mismatch: {comparison['difference_count']} channel "
                    f"difference(s), {comparison['projection_mismatch_count']} "
                    "projection mismatch(es)",
                    file=sys.stderr,
                )
                return 3
        print(
            f"complete: {result.layout.total} tiles, "
            f"recovery_count={result.report['recovery']['recovery_count']}"
        )
        return 0

    return 64


if __name__ == "__main__":
    raise SystemExit(main())

