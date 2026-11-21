#!/usr/bin/env python3
"""确定性实验脚本入口。

典型流程（无网络、无容器）::

    python run_experiment.py demo --work-dir ./workspace

子命令：

generate  生成确定性样本集
demo      生成样本 -> 双路径转换 -> 增量重算 -> 崩溃恢复 -> 校验复现
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import sys

from hdrworkbench import color
from hdrworkbench.experiment import ExperimentRunner, TrajectoryLog
from hdrworkbench.generator import default_specs, write_sample
from hdrworkbench.hdrformat import (
    CS_DISPLAY_P3,
    CS_REC2020,
    TF_PQ,
)
from hdrworkbench.pipeline import ConversionConfig
from hdrworkbench.snapshot import CrashInjectedError, recover_snapshot


def _sha256_file(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 16), b""):
            digest.update(block)
    return digest.hexdigest()


def generate(work_dir: str) -> dict:
    sample_dir = os.path.join(work_dir, "samples")
    os.makedirs(sample_dir, exist_ok=True)
    manifest = {}
    for index, spec in enumerate(default_specs()):
        path = os.path.join(sample_dir, f"sample_{index:02d}.hdr")
        write_sample(path, spec)
        manifest[os.path.basename(path)] = {
            "spec": spec.__dict__,
            "content_hash": _sha256_file(path),
        }
    with open(
        os.path.join(sample_dir, "manifest.json"), "w", encoding="utf-8", newline="\n"
    ) as handle:
        handle.write(json.dumps(manifest, sort_keys=True, indent=2))
    return manifest


def demo(work_dir: str) -> int:
    if os.path.isdir(work_dir):
        shutil.rmtree(work_dir)
    sample_manifest = generate(work_dir)
    sample_path = os.path.join(work_dir, "samples", "sample_00.hdr")
    log_path = os.path.join(work_dir, "trajectory.jsonl")
    log = TrajectoryLog(log_path)
    runner = ExperimentRunner(sample_path, work_dir, log)

    baseline = ConversionConfig(
        target_colorspace=CS_REC2020,
        target_transfer=TF_PQ,
        target_bit_depth=10,
        target_peak_nits=1000,
        tone_curve=color.TONE_REINHARD,
        mode="fixed",
        frac_bits=14,
        tolerance_codes=4.0,
        seed=7,
    )
    runner.run_version(baseline)

    # 修改峰值亮度 + 色调曲线：只重算受影响帧（本样本全帧受影响）
    v2_config = ConversionConfig(
        **{**baseline.factor_dict(), "target_peak_nits": 2000,
           "tone_curve": color.TONE_HABLE}
    )
    runner.run_version(v2_config)

    # 完全相同配置：所有帧都应命中缓存并复用
    runner.run_version(v2_config)

    # 切换目标空间：重跑链路
    v4_config = ConversionConfig(
        **{**v2_config.factor_dict(), "target_colorspace": CS_DISPLAY_P3}
    )
    runner.run_version(v4_config)

    # 浮点路径版本
    v5_config = ConversionConfig(
        **{**v4_config.factor_dict(), "mode": "float"}
    )
    runner.run_version(v5_config)

    # 崩溃恢复实验：独立快照目录，注入到第 2 帧后终止
    crash_dir = os.path.join(work_dir, "snapshots", "crash_case")
    try:
        runner.run_version(
            baseline, snapshot_dir=crash_dir, crash_after=1
        )
    except CrashInjectedError:
        log.event("crash_injected", snapshot_dir=crash_dir)
    recovered = recover_snapshot(crash_dir)
    log.event(
        "recovery_scan",
        committed_frames=recovered["committed_frames"],
        chain_head=recovered["chain_head"],
    )
    runner.run_version(baseline, snapshot_dir=crash_dir)

    log.close()

    current = runner.store.current()
    report = {
        "sample_manifest": sample_manifest,
        "versions": runner.store.history(),
        "current_version_hash": current.version_hash,
        "trajectory_hash": _sha256_file(log_path),
    }
    with open(
        os.path.join(work_dir, "report.json"), "w", encoding="utf-8", newline="\n"
    ) as handle:
        handle.write(json.dumps(report, sort_keys=True, indent=2))
    print(json.dumps(report, sort_keys=True, indent=2))
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="HDR 转换工作台实验入口")
    sub = parser.add_subparsers(dest="command", required=True)
    gen = sub.add_parser("generate", help="生成确定性样本集")
    gen.add_argument("--work-dir", default="./workspace")
    demo_parser = sub.add_parser("demo", help="完整脚本化实验")
    demo_parser.add_argument("--work-dir", default="./workspace")
    args = parser.parse_args(argv)
    if args.command == "generate":
        generate(args.work_dir)
        return 0
    return demo(args.work_dir)


if __name__ == "__main__":
    sys.exit(main())

