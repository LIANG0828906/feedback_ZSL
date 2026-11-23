"""端到端可重复演示：在离屏环境跑通全部 8 项业务能力并落盘报告。"""
from __future__ import annotations

import argparse
import json
import os
import threading
import time

import numpy as np

from .compare import diff_heatmap
from .errors import CadError
from .glcontext import probe_opengl
from .pipeline import PipelineConfig, RenderPipeline
from .samplegen import generate
from .section import Plane
from .tessellation import mesh_signature, tessellate


def run(workdir: str, checkpoint: str, export: str, *,
        width: int = 256, height: int = 256, workers: int = 3) -> dict:
    t_start = time.perf_counter()
    report: dict = {"steps": {}}

    # 0. OpenGL 能力探测（软件回退 + E_MEM 记录）
    gl = probe_opengl(width, height)
    report["steps"]["opengl_probe"] = gl.to_dict()

    # 1. 确定性样本生成
    manifest = generate(workdir)
    report["steps"]["sample_generation"] = {
        "seed": manifest["seed"],
        "surfaces": len(manifest["surfaces"]),
        "meshes": len(manifest["subdivision_meshes"]),
        "content_hash": manifest["content_hash"],
    }

    cfg = PipelineConfig(workdir=workdir, checkpoint_dir=checkpoint,
                         export_dir=export, width=width, height=height,
                         max_workers=workers)
    pipe = RenderPipeline(cfg)

    # 2. 几何真值校验
    geom = pipe.verify_geometry()
    report["steps"]["geometry_truth_max_abs_xyz"] = geom
    report["steps"]["geometry_tolerance"] = \
        manifest["declared_tolerance"]["sample_xyz"]

    # 3. 细分确定性：同密度两次逐顶点一致 + 收紧单调
    p0 = pipe.patches[0]
    m_a = tessellate(p0, 24)
    m_b = tessellate(p0, 24)
    deterministic = mesh_signature(m_a) == mesh_signature(m_b)
    counts = [len(tessellate(p0, d).triangles) for d in (8, 16, 32, 64)]
    monotonic = all(b >= a for a, b in zip(counts, counts[1:]))
    report["steps"]["tessellation"] = {
        "deterministic_vertex_identical": deterministic,
        "triangle_counts_by_density": counts,
        "monotonic_non_decreasing": monotonic,
        "adaptive_density_used": m_a.density,
    }

    # 4. 预览帧
    preview = pipe.render_preview()
    pipe.export_frame_png(preview, "preview.png")
    report["steps"]["preview"] = {
        "lod": preview.params.lod,
        "shade_model": preview.params.shade_model,
        "version": preview.params.version,
        "coverage": preview.coverage,
    }

    # 5. 精确帧（并行）+ 串行提交序列
    precise_par, sched_par = pipe.render_precise(parallel=True)
    pipe.export_frame_png(precise_par, "precise_parallel.png")
    report["steps"]["precise_parallel"] = {
        "version": precise_par.params.version,
        "shade_model": precise_par.params.shade_model,
        "complete": getattr(precise_par, "complete", True),
        "tile_order": sched_par.order,
        "quantiles": sched_par.quantiles(),
        "queue_events": sched_par.queue_events,
        "failed_tiles": sched_par.failed,
    }

    # 6. 单线程串行重跑：逐像素一致 + 相同提交序列
    pipe.store.discard()
    pipe2 = RenderPipeline(cfg)
    precise_ser, sched_ser = pipe2.render_precise(parallel=False)
    pixel_equal = bool(np.array_equal(precise_par.color, precise_ser.color))
    depth_equal = bool(np.allclose(
        np.where(np.isinf(precise_par.depth), 0, precise_par.depth),
        np.where(np.isinf(precise_ser.depth), 0, precise_ser.depth),
        atol=1e-6))
    report["steps"]["parallel_vs_serial"] = {
        "pixel_identical": pixel_equal,
        "depth_identical": depth_equal,
        "same_commit_sequence": sched_par.order == sched_ser.order,
    }

    # 7. 自动一致性比对（同一相机/裁剪）
    pipe2.last_preview = preview
    try:
        div = pipe2.compare()
        report["steps"]["divergence_check"] = div.to_dict()
    except CadError as exc:
        report["steps"]["divergence_check"] = exc.to_dict()
    pipe2.export_heatmap_png("diff_heatmap.png")

    # 8. 断点恢复：模拟完成一半后强制终止，再用新进程对象恢复
    pipe3 = RenderPipeline(cfg)
    cancel = threading.Event()
    pipe3.store.discard()
    jobs = pipe3.scheduler.build_jobs()
    half = max(1, len(jobs) // 2)
    # 仅提交前 half 个分块，模拟崩溃
    meshes = pipe3.tessellate_all(cfg.precise_density)
    for job in jobs[:half]:
        c, d = pipe3.rast.render_tile(meshes, job.x0, job.y0, job.w,
                                      job.h, "precise",
                                      pipe3.snapshot.version)
        pipe3.store.commit_tile(job.tile_id, job.tx, job.ty,
                                pipe3.snapshot.version,
                                pipe3.snapshot.content_hash, c, d)
    recovered = pipe3.store.recover(pipe3.snapshot.version,
                                    pipe3.snapshot.content_hash)
    report["steps"]["crash_recovery"] = {
        "persisted_before_kill": half,
        "recovered_after_restart": len(recovered),
        "only_persisted_claimed": len(recovered) == half,
    }
    # 续跑补齐
    frame_resume, sched_resume = pipe3.render_precise(parallel=True)
    report["steps"]["crash_recovery"]["resumed_complete"] = \
        getattr(frame_resume, "complete", False)
    report["steps"]["crash_recovery"]["resumed_order"] = sched_resume.order

    # 9. 版本冻结边界
    ver_before = pipe3.snapshot.version
    pipe3.edit_control_point(pipe3.patches[0].name, 0, 0,
                             np.array([0.05, 0.0, 0.0]))
    frozen = pipe3.versions.frozen_tiles(ver_before)
    report["steps"]["versioning"] = {
        "old_version": ver_before,
        "new_version": pipe3.snapshot.version,
        "frozen_tiles_on_old_version": sorted(frozen)[:8],
        "frozen_count": len(frozen),
    }

    # 10. 剖面测量（双读数）
    plane = Plane(point=pipe2.camera.target,
                  normal=np.array([0.0, 0.0, 1.0]))
    section = pipe2.section(plane)
    report["steps"]["section"] = section.annotation

    report["elapsed_seconds"] = round(time.perf_counter() - t_start, 3)
    out = pipe2.export_report(report, "demo_report.json")
    return report


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--workdir", default="workdir")
    ap.add_argument("--checkpoint", default="checkpoint")
    ap.add_argument("--export", default="demo_output")
    ap.add_argument("--workers", type=int, default=3)
    ap.add_argument("--size", type=int, default=256)
    args = ap.parse_args()
    report = run(args.workdir, args.checkpoint, args.export,
                 width=args.size, height=args.size, workers=args.workers)
    print(json.dumps(report, indent=2, ensure_ascii=False, default=str))


if __name__ == "__main__":
    main()
