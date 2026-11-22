"""Command-line entry points for generation, validation and experiments."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .config import Budget, QualityTolerance, ScaleTolerance, SegmentationParams
from .errors import BusinessError
from .experiments import run_scale_experiment, write_experiment_report
from .pipeline import SegmentationPipeline, export_artifacts
from .samplegen import generate_suite
from .validation import load_sample, load_suite


def _workspace_root(value: str | None) -> Path:
    if value:
        return Path(value).resolve()
    return (Path.cwd() / "workspace_data").resolve()


def _params_from_args(args: argparse.Namespace, sample: Any) -> SegmentationParams:
    params = SegmentationParams().for_sample(
        sample.recommended_params, sample.region_count
    )
    changes: dict[str, Any] = {}
    for name in (
        "color_tolerance",
        "gradient_threshold",
        "confidence_threshold",
        "min_area",
    ):
        value = getattr(args, name, None)
        if value is not None:
            changes[name] = value
    if getattr(args, "morph_size", None) is not None:
        changes["morph_size"] = args.morph_size
    return SegmentationParams(**{**params.__dict__, **changes})


def _budget_from_args(args: argparse.Namespace) -> Budget:
    return Budget(
        time_seconds=args.time_budget,
        memory_mb=args.memory_budget,
        max_workers=args.workers,
        tile_size=args.tile_size,
        block_buffer_mb=args.block_buffer_mb,
    )


def _select_samples(root: Path, sample_id: str | None) -> list[Any]:
    samples = load_suite(root / "samples")
    if sample_id is None:
        return samples
    selected = [sample for sample in samples if sample.sample_id == sample_id]
    if not selected:
        raise BusinessError(
            "E_DECODE",
            "未找到指定样本",
            details={"sample_id": sample_id},
        )
    return selected


def cmd_generate(args: argparse.Namespace) -> int:
    root = _workspace_root(args.workspace)
    index = generate_suite(root / "samples")
    print(json.dumps(index, ensure_ascii=False, indent=2))
    return 0


def cmd_validate(args: argparse.Namespace) -> int:
    root = _workspace_root(args.workspace)
    samples = load_suite(root / "samples")
    summary = [
        {
            "sample_id": sample.sample_id,
            "resolution": [sample.width, sample.height],
            "channels": sample.channels,
            "regions": sample.region_count,
            "manifest": str(sample.directory / "manifest.json"),
        }
        for sample in samples
    ]
    print(json.dumps({"valid": True, "samples": summary}, ensure_ascii=False, indent=2))
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    root = _workspace_root(args.workspace)
    samples = _select_samples(root, args.sample)
    budget = _budget_from_args(args)
    pipeline = SegmentationPipeline(
        root / "checkpoints",
        quality_tolerance=QualityTolerance(),
    )
    outputs = []
    for sample in samples:
        params = _params_from_args(args, sample)
        result = pipeline.run(
            sample,
            params,
            budget,
            force_mode=args.force_mode,
            force_factor=args.force_factor,
        )
        paths = {}
        if args.export:
            paths = export_artifacts(
                result,
                root / "exports",
                image=sample.image,
                truth_mask=sample.truth_mask,
            )
        outputs.append(
            {
                "sample_id": sample.sample_id,
                "mode": result.mode,
                "factor": result.factor,
                "complete": result.complete,
                "elapsed_seconds": round(result.elapsed_seconds, 4),
                "reused_snapshot": result.reused_snapshot,
                "recovered_stages": result.recovered_stages,
                "checkpoint_dir": result.checkpoint_dir,
                "progress": result.state_snapshot["progress"],
                "comparison": result.comparison.to_dict(),
                "decision": result.decision.to_dict(),
                "exports": paths,
            }
        )
    print(json.dumps({"runs": outputs}, ensure_ascii=False, indent=2))
    return 0


def cmd_experiment(args: argparse.Namespace) -> int:
    root = _workspace_root(args.workspace)
    samples = _select_samples(root, args.sample)
    budget = _budget_from_args(args)
    payload = run_scale_experiment(
        samples,
        SegmentationParams(),
        budget,
        scale_tolerance=ScaleTolerance(),
        quality_tolerance=QualityTolerance(),
    )
    report_path = write_experiment_report(
        payload, root / "exports" / "scale_experiment.json"
    )
    print(
        json.dumps(
            {
                "report": str(report_path),
                "all_within_tolerance": payload["all_within_tolerance"],
                "not_monotonically_worsening": payload["not_monotonically_worsening"],
                "combined_deviation_score": payload["combined_deviation_score"],
                "monotonic_worsening_metrics": payload[
                    "monotonic_worsening_metrics"
                ],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if payload["all_within_tolerance"] else 4


def cmd_gui(_args: argparse.Namespace) -> int:
    from .gui.app import main as gui_main

    return gui_main()


def _add_budget_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--time-budget", type=float, default=20.0)
    parser.add_argument("--memory-budget", type=float, default=512.0)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--tile-size", type=int, default=256)
    parser.add_argument("--block-buffer-mb", type=float, default=4.0)


def _add_param_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--color-tolerance", type=float)
    parser.add_argument("--gradient-threshold", type=float)
    parser.add_argument("--confidence-threshold", type=float)
    parser.add_argument("--morph-size", type=int)
    parser.add_argument("--min-area", type=int)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="artmask")
    parser.add_argument("--workspace", help="持久化工作目录，默认 ./workspace_data")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="生成固定种子样本")
    generate.add_argument("--workspace", help="持久化工作目录")
    generate.set_defaults(func=cmd_generate)

    validate = subparsers.add_parser("validate", help="校验全部样本与清单")
    validate.add_argument("--workspace", help="持久化工作目录")
    validate.set_defaults(func=cmd_validate)

    run = subparsers.add_parser("run", help="运行分块分割流水线")
    run.add_argument("--sample", help="仅运行指定 sample_id")
    run.add_argument("--workspace", help="持久化工作目录")
    run.add_argument("--force-mode", choices=("fine", "downsampled"))
    run.add_argument("--force-factor", type=int)
    run.add_argument("--export", action="store_true")
    _add_budget_arguments(run)
    _add_param_arguments(run)
    run.set_defaults(func=cmd_run)

    experiment = subparsers.add_parser("experiment", help="执行双路径一致性实验")
    experiment.add_argument("--sample", help="仅运行指定 sample_id")
    experiment.add_argument("--workspace", help="持久化工作目录")
    _add_budget_arguments(experiment)
    experiment.set_defaults(func=cmd_experiment)

    gui = subparsers.add_parser("gui", help="启动 PyQt5 诊断工作台")
    gui.add_argument("--workspace", help="持久化工作目录")
    gui.set_defaults(func=cmd_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
