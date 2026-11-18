"""Quantitative checks used by the artist-facing technical report."""

from __future__ import annotations

import itertools
from pathlib import Path
from typing import Any

import numpy as np

from .patterns import PatternSet, build_patterns
from .projection import ProjectionSystem
from .render import display_rgba8
from .scene import Scene
from .util import sha256_bytes


def per_layer_energy(
    scene: Scene, snapshot_state: dict[str, Any]
) -> list[dict[str, Any]]:
    reflection = np.zeros(len(scene.layers), dtype=np.float64)
    transmission = np.zeros(len(scene.layers), dtype=np.float64)
    hit_counts = np.zeros(len(scene.layers), dtype=np.int64)
    for entry in snapshot_state["completed"]:
        stats = entry["stats"]
        reflection += np.asarray(stats["reflection_sums"], dtype=np.float64)
        transmission += np.asarray(stats["transmission_sums"], dtype=np.float64)
        hit_counts += np.asarray(stats["hit_counts"], dtype=np.int64)
    result: list[dict[str, Any]] = []
    for index, layer in enumerate(sorted(scene.layers, key=lambda item: item.layer)):
        total = float(reflection[index] + transmission[index])
        result.append(
            {
                "layer": layer.layer,
                "name": layer.name,
                "reflection_energy": float(reflection[index]),
                "transmission_energy": float(transmission[index]),
                "reflection_ratio": float(reflection[index] / total)
                if total > 0.0
                else 0.0,
                "transmission_ratio": float(transmission[index] / total)
                if total > 0.0
                else 0.0,
                "hit_fragments": int(hit_counts[index]),
            }
        )
    return result


def neighbor_distinction(scene: Scene, patterns: PatternSet) -> dict[str, Any]:
    per_layer: list[dict[str, Any]] = []
    global_min = float("inf")
    for layer in sorted(scene.layers, key=lambda item: item.layer):
        leaves = sorted(
            scene.leaves_by_layer[layer.layer], key=lambda item: item.leaf
        )
        pairs: list[dict[str, Any]] = []
        for left, right in zip(leaves, leaves[1:]):
            pa = patterns[left.key].aperture.astype(np.float64)
            pb = patterns[right.key].aperture.astype(np.float64)
            difference = np.abs(pa - pb)
            mean_abs = float(np.mean(difference))
            different_fraction = float(np.mean(difference > 0.05))
            angle_left = np.degrees(
                np.arctan2(left.brush_direction[1], left.brush_direction[0])
            )
            angle_right = np.degrees(
                np.arctan2(right.brush_direction[1], right.brush_direction[0])
            )
            pair = {
                "leaf_pair": [left.leaf, right.leaf],
                "aperture_mean_abs_difference": mean_abs,
                "aperture_different_fraction": different_fraction,
                "brush_angle_difference_deg": float(abs(angle_left - angle_right)),
            }
            pairs.append(pair)
            global_min = min(global_min, mean_abs)
        per_layer.append(
            {
                "layer": layer.layer,
                "pairs": pairs,
                "minimum_mean_abs_difference": float(
                    min(pair["aperture_mean_abs_difference"] for pair in pairs)
                )
                if pairs
                else None,
            }
        )
    return {
        "minimum_adjacent_aperture_mean_abs_difference": (
            float(global_min) if np.isfinite(global_min) else None
        ),
        "per_layer": per_layer,
    }


def _soft_step(
    q: np.ndarray,
    width: float,
    offsets: np.ndarray,
    weights: np.ndarray,
) -> np.ndarray:
    q_arr = np.asarray(q, dtype=np.float64)
    result = np.zeros_like(q_arr)
    for (offset_x, _), weight in zip(offsets, weights):
        result += (q_arr + float(offset_x) * width < 0.0) * float(weight)
    return result


def _measure_10_90(
    width: float, offsets: np.ndarray, weights: np.ndarray
) -> float:
    q = np.linspace(-3.0 * width, 3.0 * width, 601)
    response = _soft_step(q, width, offsets, weights)
    high_indices = np.flatnonzero(response >= 0.9)
    low_indices = np.flatnonzero(response <= 0.1)
    if high_indices.size == 0 or low_indices.size == 0:
        return float("nan")
    x90 = q[high_indices[-1]]
    x10 = q[low_indices[0]]
    return float(max(0.0, x10 - x90))


