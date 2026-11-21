"""脚本化演示入口。

流程：GL 离屏检查 -> 生成确定性样本与基线哈希 -> 载入工作台 ->
视点扫描（LOD 选择 + 块间一致性校验）-> 后台编辑（裁剪/平滑/
降采样/颜色覆盖）-> 原子提交与回退 -> 输出轨迹日志与离屏渲染图。

轨迹日志中的 frame_cost_us 为确定性帧耗时模型
（displayed_points * 2 + visible_blocks * 50，单位微秒），
保证相同种子与操作序列下日志逐字节一致；真实墙钟耗时另行打印。
"""

from __future__ import annotations

import argparse
import json
import math
import os
import time

import numpy as np

from . import samplegen
from .blocks import rebuild
from .errors import PcwbError
from .glcheck import check_offscreen
from .lod import select_display, verify_seam_consistency
from .ops import select_box, select_sphere
from .runtime import Workbench
from .store import VersionStore


def frame_cost_us(displayed: int, visible_blocks: int) -> int:
    return displayed * 2 + visible_blocks * 50


class Trace:
    def __init__(self, path: str):
        self._fh = open(path, "w", encoding="utf-8")

    def log(self, **fields):
        parts = []
        for key, value in fields.items():
            if isinstance(value, (dict, list, tuple)):
                value = json.dumps(value, sort_keys=True, ensure_ascii=False)
            parts.append(f"{key}={value}")
        self._fh.write(" ".join(parts) + "\n")
        self._fh.flush()

    def close(self):
        self._fh.close()


def viewpoints_for(center, radius: float, count: int = 6):
    out = []
    for i in range(count):
        ang = 2.0 * math.pi * i / count
        out.append((center[0] + radius * math.cos(ang),
                    center[1] + radius * math.sin(ang),
                    center[2] + 0.5 * radius))
    return out


def run_demo(samples_dir: str, out_dir: str, edge_length: float = 10.0,
             budget: int = 2_000_000, skip_large: bool = False,
             render_image: bool = True) -> str:
    os.makedirs(out_dir, exist_ok=True)
    trace = Trace(os.path.join(out_dir, "trace.log"))

    ok, gl_info = check_offscreen()
    trace.log(event="gl_check", ok=str(ok).lower(), backend=gl_info.get("backend"),
              renderer=gl_info.get("renderer", "unknown"))
    if not ok:
        raise RuntimeError("OpenGL 离屏上下文不可用")

    hashes = samplegen.generate_samples(samples_dir)
    for name in sorted(hashes):
        trace.log(event="baseline", sample=name, content_hash=hashes[name])

    names = ["fragment_100k"] if skip_large else ["fragment_100k", "full_5m"]
    for name in names:
        _run_sample(name, samples_dir, out_dir, edge_length, budget,
                    trace, render_image and not skip_large)
    trace.close()
    return os.path.join(out_dir, "trace.log")


