"""Command-line entry points for generation, rasterization, and verification."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .atlas import verify_output_hashes
from .config import load_config
from .environment import environment_report
from .errors import E_CONFIG_INVALID, GlyphAtlasError, fail
from .generator import generate_samples
from .pipeline import run_pipeline
from .snapshot import canonical_json_bytes


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="glyph-atlas",
        description="Deterministic local glyph rasterizer and atlas workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples and baselines")
    generate.add_argument("--output", default="samples/generated", help="sample output root")
    generate.add_argument("--seed", type=int, default=1947, help="deterministic sample seed")

    run = subparsers.add_parser("run", help="rasterize and package a local configuration")
    run.add_argument("--config", required=True, help="local configuration JSON")
    run.add_argument("--resume", action="store_true", help="resume only from matching disk snapshots")
    run.add_argument("--json", action="store_true", help="emit the complete report as JSON")

    verify = subparsers.add_parser("verify", help="verify atlas, metrics, and manifest hashes")
    verify.add_argument("--config", required=True, help="local configuration JSON")

    gui = subparsers.add_parser("gui", help="launch the Tk workbench")
    gui.add_argument("--config", default="samples/generated/mixed_nonzero/config.json")

    doctor = subparsers.add_parser("doctor", help="verify Python, NumPy, Tkinter, and temp writability")
    doctor.add_argument("--temp", default=None, help="temporary directory to probe")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate":
            summary = generate_samples(args.output, args.seed)
            _print_json(summary)
            return 0
        if args.command == "run":
            report = run_pipeline(args.config, resume=args.resume)
            summary = _pipeline_summary(report)
            _print_json(summary if not args.json else report.baseline)
            return 0
        if args.command == "verify":
            config = load_config(args.config)
            results: list[dict[str, Any]] = []
            for target in config.targets:
                manifest = verify_output_hashes(config.output_dir / target.name)
                results.append({"target": target.name, "manifest": manifest})
            _print_json({"ok": True, "targets": results})
            return 0
        if args.command == "gui":
            from .gui import launch_gui

            launch_gui(args.config)
            return 0
        if args.command == "doctor":
            report = environment_report(args.temp)
            _print_json(report)
            return 0 if report["ok"] else 1
    except GlyphAtlasError as exc:
        print(json.dumps(exc.as_dict(), sort_keys=True, ensure_ascii=False), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(json.dumps({"ok": False, "code": "E_CANCELLED", "message": "Interrupted"}, sort_keys=True), file=sys.stderr)
        return 130
    fail(E_CONFIG_INVALID, "Unknown command", command=args.command)
    raise AssertionError("unreachable")


def _pipeline_summary(report: Any) -> dict[str, Any]:
    return {
        "ok": True,
        "font": report.font.name,
        "font_sha256": report.font.canonical_hash(),
        "baseline": str(report.baseline_path),
        "targets": [
            {
                "target": run.schedule.target.name,
                "parallelism": run.schedule.parallelism,
                "glyphs": [
                    {
                        "glyph": glyph.glyph_name,
                        "coverage_sha256": glyph.coverage_hash,
                        "snapshot_head": glyph.tiles[-1].chain_hash if glyph.tiles else None,
                        "resumed": glyph.resumed,
                    }
                    for glyph in run.schedule.glyphs
                ],
                "atlas_sha256": run.atlas.atlas_sha256,
                "metrics_sha256": run.atlas.metrics_sha256,
                "source_coverage_sha256": run.atlas.source_coverage_sha256,
                "trace_sha256": run.trace_sha256,
            }
            for run in report.targets
        ],
    }


def _print_json(value: Any) -> None:
    sys.stdout.write(canonical_json_bytes(value).decode("utf-8") + "\n")


if __name__ == "__main__":
    raise SystemExit(main())

