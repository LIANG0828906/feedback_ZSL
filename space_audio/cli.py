from __future__ import annotations

import argparse
import dataclasses
import json
import shutil
import sys
from pathlib import Path
from typing import Any, Sequence

from .config import AnalysisConfig, RenderConfig, WorkbenchConfig
from .errors import WorkbenchError
from .export import export_bundle
from .faults import FaultKind, inject_fault
from .generator import generate_dataset, generate_demo
from .loader import load_analysis_profile, load_sample
from .pipeline import (
    RunResult,
    compare_results,
    run_full_recompute,
    run_realtime,
    run_resume,
)


def _resolve_analysis_config(
    args: argparse.Namespace,
    profile: dict[str, Any] | None,
) -> AnalysisConfig:
    values = AnalysisConfig().to_dict()
    if profile is not None:
        for key in values:
            if key in profile:
                values[key] = profile[key]
    for key in ("frame_len", "hop", "window_len"):
        cli_value = getattr(args, key, None)
        if cli_value is not None:
            values[key] = cli_value
    return AnalysisConfig(**values)


def _workbench_config(analysis: AnalysisConfig) -> WorkbenchConfig:
    return WorkbenchConfig(
        analysis=analysis,
        render=RenderConfig(interpolation_order=analysis.interpolation_order),
        max_workers=2,
    )