def _run_sample(name, samples_dir, out_dir, edge_length, budget, trace,
                render_image):
    work_dir = os.path.join(out_dir, f"{name}_work")
    store = VersionStore(work_dir, edge_length=edge_length)
    record = store.load_initial(os.path.join(samples_dir, f"{name}.pcwb"))
    initial_index = store.current_index
    trace.log(event="load", sample=name, version=record.version,
              points=store.current_cloud.count,
              blocks=len(store.current_index.blocks),
              content_hash=record.output_hash)

    wb = Workbench(store)
    known_hashes = {0: record.output_hash}
    try:
        cloud = store.current_cloud
        center = (cloud.points.min(axis=0) + cloud.points.max(axis=0)) / 2.0

        for level in (0, 1, 2):
            truth = verify_seam_consistency(store.current_index, level, seed=0)
            trace.log(event="seam_check", sample=name, level=level,
                      truth_points=int(truth.shape[0]), status="OK")

        for frame, vp in enumerate(viewpoints_for(center, radius=150.0)):
            snap = wb.render_frame()
            selection = select_display(snap.block_index, vp, budget=budget,
                                       seed=0)
            cost = frame_cost_us(selection.total_displayed,
                                 selection.visible_block_count)
            trace.log(event="frame", sample=name, frame=frame,
                      version=snap.version, op="navigate",
                      params={"viewpoint": [round(float(v), 6) for v in vp]},
                      visible_blocks=selection.visible_block_count,
                      displayed_points=selection.total_displayed,
                      frame_cost_us=cost, content_hash=snap.content_hash)

        edits = _edit_script(cloud)
        for step, (op, params, select_fn) in enumerate(edits):
            current = wb.render_frame()
            selection = select_fn(current)
            wb.submit_edit(op, params, selection)
            snap = wb.render_frame()
            from .pcformat import content_hash as _ch
            consistent = _ch(snap.points, snap.colors) == snap.content_hash
            trace.log(event="render_while_editing", sample=name, step=step,
                      consistent=str(consistent).lower())
            result = wb.wait_result(timeout=600)
            if not result.ok:
                raise result.error
            rec = result.record
            known_hashes[rec.version] = rec.output_hash
            trace.log(event="commit", sample=name, version=rec.version,
                      op=rec.op, params=rec.params,
                      replaced_blocks=len(rec.replaced_blocks),
                      input_hash=rec.input_hash,
                      content_hash=rec.output_hash)

        target = 1
        wb.submit_edit("rollback", {"target_version": target},
                       selection=np.array([0], dtype=np.int64))
        result = wb.wait_result(timeout=600)
        if not result.ok:
            raise result.error
        rec = result.record
        known_hashes[rec.version] = rec.output_hash
        trace.log(event="commit", sample=name, version=rec.version, op=rec.op,
                  params=rec.params, replaced_blocks=len(rec.replaced_blocks),
                  input_hash=rec.input_hash, content_hash=rec.output_hash)

        _, report = rebuild(store.current_cloud, edge_length, initial_index)
        trace.log(event="rebuild", sample=name,
                  old_blocks=report.old_block_count,
                  new_blocks=report.new_block_count,
                  old_points=report.old_total_points,
                  new_points=report.new_total_points,
                  point_diff=report.point_diff)

        if render_image:
            from .glrender import render_points_ppm
            snap = wb.render_frame()
            selection = select_display(snap.block_index, center, budget=budget,
                                       seed=0)
            idx = selection.all_indices()
            out_ppm = os.path.join(out_dir, f"{name}_final.ppm")
            render_points_ppm(snap.points[idx], snap.colors[idx], out_ppm)
            trace.log(event="render_image", sample=name,
                      path=os.path.basename(out_ppm),
                      displayed_points=int(idx.shape[0]))
    finally:
        wb.shutdown()


def _edit_script(cloud):
    """确定性编辑操作序列：矩形框选 + 球形框选覆盖四类算子。

    选区在每一步对当前已提交版本现算，避免索引随版本变化失效。
    """
    pmin = cloud.points.min(axis=0)
    pmax = cloud.points.max(axis=0)
    mid = (pmin + pmax) / 2.0
    quarter = (pmax - pmin) / 4.0

    def box_sel(c):
        return select_box(c.points, mid - quarter, mid + quarter)

    def sphere_sel(c):
        return select_sphere(c.points, mid, float(quarter[0]))

    def smooth_sel(c):
        sel = select_sphere(c.points, mid, float(quarter[0]))
        stride = max(1, sel.shape[0] // 1000)
        return sel[::stride]

    return [
        ("color_override", {"rgb": [255, 32, 32]}, box_sel),
        ("smooth", {"iterations": 1, "lambda": 0.5, "radius": 5.0},
         smooth_sel),
        ("downsample", {"ratio": 0.5, "seed": 7}, box_sel),
        ("crop", {"mode": "remove"}, sphere_sel),
    ]


def main(argv=None):
    parser = argparse.ArgumentParser(description="点云工作台脚本化演示")
    parser.add_argument("--samples-dir", default="samples")
    parser.add_argument("--out-dir", default="out")
    parser.add_argument("--edge-length", type=float, default=10.0)
    parser.add_argument("--budget", type=int, default=2_000_000)
    parser.add_argument("--skip-large", action="store_true")
    parser.add_argument("--no-render-image", action="store_true")
    args = parser.parse_args(argv)

    started = time.monotonic()
    try:
        trace_path = run_demo(
            samples_dir=args.samples_dir,
            out_dir=args.out_dir,
            edge_length=args.edge_length,
            budget=args.budget,
            skip_large=args.skip_large,
            render_image=not args.no_render_image,
        )
    except PcwbError as exc:
        print(f"演示失败: {exc}")
        return 1
    elapsed = time.monotonic() - started
    print(f"演示完成: {trace_path} (wall={elapsed:.1f}s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
