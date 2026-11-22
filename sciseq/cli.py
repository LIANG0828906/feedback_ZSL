"""Command-line entry points for generation, processing, and acceptance."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .acceptance import run_acceptance
from .config import (
    BudgetConfig,
    DenoiseConfig,
    PipelineConfig,
    RegistrationConfig,
)
from .engine import SequenceProcessor
from .errors import SciSeqError
from .export import export_with_recomputation, export_result
from .generator import DEFAULT_GROUPS, generate_all
from .loader import load_sequence


def _add_pipeline_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--window", choices=("none", "hann", "hamming", "blackman"), default="none")
    parser.add_argument("--upsample", type=int, default=20)
    parser.add_argument("--max-shift", type=float, default=8.0)
    parser.add_argument("--filter", choices=("gaussian", "median", "guided"), default="gaussian")
    parser.add_argument("--block-size", type=int, default=32)
    parser.add_argument("--strength", type=float, default=1.0)
    parser.add_argument("--median-radius", type=int, default=1)
    parser.add_argument("--guided-radius", type=int, default=3)
    parser.add_argument("--reference-frame", type=int, default=0)
    parser.add_argument("--time-budget", type=float, default=600.0)
    parser.add_argument("--memory-budget", type=float, default=2048.0)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--frames-per-worker", type=int, default=2)
    parser.add_argument("--preview-denoise-fraction", type=float, default=0.5)
    parser.add_argument("--preview-mean-abs-limit", type=float, default=35.0)


def _config_from_args(args: argparse.Namespace) -> PipelineConfig:
    return PipelineConfig(
        reference_frame=args.reference_frame,
        registration=RegistrationConfig(
            window=args.window,
            upsample_factor=args.upsample,
            max_shift=args.max_shift,
        ),
        denoise=DenoiseConfig(
            filter=args.filter,
            block_size=args.block_size,
            strength=args.strength,
            median_radius=args.median_radius,
            guided_radius=args.guided_radius,
        ),
        budget=BudgetConfig(
            time_budget_s=args.time_budget,
            memory_budget_mb=args.memory_budget,
            max_workers=args.workers,
            frames_per_worker=args.frames_per_worker,
            preview_denoise_fraction=args.preview_denoise_fraction,
            preview_mean_abs_limit=args.preview_mean_abs_limit,
        ),
    )


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="sciseq")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic sample sequences")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--overwrite", action="store_true")
    generate.add_argument(
        "--groups",
        nargs="*",
        choices=[group.name for group in DEFAULT_GROUPS],
        default=[group.name for group in DEFAULT_GROUPS],
    )

    inspect = subparsers.add_parser("inspect", help="inspect sequence metadata")
    inspect.add_argument("--sequence", required=True)

    process = subparsers.add_parser("process", help="process one sequence")
    process.add_argument("--sequence", required=True)
    process.add_argument(
        "--mode",
        choices=("full", "incremental", "resume", "preview", "auto"),
        default="full",
    )
    process.add_argument("--checkpoint")
    process.add_argument("--export")
    process.add_argument("--pause-after", type=int)
    process.add_argument("--reset", action="store_true")
    _add_pipeline_arguments(process)

    accept = subparsers.add_parser("acceptance", help="run local end-to-end acceptance")
    accept.add_argument("--samples", default="samples")
    accept.add_argument("--output", default="artifacts/acceptance.json")
    accept.add_argument("--groups", nargs="*", choices=[group.name for group in DEFAULT_GROUPS])
    accept.add_argument("--regenerate", action="store_true")

    gui = subparsers.add_parser("gui", help="launch the PySide6 desktop example")
    gui.add_argument("--sequence", default="samples/small")
    gui.add_argument("--checkpoint", default="artifacts/checkpoints/gui")
    return parser


def _print_error(exc: SciSeqError) -> int:
    print(str(exc), file=sys.stderr)
    return 2


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            selected = [group for group in DEFAULT_GROUPS if group.name in args.groups]
            paths = generate_all(args.out, groups=selected, overwrite=args.overwrite)
            print(json.dumps({"generated": [str(path) for path in paths]}, indent=2))
            return 0
        if args.command == "inspect":
            sequence = load_sequence(args.sequence)
            print(
                json.dumps(
                    {
                        "sequence_id": sequence.sequence_id,
                        "shape": list(sequence.shape),
                        "frame_count": sequence.frame_count,
                        "reference_frame": sequence.reference_frame,
                        "truth_shift_min": sequence.truth.shifts.min(axis=0).tolist(),
                        "truth_shift_max": sequence.truth.shifts.max(axis=0).tolist(),
                        "features": [feature.feature_id for feature in sequence.truth.features],
                        "fingerprint": sequence.fingerprint(),
                    },
                    indent=2,
                )
            )
            return 0
        if args.command == "process":
            sequence = load_sequence(args.sequence)
            config = _config_from_args(args)
            checkpoint = Path(args.checkpoint) if args.checkpoint else Path("artifacts/checkpoints") / sequence.sequence_id
            processor = SequenceProcessor(sequence, config, checkpoint_dir=checkpoint)
            if args.mode == "full":
                result = processor.run_full(parallel=True)
                outcome = None
            elif args.mode == "auto":
                decision = processor.plan()
                if decision.mode == "preview":
                    result = processor.run_preview()
                else:
                    result = processor.run_full(parallel=True)
                outcome = None
            elif args.mode == "preview":
                result = processor.run_preview()
                outcome = None
            else:
                outcome = processor.run_incremental(
                    resume=args.mode == "resume",
                    parallel=True,
                    pause_after=args.pause_after,
                    reset=args.reset,
                )
                result = outcome.result
            summary = {
                "mode": None if outcome is not None and result is None else result.mode,
                "complete": True if result is not None else False,
                "committed": outcome.committed_count if outcome is not None else sequence.frame_count,
                "total": sequence.frame_count,
            }
            if result is not None:
                summary["result"] = result.summarize()
                if args.export:
                    if result.preview:
                        summary["export"] = export_with_recomputation(
                            processor,
                            result,
                            args.export,
                        )
                    else:
                        summary["export"] = export_result(
                            result,
                            args.export,
                            config=config,
                        )
            if outcome is not None:
                summary["snapshot_reason"] = outcome.snapshot_reason
                summary["invalidated"] = outcome.invalidated
            print(json.dumps(summary, indent=2, allow_nan=False))
            return 0
        if args.command == "acceptance":
            report = run_acceptance(
                args.samples,
                args.output,
                groups=args.groups,
                regenerate=args.regenerate,
            )
            print(
                json.dumps(
                    {
                        "status": report["status"],
                        "groups": list(report["groups"]),
                        "output": str(Path(args.output).resolve()),
                        "elapsed_seconds": report["elapsed_seconds"],
                    },
                    indent=2,
                )
            )
            return 0
        if args.command == "gui":
            from .gui.app import main as gui_main

            return gui_main(
                [
                    "--sequence",
                    args.sequence,
                    "--checkpoint",
                    args.checkpoint,
                ]
            )
    except SciSeqError as exc:
        return _print_error(exc)
    except BrokenPipeError:
        return 0
    except (OSError, ValueError) as exc:
        print(f"[E_DECODE] {exc}", file=sys.stderr)
        return 2
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