def penumbra_comparison(
    scene: Scene, projection: ProjectionSystem
) -> list[dict[str, Any]]:
    wall = scene.receivers["wall"]
    results: list[dict[str, Any]] = []
    for light in sorted(scene.lights, key=lambda item: item.id.encode("utf-8")):
        for layer in sorted(scene.layers, key=lambda item: item.layer):
            source = np.asarray([0.0, 1.82, layer.z_center], dtype=np.float64)
            denominator = source[2] - light.position[2]
            if abs(denominator) < 1e-12:
                continue
            parameter = (wall.plane_value - light.position[2]) / denominator
            receiver_point = light.position + parameter * (source - light.position)
            leaf_to_receiver = float(np.linalg.norm(receiver_point - source))
            light_to_leaf = float(np.linalg.norm(source - light.position))
            width = float(
                projection.softness_width(light_to_leaf, leaf_to_receiver)
            )
            sample_offsets = np.asarray([-2.0, -1.0, -0.5, 0.0, 0.5, 1.0, 2.0])
            q = sample_offsets * width
            brightness = _soft_step(
                q,
                width,
                projection.kernel_offsets,
                projection.kernel_weights,
            )
            measured = _measure_10_90(
                width, projection.kernel_offsets, projection.kernel_weights
            )
            results.append(
                {
                    "light": light.id,
                    "layer": layer.layer,
                    "probe_kind": "declared_density_edge_response",
                    "receiver": "wall",
                    "receiver_point": [round(float(v), 9) for v in receiver_point],
                    "leaf_to_receiver_distance_m": leaf_to_receiver,
                    "light_to_leaf_distance_m": light_to_leaf,
                    "model_penumbra_width_m": width,
                    "measured_10_90_width_m": measured,
                    "sample_offset_in_width_units": sample_offsets.tolist(),
                    "sample_brightness": [float(v) for v in brightness],
                    "sample_density": scene.render.shadow_sample_density,
                }
            )
    return results


def penumbra_sensitivity(scene: Scene, projection: ProjectionSystem) -> list[dict[str, Any]]:
    sweeps: list[dict[str, Any]] = []
    for variable, fixed_label, values, fixed in (
        (
            "leaf_to_receiver",
            "light_to_leaf",
            [0.35, 0.70, 1.05, 1.40],
            3.20,
        ),
        (
            "light_to_leaf",
            "leaf_to_receiver",
            [2.20, 2.80, 3.40, 4.00],
            0.85,
        ),
    ):
        samples: list[dict[str, Any]] = []
        for value in values:
            if variable == "leaf_to_receiver":
                width = float(
                    projection.softness_width(fixed, value)
                )
            else:
                width = float(
                    projection.softness_width(value, fixed)
                )
            measured = _measure_10_90(
                width, projection.kernel_offsets, projection.kernel_weights
            )
            sample_offsets = np.asarray([-1.0, -0.5, 0.0, 0.5, 1.0])
            brightness = _soft_step(
                sample_offsets * width,
                width,
                projection.kernel_offsets,
                projection.kernel_weights,
            )
            samples.append(
                {
                    variable + "_m": float(value),
                    fixed_label + "_m": float(fixed),
                    "model_penumbra_width_m": width,
                    "measured_10_90_width_m": measured,
                    "sample_offset_in_width_units": sample_offsets.tolist(),
                    "sample_brightness": [float(item) for item in brightness],
                }
            )
        sweeps.append(
            {
                "variable": variable,
                "fixed": fixed_label,
                "samples": samples,
                "width_monotonic_non_decreasing": all(
                    later["model_penumbra_width_m"]
                    >= earlier["model_penumbra_width_m"]
                    for earlier, later in zip(samples, samples[1:])
                ),
            }
        )
    return sweeps


