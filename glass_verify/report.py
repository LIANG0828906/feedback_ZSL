from __future__ import annotations

import hashlib
import json
from typing import Any

import numpy as np

from .geometry import GeometryContext
from .raster import (
    RENDERER_VERSION,
    ChunkResult,
    chunk_count,
    empty_metrics,
    merge_metrics,
)
from .scene import Scene
from .texture import texture_digest


def stable_json_bytes(value: Any) -> bytes:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=False,
            indent=2,
        )
        + "\n"
    ).encode("utf-8")


def _raw_digest(array: np.ndarray, dtype: str) -> str:
    contiguous = np.ascontiguousarray(array, dtype=dtype)
    return "sha256:" + hashlib.sha256(contiguous.tobytes(order="C")).hexdigest()


def assemble_image(
    scene: Scene,
    results: dict[int, ChunkResult],
) -> tuple[np.ndarray, np.ndarray]:
    rgb = np.zeros((scene.camera.height, scene.camera.width, 3), dtype=np.uint8)
    labels = np.zeros((scene.camera.height, scene.camera.width), dtype=np.uint16)
    for index in sorted(results):
        result = results[index]
        x0, y0, width, height = result.x0, result.y0, result.width, result.height
        rgb[y0 : y0 + height, x0 : x0 + width] = result.rgb
        labels[y0 : y0 + height, x0 : x0 + width] = result.labels
    return rgb, labels


def attribution_document(scene: Scene, context: GeometryContext) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "width": scene.camera.width,
        "height": scene.camera.height,
        "dtype": "uint16",
        "byte_order": "big_endian_png",
        "background_value": 0,
        "lead_value": 1,
        "labels": context.label_info,
    }


def _pane_metrics(scene: Scene, aggregate: dict[str, Any]) -> list[dict[str, Any]]:
    panes: list[dict[str, Any]] = []
    for pane in scene.panes:
        key = f"{pane.sector_id}:{pane.pane_id}"
        hits = int(aggregate["pane_fragment_hits"].get(key, 0))
        transmittance_sum = float(aggregate["pane_transmittance_sum"].get(key, 0.0))
        transmission = float(aggregate["pane_transmission_energy"].get(key, 0.0))
        reflection = float(aggregate["pane_reflection_energy"].get(key, 0.0))
        total = transmission + reflection
        panes.append(
            {
                "sector_id": pane.sector_id,
                "pane_id": pane.pane_id,
                "transmission_color": [float(value) for value in pane.transmission_color],
                "declared_transmittance": float(pane.transmittance),
                "declared_glaze_reflectance": float(pane.glaze_reflectance),
                "lead_width": float(pane.lead_width),
                "lead_depth": float(pane.lead_depth),
                "fragment_hits": hits,
                "mean_visible_transmittance": float(transmittance_sum / hits) if hits else 0.0,
                "transmission_contribution": transmission,
                "reflection_contribution": reflection,
                "transmission_fraction": float(transmission / total) if total > 0.0 else 0.0,
                "reflection_fraction": float(reflection / total) if total > 0.0 else 0.0,
            }
        )
    return panes


def _sector_metrics(scene: Scene, panes: list[dict[str, Any]]) -> list[dict[str, Any]]:
    sectors: list[dict[str, Any]] = []
    for sector_id in scene.sector_ids:
        members = [pane for pane in panes if pane["sector_id"] == sector_id]
        hits = sum(pane["fragment_hits"] for pane in members)
        weighted_transmittance = sum(
            pane["mean_visible_transmittance"] * pane["fragment_hits"] for pane in members
        )
        transmission = sum(pane["transmission_contribution"] for pane in members)
        reflection = sum(pane["reflection_contribution"] for pane in members)
        total = transmission + reflection
        sectors.append(
            {
                "sector_id": sector_id,
                "pane_count": len(members),
                "visible_fragments": int(hits),
                "visible_pixels": 0,
                "mean_transmittance": float(weighted_transmittance / hits) if hits else 0.0,
                "transmission_contribution": transmission,
                "reflection_contribution": reflection,
                "transmission_fraction": float(transmission / total) if total > 0.0 else 0.0,
                "reflection_fraction": float(reflection / total) if total > 0.0 else 0.0,
            }
        )
    return sectors


def build_stats(
    scene: Scene,
    context: GeometryContext,
    results: dict[int, ChunkResult],
    rgb: np.ndarray,
    labels: np.ndarray,
    projection_hits: dict[str, Any],
    shadow_profiles: dict[str, Any],
    file_digests: dict[str, str],
    recovery_count: int,
    snapshot_status: str,
    snapshot_reason: str | None,
    recovery_resumed_chunks: int,
) -> dict[str, Any]:
    aggregate = empty_metrics(scene)
    for index in sorted(results):
        merge_metrics(aggregate, results[index].metrics)
    panes = _pane_metrics(scene, aggregate)
    sectors = _sector_metrics(scene, panes)
    for sector in sectors:
        sector["visible_pixels"] = int(aggregate["visible_pixels"].get(str(sector["sector_id"]), 0))
    columns, rows, total = chunk_count(scene)
    completed = sorted(results)
    shadow_profile_samples = int(shadow_profiles["total_profile_samples"])
    return {
        "schema_version": 1,
        "renderer_version": RENDERER_VERSION,
        "scene_digest": scene.digest,
        "image": {
            "width": scene.camera.width,
            "height": scene.camera.height,
            "rgb_pixels_sha256": _raw_digest(rgb, "uint8"),
            "attribution_u16_sha256": _raw_digest(labels, "<u2"),
            "rgb_png_sha256": file_digests["color.png"],
            "attribution_png_sha256": file_digests["attribution.png"],
            "projection_hits_sha256": file_digests["projection_hits.json"],
        },
        "chunks": {
            "size": list(scene.render.chunk_size),
            "grid": [columns, rows],
            "total": total,
            "completed": completed,
            "completed_count": len(completed),
            "complete": len(completed) == total,
        },
        "recovery": {
            "recovery_count": int(recovery_count),
            "resumed_chunk_count": int(recovery_resumed_chunks),
            "snapshot_status": snapshot_status,
            "snapshot_discarded_reason": snapshot_reason,
        },
        "texture": {
            "seed": scene.texture.seed,
            "table_size": scene.texture.table_size,
            "pane_table_count": len(scene.panes),
            "table_digest": texture_digest(context.textures),
        },
        "projection": {
            "wall_id": projection_hits["wall_id"],
            "hit_count": projection_hits["hit_count"],
            "shadow_profile_count": len(shadow_profiles["profiles"]),
            "samples_per_axis": shadow_profiles["samples_per_axis"],
            "samples_per_light_probe": shadow_profiles["samples_per_light_probe"],
            "profile_samples": shadow_profile_samples,
            "rendered_edge_probes": int(aggregate["shadow_edge_probes"]),
            "rendered_offset_samples": int(aggregate["shadow_offset_samples"]),
            "soft_shadow_sample_count": int(
                aggregate["shadow_offset_samples"] + shadow_profile_samples
            ),
        },
        "sectors": sectors,
        "panes": panes,
    }

