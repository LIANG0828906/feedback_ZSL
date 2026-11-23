"""命令行入口（本地运行，无外部服务依赖）。"""
from __future__ import annotations

import argparse
import os
import sys

# 允许 `python -m loudbench.cli.main` 与仓库内直接脚本两种方式
if __package__ in (None, ""):
    sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from loudbench.audio.io import write_wav_float32  # noqa: E402
from loudbench.core.config import (  # noqa: E402
    ALGORITHM_VERSION, CompressorConfig, FrameConfig, LimiterConfig,
    NormalizeConfig, PipelineConfig, SchedulerConfig)
from loudbench.core.errors import BenchError  # noqa: E402
from loudbench.pipeline.app import (  # noqa: E402
    build_orchestrator, verify_three_paths)
from loudbench.pipeline.report import dump_report  # noqa: E402


def _config_from_args(args: argparse.Namespace) -> PipelineConfig:
    return PipelineConfig(
        frame=FrameConfig(),
        compressor=CompressorConfig(
            threshold_db=args.comp_threshold, ratio=args.comp_ratio,
            attack_seconds=args.comp_attack,
            release_seconds=args.comp_release),
        limiter=LimiterConfig(ceiling_db=args.limit_ceiling,
                              lookahead_seconds=args.limit_lookahead),
        normalize=NormalizeConfig(target_lufs=args.target_lufs),
        scheduler=SchedulerConfig(max_workers=args.workers),
    )


def _add_common_params(p: argparse.ArgumentParser) -> None:
    p.add_argument("--target-lufs", type=float, default=-23.0)
    p.add_argument("--comp-threshold", type=float, default=-20.0)
    p.add_argument("--comp-ratio", type=float, default=3.0)
    p.add_argument("--comp-attack", type=float, default=0.01)
    p.add_argument("--comp-release", type=float, default=0.1)
    p.add_argument("--limit-ceiling", type=float, default=-1.0)
    p.add_argument("--limit-lookahead", type=float, default=0.002)
    p.add_argument("--workers", type=int, default=2)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="loudbench", description="广播响度与动态范围处理工作台")
    sub = parser.add_subparsers(dest="cmd", required=True)

    gen = sub.add_parser("generate", help="生成确定性样本")
    gen.add_argument("--out", default="samples/data")

    run = sub.add_parser("run", help="运行处理链")
    run.add_argument("--manifest", default="samples/manifest.json")
    run.add_argument("--checkpoints", default="checkpoints")
    run.add_argument("--exports", default="exports")
    run.add_argument("--serial", action="store_true")
    run.add_argument("--restore", action="store_true")
    _add_common_params(run)

    verify = sub.add_parser("verify", help="三路径一致性验收")
    verify.add_argument("--manifest", default="samples/manifest.json")
    verify.add_argument("--checkpoints", default="checkpoints")
    verify.add_argument("--exports", default="exports")
    _add_common_params(verify)

    args = parser.parse_args(argv)

    try:
        if args.cmd == "generate":
            from samples.generate_samples import generate
            manifest = generate(args.out)
            print(f"OK 生成 {len(manifest['segments'])} 段样本")
            return 0

        config = _config_from_args(args)
        if args.cmd == "run":
            config.validate(48000)
            orch = build_orchestrator(args.manifest, config,
                                      os.path.join(args.checkpoints, "cli_run"))
            if args.restore and not orch.restore():
                print("WARN 无有效检查点，执行全量计算")
            orch.run(parallel=not args.serial)
            fs, audio = orch.concatenated_output()
            wav_path = os.path.join(args.exports, "cli_program.wav")
            write_wav_float32(wav_path, audio, fs)
            report_path = os.path.join(args.exports, "cli_report.json")
            dump_report(report_path, orch.build_report())
            print(f"OK 进度 {orch.progress()} 导出 {wav_path}")
            print(f"   报告 {report_path} 算法版本 {ALGORITHM_VERSION}")
            return 0

        if args.cmd == "verify":
            config.validate(48000)
            result = verify_three_paths(
                args.manifest, config, args.checkpoints, args.exports)
            rep = result["report"]
            print("OK 三路径逐样本一致")
            print(f"   整体响度 {rep['overall_integrated_lufs']:.3f} "
                  f"LUFS（目标 {rep['target_lufs']}，"
                  f"容差内={rep['within_tolerance']}）")
            print(f"   导出 {result['export_wav']}")
            return 0
    except BenchError as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        return 1
    parser.print_help()
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