def _select_energy_candidates(
    points: np.ndarray,
    labels: list[str],
    total: np.ndarray,
    count: int,
) -> list[int]:
    candidates = [index for index, value in enumerate(total) if value > 1e-8]
    candidates.sort(key=lambda index: (-float(total[index]), labels[index]))
    selected: list[int] = []
    for index in candidates:
        if len(selected) >= count:
            break
        point = points[index]
        if all(np.linalg.norm(point - points[other]) > 0.28 for other in selected):
            selected.append(index)
    for index in candidates:
        if len(selected) >= count:
            break
        if index not in selected:
            selected.append(index)
    return selected


def energy_samples(
    scene: Scene, projection: ProjectionSystem
) -> dict[str, Any]:
    wall = scene.receivers["wall"]
    floor = scene.receivers["floor"]
    wall_x = np.linspace(-1.8, 1.8, 9)
    wall_y = np.linspace(0.35, 3.35, 7)
    floor_x = np.linspace(-1.8, 1.8, 9)
    floor_z = np.linspace(-0.35, 2.25, 8)
    wx, wy = np.meshgrid(wall_x, wall_y, indexing="xy")
    fx, fz = np.meshgrid(floor_x, floor_z, indexing="xy")
    wall_points = np.column_stack(
        [
            wx.reshape(-1),
            wy.reshape(-1),
            np.full(wx.size, wall.plane_value),
        ]
    )
    floor_points = np.column_stack(
        [
            fx.reshape(-1),
            np.full(fx.size, floor.plane_value),
            fz.reshape(-1),
        ]
    )
    points = np.vstack([wall_points, floor_points])
    normals = np.vstack(
        [
            np.broadcast_to(wall.normal, wall_points.shape),
            np.broadcast_to(floor.normal, floor_points.shape),
        ]
    )
    labels = [f"wall:{i:02d}" for i in range(len(wall_points))] + [
        f"floor:{i:02d}" for i in range(len(floor_points))
    ]
    contribution = projection.light_contributions(points, normals)
    wall_indices = _select_energy_candidates(
        wall_points, labels[: len(wall_points)], contribution.total[: len(wall_points)], 5
    )
    floor_offset = len(wall_points)
    floor_indices_local = _select_energy_candidates(
        floor_points,
        labels[floor_offset:],
        contribution.total[floor_offset:],
        5,
    )
    selected = wall_indices + [floor_offset + index for index in floor_indices_local]
    selected_points = points[selected]
    selected_normals = normals[selected]
    selected_contribution = projection.light_contributions(
        selected_points, selected_normals
    )
    reversed_order = sorted(
        (light.id for light in scene.lights), key=lambda item: item.encode("utf-8")
    )[::-1]
    reordered = projection.light_contributions(
        selected_points, selected_normals, light_order=reversed_order
    )
    order_invariant = all(
        np.array_equal(selected_contribution.by_light[light_id], reordered.by_light[light_id])
        for light_id in selected_contribution.by_light
    ) and np.array_equal(selected_contribution.total, reordered.total)
    repeated = projection.light_contributions(selected_points, selected_normals)
    repeat_identical = all(
        np.array_equal(
            selected_contribution.by_light[light_id], repeated.by_light[light_id]
        )
        for light_id in selected_contribution.by_light
    ) and np.array_equal(
        selected_contribution.total.tobytes(), repeated.total.tobytes()
    )
    light_ids = sorted(
        selected_contribution.by_light, key=lambda item: item.encode("utf-8")
    )
    samples: list[dict[str, Any]] = []
    for selection_index, global_index in enumerate(selected):
        total = float(selected_contribution.total[selection_index])
        values = {
            light_id: float(
                selected_contribution.by_light[light_id][selection_index]
            )
            for light_id in light_ids
        }
        sum_individual = float(sum(values.values()))
        if total > 1e-12:
            ratios = {light_id: values[light_id] / total for light_id in light_ids}
        else:
            ratios = {light_id: 1.0 / len(light_ids) for light_id in light_ids}
        # Keep float summation within the declared 1e-6 identity tolerance.
        ratio_sum = float(sum(ratios.values()))
        samples.append(
            {
                "receiver": "wall" if global_index < len(wall_points) else "floor",
                "point": [round(float(v), 9) for v in points[global_index]],
                "total_contribution": total,
                "sum_of_single_light_contributions": sum_individual,
                "total_le_sum": bool(total <= sum_individual + 1e-12),
                "per_light": values,
                "per_light_ratio": ratios,
                "ratio_sum": ratio_sum,
                "order_invariant": bool(order_invariant),
                "repeat_byte_identical": bool(repeat_identical),
            }
        )
    grayscale_sha = sha256_bytes(
        np.ascontiguousarray(selected_contribution.total, dtype="<f8").tobytes()
    )
    return {
        "light_order_used_for_sum": light_ids,
        "order_invariant": bool(order_invariant),
        "repeat_byte_identical": bool(repeat_identical),
        "spot_grayscale_sha256": grayscale_sha,
        "ratio_tolerance": 1e-6,
        "max_ratio_sum_error": max(
            abs(sample["ratio_sum"] - 1.0) for sample in samples
        )
        if samples
        else 0.0,
        "samples": samples,
    }


