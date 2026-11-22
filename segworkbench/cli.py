"""工作台命令行入口。

子命令：
  generate   生成确定性样本与真值清单
  run        运行分块分割（支持注入与并发参数）
  verify     独立验证检查点产物
"""
from __future__ import annotations

import argparse
import json
import sys
from typing import Any, Dict

from segworkbench.core.errors import WorkbenchError
from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.assemble import collect_deviation_table, overall_area_tolerance, stitch_final_mask
from segworkbench.pipeline.concurrency import BoundedRunner
from segworkbench.pipeline.engine import SegmentationEngine
from segworkbench.pipeline.loader import load_sample
from segworkbench.pipeline.parameters import SegmentationParameters
from segworkbench.pipeline.samplegen import generate_all
from segworkbench.utils.hashing import sha256_array
from segworkbench.validation.verifier import run_verification


def _print(payload: Dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _load_params(args: argparse.Namespace) -> SegmentationParameters:
    injections: Dict[str, Any] = {}
    if args.inject:
        with open(args.inject, "r", encoding="utf-8") as handle:
            injections = json.load(handle)
    params = SegmentationParameters(
        color_tol=args.color_tol,
        gradient_tol=args.gradient_tol,
        struct_radius=args.struct_radius,
        min_region_pixels=args.min_region_pixels,
        spill_area_ratio=args.spill_ratio,
        injections=injections,
    )
    params.validate()
    return params


def cmd_generate(args: argparse.Namespace) -> int:
    manifests = generate_all(args.data_dir)
    _print({"generated": [item["sample_id"] for item in manifests],
            "data_dir": args.data_dir,
            "count": len(manifests)})
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    params = _load_params(args)
    sample = load_sample(args.data_dir, args.sample_id)
    store = CheckpointStore(args.checkpoint_dir)
    engine = SegmentationEngine(sample, params, store)
    runner = BoundedRunner(args.max_workers) if args.max_workers > 1 else None
    summary = engine.run_all(runner=runner)
    table = collect_deviation_table(store)
    tolerance = overall_area_tolerance(table)
    mask = stitch_final_mask(store)
    _print({
        "summary": summary,
        "area_tolerance": tolerance,
        "final_mask_sha256": sha256_array(mask),
        "deviation_table": [
            {"block_id": item["block_id"], "deviation": item["deviation"]} for item in table
        ],
    })
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    report = run_verification(args.checkpoint_dir, args.data_dir, args.sample_id)
    _print(report)
    return 0 if report["passed"] else 2


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="segworkbench", description="图像分割与掩码处理工作台")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="生成确定性样本")
    gen.add_argument("--data-dir", default="data/generated")
    gen.set_defaults(func=cmd_generate)

    run = sub.add_parser("run", help="运行分割")
    run.add_argument("--data-dir", default="data/generated")
    run.add_argument("--checkpoint-dir", required=True)
    run.add_argument("--sample-id", required=True)
    run.add_argument("--color-tol", type=float, default=42.0)
    run.add_argument("--gradient-tol", type=float, default=70.0)
    run.add_argument("--struct-radius", type=int, default=1)
    run.add_argument("--min-region-pixels", type=int, default=200)
    run.add_argument("--spill-ratio", type=float, default=1.35)
    run.add_argument("--inject", help="注入配置 JSON 文件路径")
    run.add_argument("--max-workers", type=int, default=1)
    run.set_defaults(func=cmd_run)

    verify = sub.add_parser("verify", help="独立验证")
    verify.add_argument("--data-dir", default="data/generated")
    verify.add_argument("--checkpoint-dir", required=True)
    verify.add_argument("--sample-id", required=True)
    verify.set_defaults(func=cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except WorkbenchError as exc:
        print(json.dumps({"error_code": exc.code, "message": exc.message,
                          "context": exc.context}, ensure_ascii=False, indent=2),
              file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
