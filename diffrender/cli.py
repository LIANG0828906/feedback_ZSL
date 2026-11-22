"""Command-line interface with integer exit codes and JSON diagnostics."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

import torch

from .compare import compare_reports
from .demo import run_demo
from .diagnostics import export_diagnostics
from .errors import AppError
from .generator import generate_dataset
from .mode_compare import compare_downsample_modes
from .optimize import OptimizationRunner, RunConfig
from .paths import run_three_paths
from .schema import load_dataset
from .strategy import ScaleConfig, StrategyDecision, choose_strategy


def _emit(value: Any, *, stream: Any = None) -> None:
    if stream is None:
        stream = sys.stdout
    stream.write(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    stream.flush()


def _expected(key: str, args: argparse.Namespace, default: Any) -> Any:
    value = getattr(args, key)
    return default if value is None else value


def _run_config(args: argparse.Namespace) -> RunConfig:
    return RunConfig(
        iterations=_expected("iterations", args, 20),
        learning_rate=_expected("learning_rate", args, 0.012),
        learning_rate_min=_expected("learning_rate_min", args, 2.0e-4),
        lr_schedule=_expected("lr_schedule", args, "cosine"),
        mode=_expected("mode", args, "auto"),
        memory_budget_mb=_expected("memory_budget_mb", args, 64.0),
        tile_size=_expected("tile_size", args, 16),
        max_parallel_blocks=_expected("max_parallel_blocks", args, 2),
        requested_downsample_factor=_expected("downsample_factor", args, 0),
        clip_enabled=not bool(args.no_clip),
        clip_norm=_expected("clip_norm", args, 5.0),
        explode_threshold=_expected("explode_threshold", args, 1.0e4),
        failure_patience=_expected("failure_patience", args, 2),
        checkpoint_every=_expected("checkpoint_every", args, 2),
        max_block_retries=_expected("max_block_retries", args, 1),
        seed=args.seed,
    )


def _add_config_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--iterations", type=int, default=None)
    parser.add_argument("--learning-rate", dest="learning_rate", type=float, default=None)
    parser.add_argument(
        "--learning-rate-min", dest="learning_rate_min", type=float, default=None
    )
    parser.add_argument(
        "--lr-schedule",
        dest="lr_schedule",
        choices=("none", "cosine"),
        default=None,
    )
    parser.add_argument(
        "--mode", choices=("auto", "exact", "downsample", "tiled"), default=None
    )
    parser.add_argument(
        "--memory-budget-mb", dest="memory_budget_mb", type=float, default=None
    )
    parser.add_argument("--tile-size", dest="tile_size", type=int, default=None)
    parser.add_argument(
        "--max-parallel-blocks",
        dest="max_parallel_blocks",
        type=int,
        default=None,
    )
    parser.add_argument(
        "--downsample-factor",
        dest="downsample_factor",
        type=int,
        choices=(0, 1, 2, 4, 8, 16),
        default=None,
    )
    parser.add_argument("--clip-norm", dest="clip_norm", type=float, default=None)
    parser.add_argument(
        "--explode-threshold", dest="explode_threshold", type=float, default=None
    )
    parser.add_argument(
        "--failure-patience", dest="failure_patience", type=int, default=None
    )
    parser.add_argument(
        "--checkpoint-every", dest="checkpoint_every", type=int, default=None
    )
    parser.add_argument(
        "--max-block-retries", dest="max_block_retries", type=int, default=None
    )
    parser.add_argument("--seed", type=int, default=None)
    parser.add_argument(
        "--no-clip",
        action="store_true",
        help="disable gradient norm clipping (failure detection remains active)",
    )


def _load_task(args: argparse.Namespace):
    dataset = load_dataset(args.dataset)
    if args.group not in dataset.groups:
        raise AppError(
            "E_SCHEMA",
            f"group not found in dataset: {args.group}",
            "groups",
            {"available": sorted(dataset.groups)},
        )
    return dataset.task(args.group)


def command_generate(args: argparse.Namespace) -> int:
    result = generate_dataset(args.dataset, seed=args.seed, force=args.force)
    _emit(result)
    return 0


def command_run(args: argparse.Namespace) -> int:
    task = _load_task(args)
    config = _run_config(args)
    output = Path(args.out).resolve()
    output.mkdir(parents=True, exist_ok=True)
    checkpoint_dir = (
        Path(args.checkpoint_dir).resolve()
        if args.checkpoint_dir is not None
        else output / "checkpoints"
    )
    runner = OptimizationRunner(task, config, checkpoint_dir=checkpoint_dir)
    if args.resume:
        runner.load_checkpoint()
    runner.run_to(config.iterations)
    report = runner.report()
    report_path = output / "run_report.json"
    report_path.write_text(
        json.dumps(report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True)
        + "\n",
        encoding="utf-8",
    )
    diagnostics = None
    if not args.no_diagnostics:
        diagnostics = export_diagnostics(runner, output / "diagnostics")
    result = {
        "ok": True,
        "task_id": task.scene.scene_id,
        "run_report": str(report_path),
        "strategy": report.strategy,
        "final_loss": report.final_loss,
        "loss_start": report.loss_history[0],
        "loss_end": report.loss_history[-1],
        "iterations_completed": report.iterations_completed,
        "checkpoint_load": report.checkpoint_load,
        "checkpoints": report.checkpoints,
        "diagnostics": diagnostics,
    }
    _emit(result)
    return 0


def command_verify(args: argparse.Namespace) -> int:
    task = _load_task(args)
    config = _run_config(args)
    output = Path(args.out).resolve()
    result = run_three_paths(
        task,
        config,
        output,
        checkpoint_every=args.checkpoint_every,
    )
    summary = {
        "ok": result["ok"],
        "task_id": result["task_id"],
        "split_iteration": result["split_iteration"],
        "checkpoint_every": result["checkpoint_every"],
        "comparisons": {
            key: {
                "equal": value["equal"],
                "difference_count": value["difference_count"],
                "first_difference": value["first_difference"],
            }
            for key, value in result["comparisons"].items()
        },
        "loss_history": result["realtime"]["loss_history"],
        "final_loss": result["realtime"]["final_loss"],
        "submission_sequence_length": len(
            result["realtime"]["submission_sequence"]
        ),
        "diagnostics": result["diagnostics"],
        "report": str(output / "three_path_report.json"),
    }
    _emit(summary)
    return 0


def command_compare(args: argparse.Namespace) -> int:
    reference = json.loads(Path(args.reference).read_text(encoding="utf-8"))
    candidate = json.loads(Path(args.candidate).read_text(encoding="utf-8"))
    result = compare_reports(
        reference, candidate, compare_gradient_history=args.compare_gradients
    )
    if args.out:
        Path(args.out).write_text(
            json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    _emit(result)
    if not result["equal"]:
        first = result["first_difference"]
        raise AppError(
            "E_DIFF",
            "run reports differ",
            first["field_path"],
            result,
        )
    return 0


def command_mode_compare(args: argparse.Namespace) -> int:
    task = _load_task(args)
    try:
        factors = tuple(int(value) for value in args.factors.split(","))
    except ValueError:
        raise AppError(
            "E_PARA",
            "factors must be a comma-separated list of integers",
            "factors",
            {"value": args.factors},
        )
    report = compare_downsample_modes(task, factors=factors)
    if args.out:
        Path(args.out).write_text(
            json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    _emit(report)
    return 0


def command_strategy(args: argparse.Namespace) -> int:
    task = _load_task(args)
    config = _run_config(args)
    scale = ScaleConfig(
        mode=config.mode,
        memory_budget_mb=config.memory_budget_mb,
        max_exact_pixels=config.max_exact_pixels,
        max_exact_iterations=config.max_exact_iterations,
        downsample_iteration_threshold=config.downsample_iteration_threshold,
        tile_size=config.tile_size,
        max_parallel_blocks=config.max_parallel_blocks,
        requested_downsample_factor=config.requested_downsample_factor,
        shadow_samples=task.scene.soft_shadow_samples,
    )
    decision = choose_strategy(
        object_count=len(task.scene.geometries),
        width=task.targets[0].width,
        height=task.targets[0].height,
        image_count=len(task.targets),
        iterations=config.iterations,
        config=scale,
    )
    _emit({"ok": True, "task_id": task.scene.scene_id, "decision": decision.to_dict()})
    return 0


def command_demo(args: argparse.Namespace) -> int:
    result = run_demo(
        args.dataset,
        args.out,
        seed=args.seed,
        iterations=args.iterations,
    )
    _emit(result)
    if not result.get("ok", False):
        raise AppError(
            "E_INTERNAL",
            "demo completed with one or more failed checks",
            "demo",
            {"summary": str(Path(args.out).resolve() / "demo_summary.json")},
        )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="diffrender",
        description=(
            "Deterministic differentiable ray casting and inverse imaging toolkit"
        ),
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--dataset", default="data")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument("--force", action="store_true")
    generate.set_defaults(handler=command_generate)

    run = subparsers.add_parser("run", help="run one inverse-rendering optimization")
    run.add_argument("--dataset", default="data")
    run.add_argument("--group", required=True)
    run.add_argument("--out", default="artifacts/run")
    run.add_argument("--checkpoint-dir", default=None)
    run.add_argument("--resume", action="store_true")
    run.add_argument("--no-diagnostics", action="store_true")
    _add_config_arguments(run)
    run.set_defaults(handler=command_run)

    verify = subparsers.add_parser(
        "verify", help="compare realtime, resumed and one-shot paths"
    )
    verify.add_argument("--dataset", default="data")
    verify.add_argument("--group", required=True)
    verify.add_argument("--out", default="artifacts/verify")
    _add_config_arguments(verify)
    verify.set_defaults(handler=command_verify)

    compare = subparsers.add_parser("compare", help="compare two run report JSON files")
    compare.add_argument("--reference", required=True)
    compare.add_argument("--candidate", required=True)
    compare.add_argument("--out")
    compare.add_argument("--compare-gradients", action="store_true")
    compare.set_defaults(handler=command_compare)

    mode_compare = subparsers.add_parser(
        "mode-compare", help="measure exact versus downsampled forward differences"
    )
    mode_compare.add_argument("--dataset", default="data")
    mode_compare.add_argument("--group", required=True)
    mode_compare.add_argument("--factors", default="1,2,4,8,16")
    mode_compare.add_argument("--out")
    mode_compare.set_defaults(handler=command_mode_compare)

    strategy = subparsers.add_parser("strategy", help="show automatic mode selection")
    strategy.add_argument("--dataset", default="data")
    strategy.add_argument("--group", required=True)
    _add_config_arguments(strategy)
    strategy.set_defaults(handler=command_strategy)

    demo = subparsers.add_parser(
        "demo", help="run the complete acceptance demonstration"
    )
    demo.add_argument("--dataset", default="data")
    demo.add_argument("--out", default="artifacts/demo")
    demo.add_argument("--seed", type=int, default=20261123)
    demo.add_argument("--iterations", type=int, default=20)
    demo.set_defaults(handler=command_demo)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    try:
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
    except (AttributeError, OSError):
        pass
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        torch.set_num_threads(1)
        return int(args.handler(args))
    except AppError as exc:
        _emit(exc.to_dict(), stream=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        error = AppError(
            "E_INTERNAL",
            "interrupted; no complete optimization report is promised",
            "interrupt",
        )
        _emit(error.to_dict(), stream=sys.stderr)
        return error.exit_code
    except Exception as exc:  # noqa: BLE001 - process boundary conversion
        error = AppError(
            "E_INTERNAL",
            f"unexpected failure: {type(exc).__name__}: {exc}",
            "internal",
        )
        _emit(error.to_dict(), stream=sys.stderr)
        return error.exit_code


if __name__ == "__main__":
    raise SystemExit(main())

