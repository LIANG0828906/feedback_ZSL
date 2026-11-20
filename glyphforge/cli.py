from __future__ import annotations

import argparse
import json
import sys
import threading
from pathlib import Path
from typing import Any

from .errors import ErrorCode, GlyphForgeError, RunCancelled
from .parser import load_font
from .pipeline import run_pipeline
from .samples import generate_samples
from .util import canonical_json
from .validation import validate_samples


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _previous_summaries_from_report(path: str | None) -> dict[str, dict[str, Any]] | None:
    if not path:
        return None
    document = json.loads(Path(path).read_text(encoding="utf-8"))
    if document.get("format") == "glyphforge.sample-manifest/1":
        return None
    glyphs = document.get("glyphs", [])
    result: dict[str, dict[str, Any]] = {}
    for glyph in glyphs:
        if isinstance(glyph, dict) and glyph.get("quantization"):
            result[glyph["glyph"]] = glyph["quantization"]
    return result or None


def _cmd_generate(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.out, seed=args.seed)
    _print_json(
        {
            "status": "ok",
            "output": str(Path(args.out)),
            "glyph_sets": manifest["glyph_sets"],
            "cases": len(manifest["cases"]),
        }
    )
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    from .config import WorkbenchConfig

    config = WorkbenchConfig.from_file(args.config)
    font = load_font(args.font, flatten_tolerance=config.curve_tolerance)
    output_dir = Path(args.out) if args.out else None
    snapshot_path = args.snapshot
    if snapshot_path is None and output_dir is not None:
        snapshot_path = str(output_dir / "snapshot.json")
    cancel_event = threading.Event()
    completed = 0

    def progress(event: dict[str, Any]) -> None:
        nonlocal completed
        if args.verbose:
            print(canonical_json(event), file=sys.stderr)
        if event.get("event") == "tile" and event.get("status") == "computed":
            completed += 1
            if args.stop_after_tiles and completed >= args.stop_after_tiles:
                cancel_event.set()

    try:
        result = run_pipeline(
            font,
            args.text,
            config,
            output_dir=output_dir,
            snapshot_path=snapshot_path,
            resume=args.resume,
            cancel_event=cancel_event,
            previous_summaries=_previous_summaries_from_report(args.compare_report),
            progress=progress,
        )
    except RunCancelled as exc:
        _print_json(
            {
                "status": "cancelled",
                "code": ErrorCode.E_CANCELLED.value,
                "snapshot": exc.snapshot_path,
                "message": "stopped at a tile checkpoint; no exit report was written",
            }
        )
        return 130
    except GlyphForgeError as exc:
        print(canonical_json(exc.to_dict()), file=sys.stderr)
        return _error_exit_code(exc.code)

    summary = result.summary_dict()
    _print_json(
        {
            "status": "ok" if not result.errors else "completed_with_errors",
            "output": str(output_dir) if output_dir else None,
            "snapshot": result.snapshot_path,
            "counts": summary["counts"],
            "errors": summary["errors"],
            "atlas": {
                "width": result.atlas.width,
                "height": result.atlas.height,
                "alpha_hash": result.atlas.alpha_hash,
                "glyphs": [metric.name for metric in result.atlas.metrics],
            },
        }
    )
    if any(error.code == ErrorCode.E_COVERAGE_MISMATCH for error in result.errors):
        return 3
    if any(error.code == ErrorCode.E_BUDGET_EXCEEDED for error in result.errors):
        return 4
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    report = validate_samples(args.samples, args.out)
    _print_json(
        {
            "status": "ok" if report["passed"] else "failed",
            "passed": report["passed"],
            "report": str(Path(args.out) / "validation-report.json"),
            "checks": [
                {"check": check["check"], "passed": check["passed"]}
                for check in report["checks"]
            ],
        }
    )
    return 0 if report["passed"] else 1


def _cmd_ui(args: argparse.Namespace) -> int:
    from .ui import launch_ui

    return launch_ui(
        font_path=args.font,
        config_path=args.config,
        text=args.text,
    )


def _error_exit_code(code: ErrorCode) -> int:
    if code == ErrorCode.E_CANCELLED:
        return 130
    if code == ErrorCode.E_ATLAS_OVERFLOW:
        return 5
    if code == ErrorCode.E_COVERAGE_MISMATCH:
        return 3
    if code == ErrorCode.E_BUDGET_EXCEEDED:
        return 4
    return 2


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="glyphforge",
        description="Deterministic tiled vector glyph rasterizer and atlas workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=20261121)
    generate.set_defaults(func=_cmd_generate)

    run = subparsers.add_parser("run", help="rasterize text and build an atlas")
    run.add_argument("--font", required=True)
    run.add_argument("--text", default="AOB")
    run.add_argument("--config", required=True)
    run.add_argument("--out")
    run.add_argument("--snapshot")
    run.add_argument("--resume", action="store_true")
    run.add_argument("--compare-report", help="previous report for quantization delta")
    run.add_argument(
        "--stop-after-tiles",
        type=int,
        default=0,
        help="diagnostic cooperative stop after N committed tiles",
    )
    run.add_argument("--verbose", action="store_true")
    run.set_defaults(func=_cmd_run)

    validate = subparsers.add_parser("validate", help="run acceptance checks")
    validate.add_argument("--samples", default="samples")
    validate.add_argument("--out", default="artifacts/validation")
    validate.set_defaults(func=_cmd_validate)

    ui = subparsers.add_parser("ui", help="launch the PySide6 workbench")
    ui.add_argument("--font", default="samples/fonts/mixed.json")
    ui.add_argument("--config", default="samples/configs/mixed-24-nonzero-q255.json")
    ui.add_argument("--text", default="AOB\u00c1")
    ui.set_defaults(func=_cmd_ui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except GlyphForgeError as exc:
        print(canonical_json(exc.to_dict()), file=sys.stderr)
        return _error_exit_code(exc.code)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(
            canonical_json(
                {
                    "code": "E_CONFIG_INVALID",
                    "message": str(exc),
                    "details": {},
                }
            ),
            file=sys.stderr,
        )
        return 2
