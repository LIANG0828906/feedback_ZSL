"""Stable, human- and machine-readable render reports."""

from __future__ import annotations

import hashlib
from typing import Any

import numpy as np

from .scene import Scene
from .shadow import ShadowResult
from .snapshot import summaries_for_completed, SnapshotState
from .texture import TextureBank


def _piece_rgb_mean(rgb: np.ndarray, owner: np.ndarray, serial: int) -> list[float] | None:
    mask = owner == serial
    count = int(mask.sum())
    if count == 0:
        return None
    means = rgb[mask].mean(axis=0)
    return [round(float(value), 6) for value in means]


def _probe(
    scene: Scene,
    rgb: np.ndarray,
    owner: np.ndarray,
    transmission_sums: np.ndarray,
    reflection_sums: np.ndarray,
    pixel_counts: np.ndarray,
    score,
) -> dict[str, Any] | None:
    candidates: list[tuple[float, int]] = []
    for piece in scene.pieces:
        count = int(pixel_counts[piece.serial - 1])
        if count == 0:
            continue
        mean = _piece_rgb_mean(rgb, owner, piece.serial)
        if mean is None:
            continue
        candidates.append((float(score(mean)), piece.serial))
    if not candidates:
        return None
    candidates.sort(key=lambda item: (item[0], -item[1]), reverse=True)
    serial = candidates[0][1]
    piece = scene.pieces[serial - 1]
    index = serial - 1
    trans = float(transmission_sums[index])
    refl = float(reflection_sums[index])
    total = trans + refl
    mean = _piece_rgb_mean(rgb, owner, serial)
    assert mean is not None
    return {
        "serial": serial,
        "quadrant": piece.quadrant,
        "piece_index": piece.piece_index,
        "mean_rgb_0_255": mean,
        "transmission_energy": round(trans, 8),
        "reflection_energy": round(refl, 8),
        "transmission_share": round(trans / total, 8) if total > 0.0 else 0.0,
        "reflection_share": round(refl / total, 8) if total > 0.0 else 0.0,
        "cosine_incidence": round(
            abs(float(np.dot(piece.normal, scene.light.direction))), 8
        ),
        "pixel_count": int(pixel_counts[index]),
    }


