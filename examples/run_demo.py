"""端到端可重复运行示例。

演示内容：
  1. 正常分割路径（串行 + 有界并发两次运行，逐像素一致）；
  2. 四类参数/输入注入：E_SEED / E_SPILL / E_REGION / E_BLOCK；
  3. 中断与恢复（取消后重启，只依赖持久化检查点）；
  4. 独立验证工具判定；
  5. 版本不匹配导致整份快照作废并全量重算。

运行：``.venv/bin/python examples/run_demo.py``
"""
from __future__ import annotations

import json
import os
import shutil
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from segworkbench.core.errors import ErrorCode, WorkbenchError  # noqa: E402
from segworkbench.persistence.checkpoint import CheckpointStore  # noqa: E402
from segworkbench.pipeline.assemble import (  # noqa: E402
    collect_deviation_table,
    overall_area_tolerance,
    stitch_final_mask,
)
from segworkbench.pipeline.blocks import all_block_ids, crop_block  # noqa: E402
from segworkbench.pipeline.concurrency import BoundedRunner  # noqa: E402
from segworkbench.pipeline.engine import SegmentationEngine  # noqa: E402
from segworkbench.pipeline.loader import load_sample, observation_baseline  # noqa: E402
from segworkbench.pipeline.parameters import SegmentationParameters  # noqa: E402
from segworkbench.pipeline.samplegen import generate_all  # noqa: E402
from segworkbench.utils.hashing import sha256_array  # noqa: E402
from segworkbench.validation.verifier import run_verification  # noqa: E402

DATA_DIR = os.path.join(ROOT, "data", "generated")
WORKDIR = os.path.join(ROOT, "workdir")
SAMPLE_ID = "sample_01_blocks"


def banner(title: str) -> None:
    print("\n" + "=" * 72)
    print(title)
    print("=" * 72)


def fresh_store(name: str) -> CheckpointStore:
    path = os.path.join(WORKDIR, name)
    if os.path.exists(path):
        shutil.rmtree(path)
    return CheckpointStore(path)


def run_normal(params: SegmentationParameters, store_name: str, workers: int) -> str:
    sample = load_sample(DATA_DIR, SAMPLE_ID)
    store = CheckpointStore(os.path.join(WORKDIR, store_name))
    engine = SegmentationEngine(sample, params, store)
    runner = BoundedRunner(max_workers=workers) if workers > 1 else None
    engine.run_all(runner=runner)
    mask_hash = sha256_array(stitch_final_mask(store))
    table = collect_deviation_table(store)
    tolerance = overall_area_tolerance(table)
    print(f"[{store_name}] workers={workers} mask_sha256={mask_hash}")
    print(f"[{store_name}] 面积容差结论: {json.dumps(tolerance, ensure_ascii=False)}")
    return mask_hash


def expect_error(name: str, expected_code: str, func) -> WorkbenchError:
    try:
        func()
    except WorkbenchError as exc:
        status = "OK" if exc.code == expected_code else "FAIL"
        print(f"[{status}] {name}: code={exc.code} context={json.dumps(exc.context, ensure_ascii=False)}")
        assert exc.code == expected_code, f"期望 {expected_code} 实际 {exc.code}"
        return exc
    raise AssertionError(f"{name} 未触发 {expected_code}")


def main() -> int:
    os.makedirs(WORKDIR, exist_ok=True)
    banner("0. 生成确定性样本与解析真值")
    manifests = generate_all(DATA_DIR)
    print(f"样本: {[item['sample_id'] for item in manifests]} -> {DATA_DIR}")

    sample = load_sample(DATA_DIR, SAMPLE_ID)
    print("观测基线:", json.dumps(observation_baseline(sample), ensure_ascii=False))

    params = SegmentationParameters()

    banner("1. 正常路径：串行 vs 并发，逐像素一致")
    hash_serial = run_normal(params, "normal_serial", workers=1)
    hash_parallel = run_normal(params, "normal_parallel", workers=3)
    assert hash_serial == hash_parallel, "并发与串行掩码不一致"
    print("确定性检查通过：串行/并发掩码哈希完全相同")

    banner("2. 独立验证工具（应判定通过）")
    report = run_verification(os.path.join(WORKDIR, "normal_serial"), DATA_DIR, SAMPLE_ID)
    print(f"verify.passed={report['passed']} mismatch_count={report['mismatch_count']}")
    assert report["passed"]

    banner("3. 注入 E_SEED：种子落在背景")
    bad_seed = (4, 4)  # 位于 b00 角落背景
    seed_params = SegmentationParameters(
        seed_points={"b00": bad_seed},
        injections={"seed_background": {"blocks": ["b00"]}},
    )
    expect_error("seed_background", ErrorCode.E_SEED,
                 lambda: _run_single_block(seed_params, fresh_store("inj_seed"), "b00"))

    banner("4. 注入 E_SPILL：颜色容差过大导致外溢")
    spill_params = SegmentationParameters(
        color_tol=260.0, gradient_tol=400.0,
        injections={"spill_tolerance": {"blocks": ["b01"]}},
    )
    expect_error("spill_tolerance", ErrorCode.E_SPILL,
                 lambda: _run_single_block(spill_params, fresh_store("inj_spill"), "b01"))

    banner("5. 注入 E_REGION：真值区域缺失（输入文件驱动）")
    missing_dir = os.path.join(WORKDIR, "data_missing_region")
    os.makedirs(missing_dir, exist_ok=True)
    _write_manifest_without_region(missing_dir)
    expect_error("missing_truth_region", ErrorCode.E_META,
                 lambda: load_sample(missing_dir, SAMPLE_ID))
    # 另一条 E_REGION 链路：块内连通域数量异常（极小 min_region_pixels 产生多连通域场景由零注入模拟）。
    region_params = SegmentationParameters(
        injections={"region_missing": {"blocks": ["b02"]}}, min_region_pixels=10 ** 9
    )
    expect_error("component_count_mismatch", ErrorCode.E_REGION,
                 lambda: _run_single_block(region_params, fresh_store("inj_region"), "b02"))

    banner("6. 注入 E_BLOCK：坏块（参数驱动的损坏输入）")
    block_params = SegmentationParameters(
        injections={"bad_block": {"blocks": ["b03"], "mode": "corrupt", "seed": 777}}
    )
    expect_error("bad_block_corrupt", ErrorCode.E_BLOCK,
                 lambda: _run_single_block(block_params, fresh_store("inj_block"), "b03"))
    hash_break_params = SegmentationParameters(
        injections={"bad_block": {"blocks": ["b03"], "mode": "zero"}}
    )
    expect_error("bad_block_zero", ErrorCode.E_BLOCK,
                 lambda: _run_single_block(hash_break_params, fresh_store("inj_block_zero"), "b03"))

    banner("7. 中断与恢复：取消后重启，仅依据持久化状态")
    interrupt_hash = _demo_interrupt_resume(params)
    assert interrupt_hash == hash_serial, "恢复路径掩码与正常路径不一致"
    print("恢复一致性检查通过：中断恢复后的掩码哈希等于正常路径")

    banner("8. 版本不匹配：快照整体作废并全量重算")
    _demo_version_invalidation(params, hash_serial)

    banner("全部示例完成")
    return 0