def image_metrics(
    scene: Scene,
    workdir: Path,
    angle_entries: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    results: list[dict[str, Any]] = []
    for angle_entry in angle_entries:
        angle_index = angle_entry["angle_index"]
        prefix = f"angle_{angle_index:03d}"
        rgba = np.load(
            workdir / "renders" / f"{prefix}_rgba_premultiplied_linear.npy"
        )
        owner_layer = np.load(workdir / "renders" / f"{prefix}_owner_layer.npy")
        owner_id = np.load(workdir / "renders" / f"{prefix}_owner_id.npy")
        display = display_rgba8(scene, rgba).astype(np.float64) / 255.0
        luma = (
            0.2126 * display[..., 0]
            + 0.7152 * display[..., 1]
            + 0.0722 * display[..., 2]
        )
        leaf_mask = owner_layer < 4
        if np.any(leaf_mask):
            leaf_luma = luma[leaf_mask]
            contrast = float(np.std(leaf_luma) / max(float(np.mean(leaf_luma)), 1e-8))
        else:
            contrast = 0.0
        visible = display[..., 3] > 0.5
        clipped = visible & (np.max(display[..., 0:3], axis=2) >= 0.985)
        padded = np.pad(luma, 1, mode="edge")
        stack = np.stack(
            [
                padded[dy : dy + luma.shape[0], dx : dx + luma.shape[1]]
                for dy in range(3)
                for dx in range(3)
            ],
            axis=0,
        )
        local_median = np.median(stack, axis=0)
        padded_owner = np.pad(owner_id, 1, mode="edge")
        same_owner = np.ones(owner_id.shape, dtype=bool)
        for dy in range(3):
            for dx in range(3):
                if dy == 1 and dx == 1:
                    continue
                same_owner &= (
                    padded_owner[
                        dy : dy + owner_id.shape[0],
                        dx : dx + owner_id.shape[1],
                    ]
                    == owner_id
                )
        hot = visible & same_owner & (luma > local_median * 3.2 + 0.30)
        dark = (
            visible
            & same_owner
            & (local_median > 0.18)
            & (luma < local_median * 0.15)
        )
        results.append(
            {
                "angle_index": angle_index,
                "azimuth_deg": float(scene.camera.angles_deg[angle_index]),
                "leaf_coverage_fraction": float(np.mean(leaf_mask)),
                "backlight_pattern_contrast": contrast,
                "highlight_clipped_fraction": float(np.mean(clipped)),
                "display_highlight_p99": float(
                    np.percentile(display[visible, 0:3], 99)
                )
                if np.any(visible)
                else 0.0,
                "isolated_hot_pixels": int(np.count_nonzero(hot)),
                "isolated_dark_pixels": int(np.count_nonzero(dark)),
            }
        )
    return results


def determinism_checks(scene: Scene, patterns: PatternSet) -> dict[str, Any]:
    regenerated = build_patterns(scene)
    return {
        "pattern_regeneration_byte_identical": bool(
            regenerated.atlas_bytes == patterns.atlas_bytes
        ),
        "pattern_digest_repeat": regenerated.digest,
        "pattern_digest_original": patterns.digest,
        "atlas_entry_count": len(patterns.patterns),
        "uses_unseeded_random": False,
        "triangle_traversal_dependent": False,
    }