def build_report(
    scene: Scene,
    texture_bank: TextureBank,
    shadow: ShadowResult,
    state: SnapshotState,
) -> dict[str, Any]:
    summaries = summaries_for_completed(scene, state)
    completed_summaries = [
        summaries[chunk_id] for chunk_id in sorted(summaries)
    ]
    piece_entries: list[dict[str, Any]] = []
    for piece in scene.pieces:
        index = piece.serial - 1
        trans = float(state.transmission_sums[index])
        refl = float(state.reflection_sums[index])
        total = trans + refl
        piece_entries.append(
            {
                "serial": piece.serial,
                "quadrant": piece.quadrant,
                "piece_index": piece.piece_index,
                "pixels": int(state.pixel_counts[index]),
                "mean_rgb_0_255": _piece_rgb_mean(
                    state.rgb, state.owner, piece.serial
                ),
                "transmission_energy": round(trans, 8),
                "reflection_energy": round(refl, 8),
                "transmission_share": round(trans / total, 8) if total > 0 else 0.0,
                "reflection_share": round(refl / total, 8) if total > 0 else 0.0,
                "mean_transmission_contribution_per_channel": (
                    round(trans / (state.pixel_counts[index] * 3), 8)
                    if state.pixel_counts[index] > 0
                    else 0.0
                ),
                "mean_reflection_contribution_per_channel": (
                    round(refl / (state.pixel_counts[index] * 3), 8)
                    if state.pixel_counts[index] > 0
                    else 0.0
                ),
                "cosine_incidence": round(
                    abs(float(np.dot(piece.normal, scene.light.direction))), 8
                ),
                "transmittance": piece.transmittance,
                "enamel_reflectance": piece.enamel_reflectance,
                "lead_width": piece.lead_width,
                "lead_depth": piece.lead_depth,
            }
        )

    forward_probe = _probe(
        scene,
        state.rgb,
        state.owner,
        state.transmission_sums,
        state.reflection_sums,
        state.pixel_counts,
        lambda mean: mean[2] - mean[0],
    )
    lateral_probe = _probe(
        scene,
        state.rgb,
        state.owner,
        state.transmission_sums,
        state.reflection_sums,
        state.pixel_counts,
        lambda mean: mean[0] - mean[2],
    )
    forward_ok = bool(
        forward_probe
        and forward_probe["mean_rgb_0_255"][2]
        > 1.2 * forward_probe["mean_rgb_0_255"][0]
        and forward_probe["mean_rgb_0_255"][2]
        > 1.08 * forward_probe["mean_rgb_0_255"][1]
    )
    lateral_ok = bool(
        lateral_probe
        and lateral_probe["mean_rgb_0_255"][0]
        > 1.2 * lateral_probe["mean_rgb_0_255"][1]
        and lateral_probe["mean_rgb_0_255"][0]
        > 1.2 * lateral_probe["mean_rgb_0_255"][2]
        and max(lateral_probe["mean_rgb_0_255"]) >= 24.0
    )

    texture_digests = texture_bank.digests()
    texture_digest_union = hashlib.sha256(
        "".join(
            f"{serial}:{digest};"
            for serial, digest in sorted(texture_digests.items())
        ).encode("ascii")
    ).hexdigest()
    report: dict[str, Any] = {
        "schema": "rosewindow-report-v1",
        "scene": {
            "name": scene.name,
            "digest": scene.digest,
            "seed": scene.seed,
        },
        "validation": {
            "status": "ok",
            "pieces": len(scene.pieces),
            "quadrants": sorted({piece.quadrant for piece in scene.pieces}),
        },
        "image": {
            "width": scene.width,
            "height": scene.height,
            "chunk_size": scene.chunk_size,
            "chunks_x": scene.chunks_x,
            "chunks_y": scene.chunks_y,
            "chunk_count": scene.chunk_count,
            "completed_chunks": len(state.completed_chunks),
            "gamma": scene.render.gamma,
            "rgb_min_0_255": int(state.rgb.min()),
            "rgb_max_0_255": int(state.rgb.max()),
            "channels_within_uint8": True,
            "max_pre_tonemap_linear_value": round(state.max_linear_value, 8),
        },
        "texture": {
            "seed": scene.seed,
            "size": scene.textures.size,
            "order_independent": True,
            "generator": "splitmix64-value-noise-v1",
            "unique_digest_count": len(set(texture_digests.values())),
            "digest_union_sha256": texture_digest_union,
            "piece_digests": texture_digests,
        },
        "transmission": {
            "composition": "transmission_color * transmittance * seeded_texture * "
            "(ambient + directional_cosine)",
            "tone_mapping": "1 - exp(-linear * exposure), then gamma",
            "forward_blue_probe": forward_probe,
            "lateral_red_probe": lateral_probe,
            "forward_blue_not_pink": forward_ok,
            "lateral_red_hue_preserved": lateral_ok,
        },
        "reflection": {
            "model": "Schlick Fresnel on enamel reflectance, modulated by seeded texture",
            "composition": "out = transmission * (1 - F) + environment * F",
            "environment_color": [round(float(value), 8) for value in scene.reflection.environment_color],
            "environment_intensity": scene.reflection.intensity,
            "fresnel_power": scene.reflection.fresnel_power,
            "piece_contributions": piece_entries,
            "reflection_share_range": [
                min(
                    (
                        entry["reflection_share"]
                        for entry in piece_entries
                        if entry["pixels"] > 0
                    ),
                    default=0.0,
                ),
                max(
                    (
                        entry["reflection_share"]
                        for entry in piece_entries
                        if entry["pixels"] > 0
                    ),
                    default=0.0,
                ),
            ],
        },
        "lead_shadow": {
            "wall_plane_z": scene.wall.z,
            "light_direction": [round(float(value), 8) for value in scene.light.direction],
            "lead_edge_count": len(shadow.projections),
            "projection_file": "shadow_projections.json",
            "samples_per_transition": scene.shadows.samples,
            "softness_model": "depth_aware_penumbra_with_deterministic_stratified_samples",
            "coverage_digest": shadow.digest,
            "profile": shadow.profile,
        },
        "chunks": {
            "summaries": completed_summaries,
            "summary_digest": hashlib.sha256(
                "".join(
                    f"{entry['chunk_id']}:{entry['rgb_sha256']}:{entry['owner_sha256']};"
                    for entry in completed_summaries
                ).encode("ascii")
            ).hexdigest(),
        },
        "ownership": {
            "value_zero": "non-glass",
            "glass_serials": "stable 1-based order by (quadrant, piece_index)",
            "map_file": "ownership.npy",
            "mapping_file": "ownership_map.json",
        },
    }
    return report


def build_projection_document(scene: Scene, shadow: ShadowResult) -> dict[str, Any]:
    return {
        "schema": "rosewindow-shadow-projections-v1",
        "scene_digest": scene.digest,
        "wall": {
            "z": scene.wall.z,
            "width": scene.wall.width,
            "height": scene.wall.height,
            "center_xy": [scene.wall.center_x, scene.wall.center_y],
        },
        "light_direction": [round(float(value), 8) for value in scene.light.direction],
        "count": len(shadow.projections),
        "landings": list(shadow.projections),
    }