def _run_single_block(params: SegmentationParameters, store: CheckpointStore, block_id: str):
    sample = load_sample(DATA_DIR, SAMPLE_ID)
    engine = SegmentationEngine(sample, params, store, block_ids=[block_id])
    engine.initialize()
    return engine.process_block(block_id)


def _write_manifest_without_region(target_dir: str) -> None:
    source = os.path.join(DATA_DIR, f"{SAMPLE_ID}.json")
    with open(source, "r", encoding="utf-8") as handle:
        manifest = json.load(handle)
    manifest["regions"] = [region for region in manifest["regions"] if region["block_id"] != "b03"]
    with open(os.path.join(target_dir, f"{SAMPLE_ID}.json"), "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, ensure_ascii=False, indent=2, sort_keys=True)
    shutil.copy(os.path.join(DATA_DIR, manifest["image"]["filename"]), target_dir)


def _demo_interrupt_resume(params: SegmentationParameters) -> str:
    store_path = os.path.join(WORKDIR, "interrupt")
    if os.path.exists(store_path):
        shutil.rmtree(store_path)
    sample = load_sample(DATA_DIR, SAMPLE_ID)

    store = CheckpointStore(store_path)
    engine = SegmentationEngine(sample, params, store)
    engine.initialize()
    engine.process_block("b00")
    engine.process_block("b01")
    print("首次运行已提交 b00/b01，模拟用户取消")
    engine.request_cancel()
    try:
        engine.process_block("b02")
    except WorkbenchError as exc:
        assert exc.code == ErrorCode.E_CANCELLED
        print(f"取消按预期生效: {exc.code}")

    # 新进程视角：重建引擎，restore 只读取检查点。
    store2 = CheckpointStore(store_path)
    engine2 = SegmentationEngine(load_sample(DATA_DIR, SAMPLE_ID), params, store2)
    restored = engine2.restore()
    print("重启后恢复的已提交阶段:", json.dumps(restored, ensure_ascii=False, sort_keys=True))
    for block_id in all_block_ids():
        engine2.process_block(block_id)  # 已提交阶段直接复用，未提交块继续计算
    final_hash = sha256_array(stitch_final_mask(store2))
    report = run_verification(store_path, DATA_DIR, SAMPLE_ID)
    assert report["passed"]
    print("恢复后独立验证通过")
    return final_hash


def _demo_version_invalidation(params: SegmentationParameters, good_hash: str) -> None:
    store_path = os.path.join(WORKDIR, "version_mismatch")
    if os.path.exists(store_path):
        shutil.rmtree(store_path)
    store = CheckpointStore(store_path)
    sample = load_sample(DATA_DIR, SAMPLE_ID)
    engine = SegmentationEngine(sample, params, store)
    engine.run_all()

    # 模拟参数版本变化（只改检查点头，不动源码）。
    import json as _json
    header_path = os.path.join(store_path, "snapshot.json")
    with open(header_path, "r", encoding="utf-8") as handle:
        header = _json.load(handle)
    header["param_version"] = "param-9.9.9"
    with open(header_path, "w", encoding="utf-8") as handle:
        _json.dump(header, handle, ensure_ascii=False, indent=2, sort_keys=True)

    engine2 = SegmentationEngine(sample, params, CheckpointStore(store_path))
    try:
        engine2.restore()
    except WorkbenchError as exc:
        assert exc.code == ErrorCode.E_VERSION
        print(f"版本不匹配按预期作废快照: {exc.context}")
    engine2.initialize()
    engine2.run_all()
    rebuilt_hash = sha256_array(stitch_final_mask(CheckpointStore(store_path)))
    assert rebuilt_hash == good_hash
    print("全量重算后掩码哈希与基线一致")


if __name__ == "__main__":
    raise SystemExit(main())
