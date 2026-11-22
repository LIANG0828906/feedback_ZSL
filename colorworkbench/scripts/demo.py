"""端到端验收演示：覆盖 8 项业务要求的关键路径并打印实际结果。"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from cwb.engine import Workbench  # noqa: E402
from cwb.errors import CwbError  # noqa: E402
from cwb.integrity import check_samples  # noqa: E402
from cwb.model import (  # noqa: E402
    INTENT_ABSOLUTE,
    INTENT_PERCEPTUAL,
    INTENT_RELATIVE,
)
from cwb.profile_io import load_profile, parse_profile  # noqa: E402

SAMPLES = os.path.join(ROOT, "samples")


def line(title: str) -> None:
    print("\n=== " + title + " ===")


def main() -> int:
    line("运行前样本完整性 / 位深 / 离线检查")
    print(check_samples(SAMPLES))

    line("1. 多版本配置统一解析（保留源版本标识）")
    for name in ("src_v1", "src_v2", "src_v3"):
        profile = load_profile(os.path.join(SAMPLES, "profiles",
                                            name + ".json"))
        print(f"{name}: version={profile.source_version} "
              f"trc={profile.trc.kind} lut3d={profile.lut3d is not None} "
              f"wp={'explicit' if profile.white_point else 'declared-default'}")

    line("2/3. 完整转换管线 + 渲染意图/白点")
    wb = Workbench(
        os.path.join(SAMPLES, "profiles", "src_v3.json"),
        os.path.join(SAMPLES, "profiles", "dst_v3.json"),
        INTENT_RELATIVE, output_depth=16, batch_rows=8, max_workers=4)
    image_path = os.path.join(SAMPLES, "images", "tile_32x24_16_rgb.cimg")
    wb.load_image(image_path, 16)
    out = wb.run_full(parallel=True)
    stats, hist = wb.stats_and_histogram(out)
    print("输出:", out.shape, out.dtype,
          "mean_abs=%.6f max_abs=%.6f" % (stats["mean_abs"],
                                          stats["max_abs"]))
    intent_means = {}
    for intent in (INTENT_RELATIVE, INTENT_ABSOLUTE, INTENT_PERCEPTUAL):
        probe = Workbench(
            os.path.join(SAMPLES, "profiles", "src_v3.json"),
            os.path.join(SAMPLES, "profiles", "dst_v3.json"),
            intent, output_depth=16)
        probe.load_image(image_path, 16)
        intent_means[intent] = float(probe.run_full(parallel=False).mean())
    print("各意图输出均值（差异可观测）:", {k: round(v, 3)
                                          for k, v in intent_means.items()})

    line("4/5. 编辑 -> 增量重算 -> 与空缓存全量重算逐像素比对")
    new_matrix = [0.40, 0.36, 0.18, 0.21, 0.70, 0.07,
                  0.02, 0.12, 0.93]
    full = Workbench(
        os.path.join(SAMPLES, "profiles", "src_v3.json"),
        os.path.join(SAMPLES, "profiles", "dst_v3.json"),
        INTENT_RELATIVE, output_depth=16, batch_rows=8)
    full.load_image(image_path, 16)
    full.run_full(parallel=False)
    full.editor.edit_matrix(new_matrix)
    full.editor.commit()
    full.pipeline.invalidate(
        [b.batch_id for b in full.pipeline.batches], "S1_trc_invert")
    reference = full.run_full(parallel=False)
    ref_stats, ref_hist = full.stats_and_histogram(reference)

    def edit(ed):
        ed.edit_matrix(new_matrix)

    incr_out = wb.edit_and_recompute(edit)
    report = wb.verify_equivalence(reference, ref_stats, ref_hist)
    print("一致性报告:", report)
    print("失效范围查询:", wb.editor.query_invalidation())

    line("6. 版本化快照与断点恢复")
    snap_dir = tempfile.mkdtemp()
    snap_wb = Workbench(
        os.path.join(SAMPLES, "profiles", "src_v1.json"),
        os.path.join(SAMPLES, "profiles", "dst_v1.json"),
        INTENT_RELATIVE, output_depth=16, snapshot_dir=snap_dir)
    snap_wb.load_image(image_path, 16)
    first = snap_wb.run_full(parallel=False)
    restarted = Workbench(
        os.path.join(SAMPLES, "profiles", "src_v1.json"),
        os.path.join(SAMPLES, "profiles", "dst_v1.json"),
        INTENT_RELATIVE, output_depth=16, snapshot_dir=snap_dir)
    restarted.load_image(image_path, 16)
    second = restarted.run_full(parallel=False)
    print("快照恢复逐像素一致:", bool(np.array_equal(first, second)))
    shutil.rmtree(snap_dir, ignore_errors=True)

    line("7/8. 异常码链路（损坏图像 / 位深 / 越界参数 / 未知版本）")
    for label, fn in (
        ("E_DECODE", lambda: Workbench(
            os.path.join(SAMPLES, "profiles", "src_v1.json"),
            os.path.join(SAMPLES, "profiles", "dst_v1.json"),
            INTENT_RELATIVE, 8).load_image(
                os.path.join(SAMPLES, "corrupt", "truncated.cimg"))),
        ("E_DEPTH", lambda: Workbench(
            os.path.join(SAMPLES, "profiles", "src_v1.json"),
            os.path.join(SAMPLES, "profiles", "dst_v1.json"),
            INTENT_RELATIVE, 16).load_image(image_path, 8)),
    ):
        try:
            fn()
        except CwbError as exc:
            print(f"期望 {label} -> 实际 {exc.code}: {exc.message}")
    raw = json.load(open(os.path.join(SAMPLES, "profiles",
                                      "src_v1.json")))
    raw["red_trc"]["value"] = 99.0
    try:
        parse_profile(raw)
    except CwbError as exc:
        print(f"期望 E_PARA -> 实际 {exc.code} 标签={exc.tag}")
    raw["profile_version"] = 42
    try:
        parse_profile(raw)
    except CwbError as exc:
        print(f"期望 E_VER -> 实际 {exc.code}: {exc.message}")

    print("\n全部验收演示完成。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
