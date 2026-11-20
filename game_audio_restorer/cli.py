"""Command-line interface for generation, restoration and comparison."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any, Sequence

from .compare import compare_versions, write_compare_outputs
from .errors import BusinessError
from .recipes import load_recipe
from .restore import RestorationEngine, export_existing
from .samples import generate_dataset


def _emit(payload: Any) -> None:
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2))


def _path(value: str | None, default: Path) -> Path:
    return Path(value) if value else default


def _run_command(args: argparse.Namespace) -> dict[str, Any]:
    source = Path(args.wav)
    recipe = load_recipe(args.recipe)
    work = Path(args.work)
    engine = RestorationEngine(
        source,
        recipe,
        work,
        output_wav=_path(args.output, work / "restored.wav"),
        report_path=_path(args.report, work / "report.json"),
        trace_path=_path(args.trace, work / "trace.jsonl"),
        segment_seconds=args.segment_seconds,
        resume=not args.no_resume,
    )
    result = engine.run(finalize=True)
    return result.to_dict()


def _export_command(args: argparse.Namespace) -> dict[str, Any]:
    source = Path(args.wav)
    recipe = load_recipe(args.recipe)
    work = Path(args.work)
    result = export_existing(
        source,
        recipe,
        work,
        output_wav=_path(args.output, work / "restored.wav"),
        report_path=_path(args.report, work / "report.json"),
        trace_path=_path(args.trace, work / "trace.jsonl"),
    )
    return result.to_dict()


def _compare_command(args: argparse.Namespace) -> dict[str, Any]:
    recipes = [
        load_recipe(args.recipe_v1),
        load_recipe(args.recipe_v2),
        load_recipe(args.recipe_v3),
    ]
    report = compare_versions(
        args.wav,
        recipes,
        args.work,
        segment_seconds=args.segment_seconds,
    )
    output = _path(args.output, Path(args.work) / "comparison.json")
    trace = _path(args.trace, Path(args.work) / "comparison_trace.jsonl")
    write_compare_outputs(report, output, trace)
    return {
        "comparison_report": str(output),
        "comparison_trace": str(trace),
        "comparison_hash": report["hash"],
        "segment_count": report["segment_count"],
        "summary": report["summary"],
    }


def _generate_command(args: argparse.Namespace) -> dict[str, Any]:
    if args.quick:
        rates = (8000, 48000)
        channels = (1, 2)
    else:
        rates = tuple(args.sample_rates)
        channels = tuple(args.channel_layouts)
    return generate_dataset(
        args.out,
        seed=args.seed,
        sample_rates=rates,
        channel_layouts=channels,
        duration_s=args.duration,
        segment_seconds=args.segment_seconds,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="game-audio-restorer",
        description="本地确定性游戏配音修复、跨版本比对与断点恢复工作台",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    for command_name, help_text in (
        ("run", "执行修复；发现有效快照时从最近提交片段恢复"),
        ("resume", "显式从磁盘快照恢复并完成剩余片段"),
    ):
        run = sub.add_parser(command_name, help=help_text)
        run.add_argument("--wav", required=True)
        run.add_argument("--recipe", required=True)
        run.add_argument("--work", required=True)
        run.add_argument("--output")
        run.add_argument("--report")
        run.add_argument("--trace")
        run.add_argument("--segment-seconds", type=float, default=1.0)
        run.add_argument("--no-resume", action="store_true")
        run.set_defaults(handler=_run_command)

    export = sub.add_parser("export", help="从已完成快照校验哈希并原子导出 WAV/报告/轨迹")
    export.add_argument("--wav", required=True)
    export.add_argument("--recipe", required=True)
    export.add_argument("--work", required=True)
    export.add_argument("--output")
    export.add_argument("--report")
    export.add_argument("--trace")
    export.set_defaults(handler=_export_command)

    compare = sub.add_parser("compare", help="对同一 WAV 执行 v1/v2/v3 并逐片段比对")
    compare.add_argument("--wav", required=True)
    compare.add_argument("--recipe-v1", required=True)
    compare.add_argument("--recipe-v2", required=True)
    compare.add_argument("--recipe-v3", required=True)
    compare.add_argument("--work", required=True)
    compare.add_argument("--output")
    compare.add_argument("--trace")
    compare.add_argument("--segment-seconds", type=float, default=1.0)
    compare.set_defaults(handler=_compare_command)

    generate = sub.add_parser("generate", help="生成确定性多采样率/声道/配方样本与基线")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=190_093)
    generate.add_argument("--duration", type=float, default=1.8)
    generate.add_argument("--segment-seconds", type=float, default=1.0)
    generate.add_argument("--sample-rates", type=int, nargs="+", default=[8000, 16000, 44100, 48000])
    generate.add_argument("--channel-layouts", type=int, nargs="+", default=[1, 2])
    generate.add_argument("--quick", action="store_true")
    generate.set_defaults(handler=_generate_command)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.handler(args)
    except BusinessError as exc:
        _emit(exc.to_dict())
        return 2
    _emit(result)
    if result.get("status") == "cancelled":
        return 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
