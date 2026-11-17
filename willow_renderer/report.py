"""Stable statistics report generation."""

import hashlib
import json


def file_sha256(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def build_report(scene, scene_digest, proj_result, layer_stats, tiles,
                 output_digests, resumed_from, discarded_snapshot_reason,
                 elapsed_seconds):
    layers = sorted(scene["layers"], key=lambda item: item["plane_z"])
    per_layer = []
    for layer in layers:
        lid = layer["layer_id"]
        stats = layer_stats.get(lid, {"transmission": 0.0, "reflection": 0.0})
        tran = stats["transmission"]
        refl = stats["reflection"]
        total = tran + refl
        per_layer.append({
            "layer_id": lid,
            "transmission_contribution": tran,
            "reflection_contribution": refl,
            "transmission_ratio": (tran / total) if total > 0.0 else 0.0,
            "reflection_ratio": (refl / total) if total > 0.0 else 0.0,
        })

    return {
        "report_version": 1,
        "scene_digest": scene_digest,
        "image": dict(scene["image"]),
        "tile_size": scene["tile"]["size"],
        "tile_count": len(tiles),
        "resumed_from_tiles": resumed_from,
        "discarded_snapshot_reason": discarded_snapshot_reason,
        "elapsed_seconds": round(elapsed_seconds, 6),
        "outputs": output_digests,
        "per_layer_contributions": per_layer,
        "projection_landings": proj_result.landings,
        "penumbra_measurements": proj_result.penumbra_samples,
    }


def write_text_summary(report, path):
    lines = []
    lines.append("柳编斗拱灯箱软光栅渲染 · 稳定统计报告")
    lines.append("=" * 52)
    lines.append(f"场景摘要: {report['scene_digest']}")
    lines.append(f"图像: {report['image']['width']}x{report['image']['height']}  "
                 f"分块: {report['tile_size']}px  块数: {report['tile_count']}")
    lines.append(f"续算自块: {report['resumed_from_tiles'] or '无（全新渲染）'}")
    if report["discarded_snapshot_reason"]:
        lines.append(f"快照已丢弃: {report['discarded_snapshot_reason']}")
    lines.append("")
    lines.append("各层透射/反射贡献占比:")
    for entry in report["per_layer_contributions"]:
        lines.append(
            f"  层 {entry['layer_id']}: 透射 {entry['transmission_ratio']:.4f}  "
            f"反射 {entry['reflection_ratio']:.4f}")
    lines.append("")
    lines.append(f"投影落点数量: {len(report['projection_landings'])}")
    lines.append("半影采样（宽度随空气隙与灯距增大而变宽）:")
    for m in report["penumbra_measurements"][:12]:
        lines.append(
            f"  灯{m['lamp_id']} 孔{m['hole_id']} 层{m['from_layer']}→{m['receiver']}"
            f" 隙={m['air_gap']:.3f} 灯距={m['lamp_distance']:.3f}"
            f" 亮度={m['centroid_brightness']:.4f}"
            f" 边宽L={m['edge_width_left']:.4f} R={m['edge_width_right']:.4f}")
    lines.append("")
    lines.append("输出摘要:")
    for name, digest in report["outputs"].items():
        lines.append(f"  {name}: {digest}")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")


def dump_json(data, path):
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, ensure_ascii=False, indent=1, sort_keys=True)
        fh.write("\n")
