"""Artist-facing JSON and Markdown technical report."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .analysis import (
    determinism_checks,
    energy_samples,
    image_metrics,
    neighbor_distinction,
    penumbra_comparison,
    penumbra_sensitivity,
    per_layer_energy,
)
from .patterns import PatternSet
from .projection import ProjectionSystem
from .scene import Scene
from .util import atomic_write_json, atomic_write_bytes, object_digest, sha256_file


def build_report(
    scene: Scene,
    patterns: PatternSet,
    projection: ProjectionSystem,
    workdir: str | Path,
    snapshot_state: dict[str, Any],
    angle_entries: list[dict[str, Any]],
    landing_document: dict[str, Any],
    lightmap_document: dict[str, Any],
    run_meta: dict[str, Any],
) -> dict[str, Any]:
    workdir = Path(workdir)
    layer_energy = per_layer_energy(scene, snapshot_state)
    energy = energy_samples(scene, projection)
    penumbra = penumbra_comparison(scene, projection)
    sensitivity = penumbra_sensitivity(scene, projection)
    neighbors = neighbor_distinction(scene, patterns)
    metrics = image_metrics(scene, workdir, angle_entries)
    determinism = determinism_checks(scene, patterns)
    landing_summary = {
        "file": "landing_points.json",
        "count": landing_document["landing_count"],
        "digest": landing_document["digest"],
        "camera_dependent": False,
        "out_of_receiver_policy": landing_document["out_of_receiver_policy"],
    }
    report = {
        "format": "GLV-TECHNICAL-REPORT-V1",
        "scene": {
            "scene_id": scene.raw.get("scene_id"),
            "scene_digest": scene.digest,
            "pattern_digest": patterns.digest,
            "units": scene.raw.get("units", "metres"),
            "layers": len(scene.layers),
            "leaves": len(scene.leaves),
            "lights": len(scene.lights),
            "angles_deg": list(scene.camera.angles_deg),
            "resolution": [scene.camera.width, scene.camera.height],
            "tile_size": [scene.render.tile_width, scene.render.tile_height],
            "shadow_sample_density": scene.render.shadow_sample_density,
        },
        "run": run_meta,
        "layer_energy": layer_energy,
        "projection": {
            "landing_summary": landing_summary,
            "receiver_lightmaps": {
                "document": "diagnostics/lightmaps.json",
                "digest": lightmap_document["digest"],
                "camera_dependent": False,
                "maps": lightmap_document["maps"],
            },
            "softness_model": {
                "formula": "base + a * distance_leaf_to_receiver + b * distance_light_to_leaf",
                "base_m": scene.render.softness_base,
                "leaf_to_receiver_coefficient": scene.render.softness_per_leaf_receiver,
                "light_to_leaf_coefficient": scene.render.softness_per_light_leaf,
                "sample_density": scene.render.shadow_sample_density,
                "monotonic_non_decreasing_in_both_distances": True,
            },
            "penumbra_comparison": penumbra,
            "penumbra_sensitivity": sensitivity,
            "energy_samples": energy,
        },
        "neighbor_distinction": neighbors,
        "image_metrics": metrics,
        "determinism": determinism,
        "interlayer_coupling": {
            "formula": "0.72 + 0.28 * exp(-gap/(thickness+0.04)) * |dot(N,V)|",
            "gap_m": scene.render.interlayer_gap,
            "view_dependent_but_continuous": True,
            "per_fragment_randomness": False,
        },
        "snapshot": {
            "format": snapshot_state["format"],
            "status": snapshot_state["status"],
            "completed_blocks": snapshot_state["completed_blocks"],
            "expected": snapshot_state["expected"],
            "snapshot_checksum": snapshot_state["snapshot_checksum"],
        },
        "outputs": {
            "angle_entries": angle_entries,
            "manifest_digest": object_digest(angle_entries),
        },
    }
    report["report_payload_digest"] = object_digest(report)
    return report


def write_report(
    workdir: str | Path, report: dict[str, Any], landing_document: dict[str, Any]
) -> None:
    workdir = Path(workdir)
    atomic_write_json(workdir / "report.json", report)
    atomic_write_json(workdir / "landing_points.json", landing_document)
    manifest = {
        "format": "GLV-RENDER-MANIFEST-V1",
        "scene_digest": report["scene"]["scene_digest"],
        "pattern_digest": report["scene"]["pattern_digest"],
        "report_payload_digest": report["report_payload_digest"],
        "angles": report["outputs"]["angle_entries"],
        "landing_digest": landing_document["digest"],
        "receiver_lightmap_digest": report["projection"]["receiver_lightmaps"]["digest"],
    }
    manifest["manifest_payload_digest"] = object_digest(manifest)
    atomic_write_json(workdir / "manifest.json", manifest)
    markdown = render_markdown(report)
    atomic_write_bytes(workdir / "report.md", markdown.encode("utf-8"))


def render_markdown(report: dict[str, Any]) -> str:
    scene = report["scene"]
    lines: list[str] = []
    lines.append("# 铸铝百叶幕墙离屏渲染技术报告")
    lines.append("")
    lines.append("## 场景与确定性")
    lines.append("")
    lines.append(f"- 场景：`{scene['scene_id']}`，{scene['layers']} 层、{scene['leaves']} 片叶片、{scene['lights']} 盏轨道射灯。")
    lines.append(f"- 分辨率：{scene['resolution'][0]}×{scene['resolution'][1]}；分块 {scene['tile_size'][0]}×{scene['tile_size'][1]}；半影采样密度 {scene['shadow_sample_density']}。")
    lines.append(f"- 场景摘要：`{scene['scene_digest']}`。")
    lines.append(f"- 花窗/拉丝图集摘要：`{scene['pattern_digest']}`。")
    lines.append("- 花窗与拉丝表仅由 `(seed, layer, leaf)` 和声明分辨率生成，不含未播种随机数，不依赖三角形遍历顺序。")
    lines.append("- 线性浮点 RGBA 为预乘 Alpha；PNG 为显示空间预乘 Alpha。")
    lines.append("")
    lines.append("## 逐层反射与透射")
    lines.append("")
    lines.append("| 层 | 名称 | 反射能量 | 透射能量 | 反射占比 | 透射占比 | 命中片元 |")
    lines.append("|---:|---|---:|---:|---:|---:|---:|")
    for layer in report["layer_energy"]:
        lines.append(
            f"| {layer['layer']} | {layer['name']} | {layer['reflection_energy']:.6f} | "
            f"{layer['transmission_energy']:.6f} | {layer['reflection_ratio']:.6f} | "
            f"{layer['transmission_ratio']:.6f} | {layer['hit_fragments']} |"
        )
    lines.append("")
    lines.append("## 光斑、半影与能量")
    lines.append("")
    projection = report["projection"]
    lines.append(f"- 光斑落点：{projection['landing_summary']['count']} 条，摘要 `{projection['landing_summary']['digest']}`；落点不依赖相机角度。")
    lines.append(f"- 世界空间接收面光斑图：`diagnostics/wall_lightmap.png`、`diagnostics/floor_lightmap.png`；摘要 `{projection['receiver_lightmaps']['digest']}`。")
    lines.append("- 半影宽度模型：`base + a·d(leaf,receiver) + b·d(light,leaf)`，两个距离系数均为正。")
    lines.append("- 多灯叠加按灯 ID 的规范顺序累加，因此光源遍历顺序不改变位级结果；接收面总量不超过单灯贡献之和。")
    lines.append("")
    lines.append("| 灯 | 层 | d(leaf,receiver) m | d(light,leaf) m | 模型宽度 m | 实测10–90宽度 m |")
    lines.append("|---|---:|---:|---:|---:|---:|")
    for row in projection["penumbra_comparison"]:
        lines.append(
            f"| {row['light']} | {row['layer']} | "
            f"{row['leaf_to_receiver_distance_m']:.5f} | "
            f"{row['light_to_leaf_distance_m']:.5f} | "
            f"{row['model_penumbra_width_m']:.6f} | "
            f"{row['measured_10_90_width_m']:.6f} |"
        )
    lines.append("")
    lines.append("| 控制变量 | 变化量 | 固定量 | 模型宽度 m | 实测10–90宽度 m | 单调 |")
    lines.append("|---|---:|---:|---:|---:|---|")
    for sweep in projection["penumbra_sensitivity"]:
        for row in sweep["samples"]:
            changing = row[sweep["variable"] + "_m"]
            fixed_value = row[sweep["fixed"] + "_m"]
            lines.append(
                f"| {sweep['variable']} | {changing:.3f} m | "
                f"{sweep['fixed']} {fixed_value:.3f} m | "
                f"{row['model_penumbra_width_m']:.6f} | "
                f"{row['measured_10_90_width_m']:.6f} | "
                f"{sweep['width_monotonic_non_decreasing']} |"
            )
    lines.append("")
    lines.append("| 接收面 | 坐标 (x,y,z) | 总贡献 | 各灯占比 | 占比和 | 顺序无关 |")
    lines.append("|---|---|---:|---|---:|---|")
    for sample in projection["energy_samples"]["samples"]:
        ratios = ", ".join(
            f"{key}={value:.6f}"
            for key, value in sample["per_light_ratio"].items()
        )
        point = ", ".join(f"{value:.3f}" for value in sample["point"])
        lines.append(
            f"| {sample['receiver']} | ({point}) | "
            f"{sample['total_contribution']:.6f} | {ratios} | "
            f"{sample['ratio_sum']:.9f} | {sample['order_invariant']} |"
        )
    lines.append("")
    lines.append("## 图像质量与可辨识度")
    lines.append("")
    lines.append("| 角度 | 方位角 | 叶片覆盖率 | 背光花纹对比 | 高光截断占比 | 孤立亮点 | 孤立暗点 |")
    lines.append("|---:|---:|---:|---:|---:|---:|---:|")
    for metric in report["image_metrics"]:
        lines.append(
            f"| {metric['angle_index']} | {metric['azimuth_deg']:.1f}° | "
            f"{metric['leaf_coverage_fraction']:.4f} | "
            f"{metric['backlight_pattern_contrast']:.4f} | "
            f"{metric['highlight_clipped_fraction']:.6f} | "
            f"{metric['isolated_hot_pixels']} | {metric['isolated_dark_pixels']} |"
        )
    neighbor = report["neighbor_distinction"]
    lines.append("")
    lines.append(
        f"- 同层相邻叶片最小花窗平均绝对差：{neighbor['minimum_adjacent_aperture_mean_abs_difference']:.6f}；每片还使用独立拉丝方向与微表面表。"
    )
    determinism = report["determinism"]
    lines.append(
        f"- 花窗图集重复生成逐字节一致：{determinism['pattern_regeneration_byte_identical']}。"
    )
    lines.append(
        f"- 光斑灰度重复运行逐字节一致：{projection['energy_samples']['repeat_byte_identical']}。"
    )
    lines.append(
        f"- 层间耦合：`{report['interlayer_coupling']['formula']}`；无逐片元随机项，交叠处不会产生 z-fighting 闪烁。"
    )
    lines.append("")
    lines.append("## 快照与续算")
    lines.append("")
    snapshot = report["snapshot"]
    run = report["run"]
    lines.append(f"- 快照状态：`{snapshot['status']}`，已完成 {snapshot['completed_blocks']} / {snapshot['expected']['total_blocks']} 块。")
    lines.append(f"- 本次恢复：`{run.get('resumed', False)}`；复用已提交块：{run.get('reused_blocks', 0)}；重置原因：{run.get('reset_reason')}。")
    lines.append("- 每块完成后以原子整体替换方式提交 `snapshot.json`；恢复只读取快照列出的连续前缀块，不扫描或推断旧块偏移。")
    lines.append("")
    lines.append("## 输出")
    lines.append("")
    for angle in report["outputs"]["angle_entries"]:
        lines.append(
            f"- 角度 {angle['angle_index']} ({angle['azimuth_deg']:+.1f}°)："
            f"`{angle['files']['rgba_png']}`，归属图 `{angle['files']['owner_png']}`。"
        )
    lines.append("")
    return "\n".join(lines)