def _clear_dir(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _fault_record(
    sample,
    config: WorkbenchConfig,
    workdir: Path,
    kind: FaultKind,
    expected: str,
) -> dict[str, Any]:
    faulted = inject_fault(
        sample,
        kind,
        channel="R",
        frame_index=2,
        config=config,
    )
    entry: dict[str, Any] = {"injected": kind.value, "expected_code": expected}
    try:
        result = run_realtime(
            faulted,
            config,
            workdir / f"fault_{kind.value}.sqlite",
            fresh=True,
        )
        error = result.frame_errors[0] if result.frame_errors else None
        entry.update(
            {
                "code": None if error is None else error.code.value,
                "channel": None if error is None else error.channel,
                "frame_index": None if error is None else error.frame_index,
                "sample_index": None if error is None else error.sample_index,
                "committed_frames": result.committed_frames,
                "report_complete": result.report_complete,
                "passed": error is not None and error.code.value == expected,
            }
        )
        if error is not None:
            entry["error"] = error.to_dict()
    except WorkbenchError as exc:
        entry.update(
            {
                "code": exc.code.value,
                "channel": exc.channel,
                "frame_index": exc.frame_index,
                "sample_index": exc.sample_index,
                "committed_frames": 0,
                "report_complete": False,
                "passed": exc.code.value == expected,
                "error": exc.to_dict(),
            }
        )
    return entry


def command_generate(args: argparse.Namespace) -> int:
    if args.count > 1:
        generated_dataset = generate_dataset(
            args.workdir,
            seed=args.seed,
            count=args.count,
            frame_len=args.frame_len,
            hop=args.hop,
            window_len=args.window_len,
            frame_count=args.frames,
        )
        print(generated_dataset.manifest_path)
    else:
        generated = generate_demo(
            args.workdir,
            seed=args.seed,
            frame_len=args.frame_len,
            hop=args.hop,
            window_len=args.window_len,
            frame_count=args.frames,
        )
        print(generated.manifest_path)
    return 0


def command_run(args: argparse.Namespace) -> int:
    sample = load_sample(args.manifest, sample_id=args.sample_id)
    profile = load_analysis_profile(args.manifest)
    config = _workbench_config(_resolve_analysis_config(args, profile))
    checkpoint = Path(args.checkpoint)
    if args.mode == "realtime":
        result = run_realtime(
            sample,
            config,
            checkpoint,
            stop_after_commits=args.stop_after,
            fresh=args.fresh,
        )
    elif args.mode == "resume":
        result = run_resume(
            sample,
            config,
            checkpoint,
            stop_after_commits=args.stop_after,
        )
    else:
        result = run_full_recompute(sample, checkpoint, config)
    report = result.to_report(sample)
    if args.report:
        Path(args.report).write_text(
            json.dumps(report, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
    print(
        json.dumps(
            {
                "mode": result.mode,
                "completed": result.completed,
                "report_complete": result.report_complete,
                "committed_frames": result.committed_frames,
                "total_frames": result.total_frames,
                "errors": [error.to_dict() for error in result.frame_errors],
                "output_shape": (
                    None if result.output is None else list(result.output.shape)
                ),
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if result.report_complete else 2


def command_verify(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir)
    _clear_dir(workdir)
    demo_dir = workdir / "sample"
    generate_analysis = _resolve_analysis_config(args, None)
    generated = generate_demo(
        demo_dir,
        seed=args.seed,
        frame_len=generate_analysis.frame_len,
        hop=generate_analysis.hop,
        window_len=generate_analysis.window_len,
        frame_count=args.frames,
    )
    sample = load_sample(generated.manifest_path, sample_id=generated.sample_id)
    profile = load_analysis_profile(generated.manifest_path)
    config = _workbench_config(_resolve_analysis_config(args, profile))
    checkpoint_a = workdir / "realtime.sqlite"
    checkpoint_b = workdir / "resume.sqlite"

    realtime = run_realtime(sample, config, checkpoint_a, fresh=True)
    partial = run_realtime(
        sample,
        config,
        checkpoint_b,
        stop_after_commits=max(1, args.frames // 3),
        fresh=True,
    )
    resumed = run_resume(sample, config, checkpoint_b)
    recomputed = run_full_recompute(sample, checkpoint_b, config)
    comparison_ab = compare_results(realtime, resumed)
    comparison_ac = compare_results(realtime, recomputed)

    output_length_ok = (
        realtime.output is not None
        and realtime.output.shape[1] == sample.dry_length
    )
    frame_errors_ok = all(
        not frame.frame_errors for frame in (realtime, resumed, recomputed)
    )
    bounds_ok = all(
        channel.observed_errors.peak_quantization_s
        <= frame.bounds.peak_quantization_s + 1.0e-12
        and channel.observed_errors.energy_truncation_relative
        <= frame.bounds.energy_truncation_relative + 1.0e-12
        and channel.observed_errors.tdoa_s <= frame.bounds.tdoa_s + 1.0e-12
        and channel.observed_errors.convolution_interpolation_relative
        <= frame.bounds.convolution_interpolation_relative + 1.0e-12
        for frame in realtime.frames
        for channel in frame.channels
    )

    changed_dry = sample.dry.copy()
    changed_dry[0, 0] += 0.0001
    changed_sample = dataclasses.replace(sample, dry=changed_dry)
    invalidated = run_resume(
        changed_sample,
        config,
        checkpoint_b,
    )
    invalidation_ok = (
        invalidated.invalidated
        and invalidated.completed
        and invalidated.committed_frames == invalidated.total_frames
    )

    faults = [
        _fault_record(
            sample,
            config,
            workdir,
            FaultKind.CHANNEL_MISSING,
            "E_CHAN",
        ),
        _fault_record(
            sample,
            config,
            workdir,
            FaultKind.TIMING_INVERSION,
            "E_ORDER",
        ),
        _fault_record(
            sample,
            config,
            workdir,
            FaultKind.GAIN_OVERFLOW,
            "E_CLIP",
        ),
        _fault_record(
            sample,
            config,
            workdir,
            FaultKind.BAD_FRAME,
            "E_FRAME",
        ),
        _fault_record(
            sample,
            config,
            workdir,
            FaultKind.BOUND_VIOLATION,
            "E_BOUND",
        ),
    ]

    divergent_output = realtime.output.copy()
    divergent_output[0, 0] += 1.0e-9
    divergent_render = dataclasses.replace(
        realtime.render,
        output=divergent_output,
    )
    divergent = dataclasses.replace(realtime, render=divergent_render)
    try:
        compare_results(realtime, divergent)
        divergence_record: dict[str, Any] = {"passed": False}
    except WorkbenchError as exc:
        divergence_record = {
            "passed": exc.code.value == "E_DIVERGE",
            "code": exc.code.value,
            "frame_index": exc.frame_index,
            "sample_index": exc.sample_index,
            "details": exc.details,
        }

    exports = export_bundle(
        realtime,
        workdir / "exports",
        sample_rate=sample.sample_rate,
        sample=sample,
        prefix=sample.sample_id,
    )
    passed = (
        realtime.completed
        and resumed.completed
        and recomputed.completed
        and comparison_ab.identical
        and comparison_ac.identical
        and output_length_ok
        and frame_errors_ok
        and bounds_ok
        and invalidation_ok
        and all(record["passed"] for record in faults)
        and divergence_record["passed"]
    )
    report = {
        "schema": "spatial-audio-verification-v1",
        "passed": passed,
        "seed": args.seed,
        "sample": {
            "sample_id": sample.sample_id,
            "sample_rate": sample.sample_rate,
            "layout": sample.layout.to_dict(),
            "ir_shape": list(sample.ir.shape),
            "dry_shape": list(sample.dry.shape),
            "input_hash": sample.input_hash(),
        },
        "config": config.to_dict(),
        "three_paths": {
            "realtime": _result_summary(realtime),
            "partial_before_resume": _result_summary(partial),
            "resumed": _result_summary(resumed),
            "full_recompute": _result_summary(recomputed),
            "realtime_vs_resumed": dataclasses.asdict(comparison_ab),
            "realtime_vs_full_recompute": dataclasses.asdict(comparison_ac),
            "output_length_matches_dry": output_length_ok,
            "all_frame_bounds_pass": bounds_ok,
        },
        "checkpoint_invalidation": {
            "passed": invalidation_ok,
            "invalidated": invalidated.invalidated,
            "committed_frames": invalidated.committed_frames,
            "total_frames": invalidated.total_frames,
        },
        "fault_injection": faults,
        "divergence_probe": divergence_record,
        "exports": exports,
    }
    report_path = workdir / "verification.json"
    report_path.write_text(
        json.dumps(report, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if passed else 1


def _result_summary(result: RunResult) -> dict[str, Any]:
    return {
        "mode": result.mode,
        "completed": result.completed,
        "report_complete": result.report_complete,
        "committed_frames": result.committed_frames,
        "total_frames": result.total_frames,
        "commit_sequence": list(result.commit_sequence),
        "output_shape": None if result.output is None else list(result.output.shape),
        "errors": [error.to_dict() for error in result.frame_errors],
    }


def command_gui(args: argparse.Namespace) -> int:
    del args
    from .gui import main as gui_main

    return gui_main([sys.argv[0]])


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="spatial-audio",
        description="Deterministic spatial acoustics workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate")
    generate_parser.add_argument("--workdir", default="artifacts/demo")
    generate_parser.add_argument("--seed", type=int, default=20261123)
    generate_parser.add_argument("--frames", type=int, default=12)
    generate_parser.add_argument("--count", type=int, default=1)
    generate_parser.add_argument("--frame-len", type=int, default=2048)
    generate_parser.add_argument("--hop", type=int, default=1024)
    generate_parser.add_argument("--window-len", type=int, default=768)
    generate_parser.set_defaults(func=command_generate)

    run_parser = subparsers.add_parser("run")
    run_parser.add_argument("--manifest", required=True)
    run_parser.add_argument("--sample-id", default=None)
    run_parser.add_argument("--checkpoint", required=True)
    run_parser.add_argument(
        "--mode",
        choices=("realtime", "resume", "full"),
        default="realtime",
    )
    run_parser.add_argument("--stop-after", type=int, default=None)
    run_parser.add_argument("--fresh", action="store_true")
    run_parser.add_argument("--report", default=None)
    run_parser.add_argument("--frame-len", type=int, default=None)
    run_parser.add_argument("--hop", type=int, default=None)
    run_parser.add_argument("--window-len", type=int, default=None)
    run_parser.set_defaults(func=command_run)

    verify_parser = subparsers.add_parser("verify")
    verify_parser.add_argument("--workdir", default="artifacts/verification")
    verify_parser.add_argument("--seed", type=int, default=20261123)
    verify_parser.add_argument("--frames", type=int, default=8)
    verify_parser.add_argument("--frame-len", type=int, default=None)
    verify_parser.add_argument("--hop", type=int, default=None)
    verify_parser.add_argument("--window-len", type=int, default=None)
    verify_parser.set_defaults(func=command_verify)

    gui_parser = subparsers.add_parser("gui")
    gui_parser.set_defaults(func=command_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
