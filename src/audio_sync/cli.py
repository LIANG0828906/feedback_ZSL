from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .config import AlignmentConfig
from .engine import AlignmentEngine, run_three_paths
from .errors import SyncError
from .export import export_alignment
from .generator import SyntheticSessionConfig, generate_demo_suite, generate_session
from .manifest import load_session, validate_session


def _configuration(args: argparse.Namespace) -> AlignmentConfig:
    return AlignmentConfig(
        interpolation_order=args.order,
        window_half_width=args.window_half_width,
        segment_length=args.segment_length,
        tolerance_samples=args.tolerance,
        max_workers=args.workers,
        max_buffer_bytes=args.buffer_mib * 1024 * 1024,
        preserve_committed=not args.ignore_committed,
    )


def _print_json(payload: Any, *, stream: Any = sys.stdout) -> None:
    print(json.dumps(payload, indent=2, sort_keys=True), file=stream)


def _summary(result: Any) -> dict[str, Any]:
    return {
        "session_id": result.session_id,
        "path": result.path,
        "complete": result.complete,
        "sample_rate": result.sample_rate,
        "channels": {
            channel: {
                "initial_offset_error_samples": report["initial_offset_error_samples"],
                "max_abs_rate_error_ppm": float(
                    max((abs(value) for value in report["rates_ppm_error"]), default=0.0)
                ),
                "max_abs_residual_samples": report["max_abs_residual_samples"],
                "max_residual_bound_samples": float(
                    max(report["marker_residual_bounds_samples"], default=0.0)
                ),
                "low_confidence": report["low_confidence"],
            }
            for channel, report in result.reports.items()
        },
        "low_confidence_channels": result.low_confidence_channels,
        "diagnostics": result.diagnostics,
    }


def _add_dsp_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--order", type=int, default=7, help="interpolation order (1..15)")
    parser.add_argument("--window-half-width", type=int, default=None, help="interpolation window half-width")
    parser.add_argument("--segment-length", type=int, default=64_000)
    parser.add_argument("--tolerance", type=float, default=0.25, help="truth validation tolerance in samples")
    parser.add_argument("--workers", type=int, default=2, help="maximum worker threads")
    parser.add_argument("--buffer-mib", type=int, default=4, help="per-thread interpolation buffer limit")
    parser.add_argument("--ignore-committed", action="store_true", help="do not reuse compatible committed rows")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="audio-sync", description="Interactive audio clock synchronization")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic multichannel sessions")
    generate.add_argument("--out", type=Path, default=Path("artifacts/demo_session"))
    generate.add_argument("--suite", action="store_true", help="generate three sessions below --out")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument("--duration", type=float, default=8.0)
    generate.add_argument("--session-id", default="session-clean-001")

    validate = subparsers.add_parser("validate", help="validate manifest, fingerprints and channel samples")
    validate.add_argument("--session", type=Path, required=True)

    run = subparsers.add_parser("run", help="run one alignment path and export")
    run.add_argument("--session", type=Path, required=True)
    run.add_argument("--path", choices=("full", "realtime", "resume"), default="full")
    run.add_argument("--checkpoint", type=Path, default=None)
    run.add_argument("--out", type=Path, default=Path("artifacts/runs/default"))
    run.add_argument("--interrupt-after", type=int, default=None)
    run.add_argument("--allow-truth-miss", action="store_true")
    _add_dsp_options(run)

    verify = subparsers.add_parser("verify", help="compare realtime, resume and full recomputation")
    verify.add_argument("--session", type=Path, required=True)
    verify.add_argument("--checkpoint-root", type=Path, default=Path("artifacts/runs/three-path"))
    verify.add_argument("--out", type=Path, default=Path("artifacts/runs/verified"))
    _add_dsp_options(verify)

    gui = subparsers.add_parser("gui", help="open the PySide6 desktop workbench")
    gui.add_argument("--session", type=Path, default=Path("artifacts/demo_session"))

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate":
            if args.suite:
                results = generate_demo_suite(args.out)
            else:
                results = generate_session(
                    args.out,
                    config=SyntheticSessionConfig(
                        session_id=args.session_id,
                        seed=args.seed,
                        duration_seconds=args.duration,
                    ),
                )
            _print_json(results)
            return 0

        if args.command == "validate":
            _print_json(validate_session(args.session))
            return 0

        if args.command == "gui":
            from .gui import run_gui

            return run_gui(args.session)

        config = _configuration(args)
        config.validate()
        session = load_session(args.session)

        if args.command == "run":
            checkpoint = args.checkpoint
            if args.path != "full" and checkpoint is None:
                checkpoint = Path("artifacts") / "runs" / session.manifest.session_id / "checkpoints.sqlite"
            engine = AlignmentEngine(session, config, checkpoint_path=checkpoint)
            result = engine.run(
                args.path,
                interrupt_after=args.interrupt_after,
                enforce_truth_tolerance=not args.allow_truth_miss,
            )
            exported = export_alignment(result, args.out) if result.complete else None
            _print_json({"result": _summary(result), "export": exported})
            return 0

        if args.command == "verify":
            verification = run_three_paths(session, config, checkpoint_root=args.checkpoint_root)
            exported = export_alignment(verification.full, args.out)
            payload = {
                "ok": True,
                "comparisons": verification.comparisons,
                "full": _summary(verification.full),
                "export": exported,
            }
            args.out.mkdir(parents=True, exist_ok=True)
            verification_path = args.out / "three_path_verification.json"
            verification_path.write_text(
                json.dumps(payload, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            payload["verification_report"] = str(verification_path)
            _print_json(payload)
            return 0
    except SyncError as exc:
        _print_json(exc.to_dict(), stream=sys.stderr)
        return 2
    except Exception as exc:
        _print_json({"code": "E_INTERNAL", "message": str(exc)}, stream=sys.stderr)
        return 3
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

