"""Command-line entry points for the spatial acoustic workbench."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .analysis import analyze_sample
from .audio import export_render
from .errors import ErrorCode, WorkbenchError
from .io import load_sample
from .models import AnalysisConfig
from .pipeline import PipelineOutcome, ReconstructionPipeline
from .samplegen import generate_sample_bundle
from .verification import run_full_verification


def _load_channel_weights(value: str | None) -> dict[str, dict[str, float]] | None:
    if value is None:
        return None
    try:
        raw = Path(value).read_text(encoding="utf-8") if Path(value).is_file() else value
        parsed = json.loads(raw)
    except (OSError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "channel map JSON is invalid",
            stage="config",
        ) from exc
    if not isinstance(parsed, dict):
        raise WorkbenchError(
            ErrorCode.E_PARA,
            "channel map must be a JSON object",
            stage="config",
        )
    result: dict[str, dict[str, float]] = {}
    for output, mapping in parsed.items():
        if not isinstance(mapping, dict):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "each output channel map must be an object",
                stage="config",
            )
        result[str(output)] = {str(key): float(item) for key, item in mapping.items()}
    return result


def _config_from_args(args: argparse.Namespace) -> AnalysisConfig:
    return AnalysisConfig(
        frame_length=args.frame_length,
        hop_length=args.hop_length,
        energy_threshold=args.energy_threshold,
        early_window_seconds=args.early_window_seconds,
        max_direct_delay_seconds=args.max_direct_delay_seconds,
        max_reflections=args.max_reflections,
        clip_limit=args.clip_limit,
        input_peak_limit=args.input_peak_limit,
        max_workers=args.max_workers,
        max_buffer_bytes=args.max_buffer_bytes,
        output_layout=args.output_layout,
        channel_weights=_load_channel_weights(args.channel_map),
        algorithm_version=args.algorithm_version,
    )


def _print(value: dict[str, Any]) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _outcome_summary(outcome: PipelineOutcome) -> dict[str, Any]:
    return outcome.to_dict(include_frames=False)


def _add_config_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--frame-length", type=int, default=1024)
    parser.add_argument("--hop-length", type=int, default=512)
    parser.add_argument("--energy-threshold", type=float, default=0.005)
    parser.add_argument("--early-window-seconds", type=float, default=0.03)
    parser.add_argument("--max-direct-delay-seconds", type=float, default=0.25)
    parser.add_argument("--max-reflections", type=int, default=8)
    parser.add_argument("--clip-limit", type=float, default=1.0)
    parser.add_argument("--input-peak-limit", type=float, default=4.0)
    parser.add_argument("--max-workers", type=int, default=4)
    parser.add_argument("--max-buffer-bytes", type=int, default=16 * 1024 * 1024)
    parser.add_argument("--output-layout", default=None)
    parser.add_argument("--channel-map", default=None)
    parser.add_argument("--algorithm-version", default="spatial-recon-v1")


def _cmd_generate(args: argparse.Namespace) -> int:
    bundle = generate_sample_bundle(
        args.out,
        seed=args.seed,
        sample_rate=args.sample_rate,
        length=args.length,
        layout_name=args.layout,
        overwrite=not args.no_overwrite,
    )
    _print({"ok": True, "seed": args.seed, "files": bundle.as_dict()})
    return 0


def _cmd_analyze(args: argparse.Namespace) -> int:
    sample = load_sample(args.sample)
    config = _config_from_args(args)
    context, frames, truth = analyze_sample(sample, config)
    payload = {
        "ok": True,
        "sample": {
            "path": str(Path(args.sample)),
            "sample_rate": sample.sample_rate,
            "channels": list(sample.layout.channels),
            "ir_samples": int(sample.ir.shape[1]),
            "dry_samples": int(sample.dry.shape[0]),
            "input_hash": sample.content_hash,
        },
        "config": config.to_dict(),
        "global_direct_delays_samples": list(context.global_direct_delays),
        "truth_report": truth,
        "frames": [
            {
                "frame_index": frame.frame_index,
                "parameters": frame.to_dict(),
                "energy_statistics": frame.energy_statistics(),
            }
            for frame in frames
        ],
    }
    if args.output:
        Path(args.output).parent.mkdir(parents=True, exist_ok=True)
        Path(args.output).write_text(
            json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    _print(payload if args.output is None else {"ok": True, "output": args.output})
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    sample = load_sample(args.sample)
    config = _config_from_args(args)
    pipeline = ReconstructionPipeline(sample, config, args.checkpoint)
    if args.mode == "realtime":
        outcome = pipeline.run_realtime(stop_after=args.stop_after)
    elif args.mode == "recovery":
        outcome = pipeline.run_recovery(stop_after=args.stop_after)
    else:
        outcome = pipeline.run_replay()
    payload = _outcome_summary(outcome)
    if outcome.result is not None and args.out:
        payload["exports"] = export_render(
            outcome.result,
            args.out,
            basename=args.basename,
        )
    _print(payload)
    return 0 if outcome.complete else 2


def _cmd_verify(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    bundle = None
    if args.sample_dir:
        bundle = generate_sample_bundle(
            args.sample_dir,
            seed=args.seed,
            overwrite=True,
        )
    report = run_full_verification(
        args.sample,
        args.work_dir,
        config=config,
        bundle=bundle,
        recover_after=args.recover_after,
    )
    _print(report)
    return 0 if report.get("ok") else 2


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(
        sample_path=args.sample,
        checkpoint_path=args.checkpoint,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="spatial-workbench",
        description="Deterministic spatial acoustic reconstruction workbench",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", default="data/samples")
    generate.add_argument("--seed", type=int, default=42_042)
    generate.add_argument("--sample-rate", type=int, default=16_000)
    generate.add_argument("--length", type=int, default=32_768)
    generate.add_argument("--layout", default="quad")
    generate.add_argument("--no-overwrite", action="store_true")
    generate.set_defaults(func=_cmd_generate)

    analyze = sub.add_parser("analyze", help="analyze a sample frame by frame")
    analyze.add_argument("--sample", required=True)
    analyze.add_argument("--output")
    _add_config_arguments(analyze)
    analyze.set_defaults(func=_cmd_analyze)

    run = sub.add_parser("run", help="run realtime, recovery or replay path")
    run.add_argument("--sample", required=True)
    run.add_argument("--checkpoint", required=True)
    run.add_argument("--mode", choices=("realtime", "recovery", "replay"), default="realtime")
    run.add_argument("--stop-after", type=int)
    run.add_argument("--out")
    run.add_argument("--basename", default="spatial_render")
    _add_config_arguments(run)
    run.set_defaults(func=_cmd_run)

    verify = sub.add_parser("verify", help="run the full acceptance verification")
    verify.add_argument("--sample", default="data/samples/spatial_normal.npz")
    verify.add_argument("--sample-dir")
    verify.add_argument("--work-dir", default="artifacts/verification")
    verify.add_argument("--seed", type=int, default=42_042)
    verify.add_argument("--recover-after", type=int)
    _add_config_arguments(verify)
    verify.set_defaults(func=_cmd_verify)

    gui = sub.add_parser("gui", help="launch the desktop visualization panel")
    gui.add_argument("--sample", default="data/samples/spatial_normal.npz")
    gui.add_argument("--checkpoint", default="artifacts/gui/checkpoint.json")
    gui.set_defaults(func=_cmd_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _print(exc.to_dict())
        return 2
    except Exception as exc:
        wrapped = WorkbenchError.wrap(
            ErrorCode.E_IO,
            "unhandled workbench failure",
            exc,
            stage="cli",
        )
        _print(wrapped.to_dict())
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

