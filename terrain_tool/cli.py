"""命令行入口。

子命令::

    generate   生成确定性样本与基线哈希
    verify     端到端验收：增量/全量一致、原子提交、强制终止恢复
    gui        启动 PySide6 示例界面
"""

from __future__ import annotations

import argparse
import json
import os

import numpy as np

from .binary_io import load_heightfield, load_material, load_patch
from .engine import TerrainEngine
from .hashing import GENESIS_HASH
from .pipeline import normal_angle_degrees
from .render import OffscreenRenderer
from .samples import TERRAIN_TYPES, generate_all, generate_sample
from .snapshot import commit_batch, latest_seq, recover


def _fresh_engine(bundle, **kwargs) -> TerrainEngine:
    hf = load_heightfield(bundle.height_path)
    mat = load_material(bundle.material_path)
    return TerrainEngine(hf, mat, **kwargs)


def cmd_generate(args: argparse.Namespace) -> int:
    if args.all:
        bundles = generate_all(
            args.sample_root,
            chunk_size=args.chunk_size,
            patch_count=args.patch_count,
        )
        for kind, bundle in bundles.items():
            print(f"[{kind}] 基线高程哈希 {bundle.baseline['height_hash'][:16]}…")
    else:
        bundle = generate_sample(
            os.path.join(args.sample_root, args.terrain),
            terrain_type=args.terrain,
            chunk_size=args.chunk_size,
            patch_count=args.patch_count,
        )
        print(f"基线高程哈希 {bundle.baseline['height_hash']}")
        print(f"基线材质哈希 {bundle.baseline['material_hash']}")
    print(f"样本目录：{args.sample_root}")
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    """验收主流程，返回非零表示失败。"""
    bundle = generate_sample(
        os.path.join(args.sample_root, args.terrain),
        terrain_type=args.terrain,
        chunk_size=args.chunk_size,
        patch_count=args.patch_count,
    )
    engine_kwargs = dict(
        chunk_size=args.chunk_size,
        lod_threshold=args.lod_threshold,
        crack_tolerance=args.crack_tolerance,
    )

    # ---------- 增量路径 ----------
    inc_engine = _fresh_engine(bundle, **engine_kwargs)
    inc_engine.full_rebuild(seq=0)
    inc_renderer = OffscreenRenderer(inc_engine)
    inc_renderer.render_full()
    chain = GENESIS_HASH
    seq_records = []
    for patch_path in bundle.patch_paths:
        patch = load_patch(patch_path)
        report = inc_engine.apply_patch(patch)
        summary = inc_renderer.render_incremental(report)
        invalidated = {
            kind.value: sorted(list(chunks))
            for kind, chunks in report.invalidation.invalid.items()
        }
        record = commit_batch(
            args.snapshot_dir,
            inc_engine.elevations,
            inc_engine.material,
            inc_engine.cell_size,
            inc_engine.valid_rect,
            inc_engine.version,
            chain,
            invalidated,
            summary.to_dict(),
        )
        chain = record.chain
        seq_records.append((report, summary))
        print(
            f"补丁 {patch.seq}: 重算 {summary.invalidated_count} 块, "
            f"复用 {summary.reused_count} 块, 接缝 {summary.seam_status}"
        )

    # ---------- 全量真值路径 ----------
    full_engine = _fresh_engine(bundle, **engine_kwargs)
    for patch_path in bundle.patch_paths:
        full_engine.apply_patch(load_patch(patch_path))
    full_engine.full_rebuild()
    full_renderer = OffscreenRenderer(full_engine)
    full_summary = full_renderer.render_full()

    color_equal = np.array_equal(inc_renderer.color_buffer, full_renderer.color_buffer)
    heat_equal = np.array_equal(inc_renderer.heat_buffer, full_renderer.heat_buffer)
    angles = normal_angle_degrees(inc_engine.normals, full_engine.normals)
    blend_diff = np.abs(
        inc_engine.blend.astype(np.float64) - full_engine.blend.astype(np.float64)
    )
    pixel_equal = np.array_equal(inc_engine.blend, full_engine.blend)

    # 连续两次渲染一致
    again = full_renderer.render_full()
    deterministic = (
        again.color_hash == full_summary.color_hash
        and again.heat_hash == full_summary.heat_hash
    )

    # ---------- 强制终止恢复 ----------
    crash_tmp = os.path.join(args.snapshot_dir, "seq_999999.hgt.tmp")
    with open(crash_tmp, "wb") as fh:
        fh.write(b"simulated crash residue")
    recovery = recover(args.snapshot_dir)

    result = {
        "terrain": args.terrain,
        "patches": args.patch_count,
        "color_buffer_byte_equal": bool(color_equal),
        "heat_buffer_byte_equal": bool(heat_equal),
        "normal_angle_max_deg": float(angles.max()),
        "normal_angle_mean_deg": float(angles.mean()),
        "blend_max_abs_diff": float(blend_diff.max()),
        "blend_pixel_equal": bool(pixel_equal),
        "render_deterministic": bool(deterministic),
        "snapshot_last_seq": latest_seq(args.snapshot_dir),
        "recovery_seq": recovery["last_seq"],
        "recovery_chain_intact": recovery["chain"] == chain,
        "crash_tmp_cleaned": not os.path.exists(crash_tmp),
        "color_hash": full_summary.color_hash,
        "heat_hash": full_summary.heat_hash,
    }
    out_path = os.path.join(args.render_dir, "verify_report.json")
    os.makedirs(args.render_dir, exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(result, fh, ensure_ascii=False, indent=2, sort_keys=True)
    full_renderer.color_qimage().save(os.path.join(args.render_dir, "color.png"))
    full_renderer.heat_qimage().save(os.path.join(args.render_dir, "heat.png"))

    print(json.dumps(result, ensure_ascii=False, indent=2))
    ok = (
        color_equal
        and heat_equal
        and angles.max() <= args.angle_tolerance
        and blend_diff.max() <= args.pixel_tolerance
        and deterministic
        and recovery["last_seq"] == args.patch_count
        and recovery["chain"] == chain
    )
    print("验收结论:", "通过 ✅" if ok else "失败 ❌")
    return 0 if ok else 1


def cmd_gui(args: argparse.Namespace) -> int:
    from .ui import run_gui

    return run_gui(args.sample_root, args.snapshot_dir)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="地形可视化增量更新工具")
    sub = parser.add_subparsers(dest="command", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--sample-root", default="artifacts/samples")
    common.add_argument("--snapshot-dir", default="artifacts/snapshots/default")
    common.add_argument("--render-dir", default="artifacts/render/default")
    common.add_argument("--chunk-size", type=int, default=16)
    common.add_argument("--patch-count", type=int, default=6)

    p_gen = sub.add_parser("generate", parents=[common])
    p_gen.add_argument("--terrain", choices=TERRAIN_TYPES, default="hills")
    p_gen.add_argument("--all", action="store_true")
    p_gen.set_defaults(func=cmd_generate)

    p_ver = sub.add_parser("verify", parents=[common])
    p_ver.add_argument("--terrain", choices=TERRAIN_TYPES, default="hills")
    p_ver.add_argument("--lod-threshold", type=float, default=1.0)
    p_ver.add_argument("--crack-tolerance", type=float, default=1e9)
    p_ver.add_argument("--angle-tolerance", type=float, default=0.5)
    p_ver.add_argument("--pixel-tolerance", type=float, default=1e-6)
    p_ver.set_defaults(func=cmd_verify)

    p_gui = sub.add_parser("gui", parents=[common])
    p_gui.set_defaults(func=cmd_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)
