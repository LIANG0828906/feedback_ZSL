"""Stable, JSON-serializable verification and image reports."""

from __future__ import annotations

import hashlib
import itertools
import math
from typing import Any

import numpy as np

from .optics import caustic_point_contributions, pixel_world_size
from .render import BlockSpec, RenderContext, TileResult, camera_rays
from .scene import SceneBundle, sha256_bytes

_LUMINANCE = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)


def luminance(rgb: np.ndarray) -> np.ndarray:
    return np.asarray(rgb, dtype=np.float64) @ _LUMINANCE


def make_block_record(spec: BlockSpec, tile: TileResult) -> dict[str, Any]:
    mean = tile.rgba.reshape(-1, 4).astype(np.float64).mean(axis=0)
    thickness_sum = float(tile.thickness_sum)
    thickness_count = int(tile.thickness_count)
    return {
        "block_id": spec.block_id,
        "camera_id": spec.camera_id,
        "block_index": spec.block_index,
        "x": spec.x,
        "y": spec.y,
        "width": spec.width,
        "height": spec.height,
        "visible_face_count": int(tile.visible_face_count),
        "thickness_sum": thickness_sum,
        "thickness_count": thickness_count,
        "mean_thickness_nm": (
            thickness_sum / thickness_count if thickness_count else None
        ),
        "rgba_mean_premultiplied": [float(value) for value in mean],
        "rgba_sha256": sha256_bytes(tile.rgba.tobytes(order="C")),
        "face_id_sha256": sha256_bytes(tile.face_id.tobytes(order="C")),
    }


def caustic_report(context: RenderContext, primary_camera: dict[str, Any]) -> dict[str, Any]:
    pixel_size = pixel_world_size(primary_camera, context.leaf_y)
    leaf_penumbra = context.bundle.scene["leaf"]["penumbra"]
    lights: list[dict[str, Any]] = []
    probes: list[dict[str, Any]] = []
    trend_ok = True
    for source_id in sorted(context.caustics):
        source = context.caustics[source_id]
        valid = source.valid_indices
        valid_mask = source.landing_valid & (source.amplitude > 0.0)
        positive_mask = source.amplitude > 0.0
        out_of_bounds_count = int(np.count_nonzero(positive_mask & ~source.landing_valid))
        inactive_count = int(np.count_nonzero(~positive_mask))
        denominator = max(float(np.sum(source.amplitude[valid_mask])), 1.0e-12)
        records: list[dict[str, Any]] = []
        for index in valid:
            records.append(
                {
                    "sample_index": int(index),
                    "face_id": int(source.face_ids[index]),
                    "surface_position": [
                        float(value) for value in source.surface_points[index]
                    ],
                    "landing_point": [
                        float(value) for value in source.landing_points[index]
                    ],
                    "thickness_nm": float(source.thickness_nm[index]),
                    "amplitude": float(source.amplitude[index]),
                    "tint": [float(value) for value in source.tint[index]],
                    "penumbra_half_width": float(source.sigma_world[index]),
                    "penumbra_half_width_px": float(
                        source.sigma_world[index] / pixel_size
                    ),
                    "distance_leaf": float(source.distance_leaf[index]),
                    "distance_source": float(source.distance_source[index]),
                }
            )
        widths = source.sigma_world[valid]
        lights.append(
            {
                "source_id": source_id,
                "source_position": [
                    float(v)
                    for v in next(
                        light["position"]
                        for light in context.bundle.scene["lights"]
                        if light["id"] == source_id
                    )
                ],
                "surface_sample_count": int(source.face_ids.shape[0]),
                "landing_count": int(valid.size),
                "out_of_bounds_count": out_of_bounds_count,
                "inactive_sample_count": inactive_count,
                "noncontributing_count": int(source.face_ids.shape[0] - valid.size),
                "penumbra_width_min": float(np.min(widths)) if widths.size else 0.0,
                "penumbra_width_median": float(np.median(widths)) if widths.size else 0.0,
                "penumbra_width_max": float(np.max(widths)) if widths.size else 0.0,
                "samples": records,
            }
        )

        if valid.size:
            metric = (
                source.distance_leaf[valid]
                * source.distance_source[valid]
                / np.maximum(
                    source.distance_leaf[valid] + source.distance_source[valid],
                    1.0e-12,
                )
            )
            order = np.argsort(metric, kind="stable")
            chosen_local = [
                int(order[min(len(order) - 1, max(0, round((len(order) - 1) * q)))])
                for q in (0.1, 0.5, 0.9)
            ]
            chosen = valid[np.asarray(chosen_local, dtype=np.int64)]
            previous_width = -math.inf
            for index in chosen:
                width = float(source.sigma_world[index])
                if width + 1.0e-15 < previous_width:
                    trend_ok = False
                previous_width = width
                center_local = (
                    context.caustic_gain
                    * float(source.amplitude[index])
                    / denominator
                )
                probes.append(
                    {
                        "source_id": source_id,
                        "sample_index": int(index),
                        "face_id": int(source.face_ids[index]),
                        "landing_point": [
                            float(value) for value in source.landing_points[index]
                        ],
                        "distance_leaf": float(source.distance_leaf[index]),
                        "distance_source": float(source.distance_source[index]),
                        "distance_product_over_sum": float(
                            source.distance_leaf[index]
                            * source.distance_source[index]
                            / (source.distance_leaf[index] + source.distance_source[index])
                        ),
                        "penumbra_half_width": width,
                        "penumbra_half_width_px": width / pixel_size,
                        "center_local_brightness": center_local,
                        "half_width_local_brightness": center_local * math.exp(-0.5),
                    }
                )

    model = (
        "landing = source + t*(surface-source), "
        "t=(leaf_y-source_y)/(surface_y-source_y); "
        "half_width=base_width+distance_gain*"
        "(distance_leaf*distance_source)/(distance_leaf+distance_source); "
        "in-bounds only; Gaussian half-width kernel"
    )
    return {
        "projection_model": model,
        "penumbra_model": {
            "base_width": float(leaf_penumbra["base_width"]),
            "distance_gain": float(leaf_penumbra["distance_gain"]),
            "min_pixel_width": float(leaf_penumbra["min_pixel_width"]),
            "pixel_world_size_at_leaf": pixel_size,
        },
        "lights": lights,
        "penumbra_probes": probes,
        "penumbra_trend_pass": bool(trend_ok),
    }


def energy_report(context: RenderContext) -> dict[str, Any]:
    leaf = context.bundle.scene["leaf"]
    grid = leaf["report_grid"]
    points = np.asarray(
        [
            [float(x), context.leaf_y, float(z)]
            for x, z in itertools.product(grid["x"], grid["z"])
        ],
        dtype=np.float64,
    )
    contributions = caustic_point_contributions(
        context.caustics, points, context.caustic_gain
    )
    source_ids = sorted(contributions)
    brightness = {
        source_id: luminance(contributions[source_id]) for source_id in source_ids
    }
    samples: list[dict[str, Any]] = []
    inactive: list[list[float]] = []
    max_ratio_error = 0.0
    max_energy_excess = 0.0
    max_order_delta = 0.0
    multi_light_sample_count = 0
    for point_index, point in enumerate(points):
        per_source = [float(brightness[source_id][point_index]) for source_id in source_ids]
        combined_rgb = np.zeros((3,), dtype=np.float64)
        individual_total = 0.0
        for source_id in source_ids:
            combined_rgb += contributions[source_id][point_index]
            individual_total += brightness[source_id][point_index]
        total = float(luminance(combined_rgb))
        if total <= 1.0e-12:
            inactive.append([float(point[0]), float(point[2])])
            continue
        ratios = [value / total for value in per_source[:-1]]
        ratios.append(max(0.0, 1.0 - math.fsum(ratios)))
        if sum(1 for value in per_source if value > 1.0e-12) >= 2:
            multi_light_sample_count += 1
        ratio_error = abs(math.fsum(ratios) - 1.0)
        max_ratio_error = max(max_ratio_error, ratio_error)
        reverse_total = math.fsum(reversed(per_source))
        max_order_delta = max(max_order_delta, abs(total - reverse_total))
        excess = total - individual_total
        max_energy_excess = max(max_energy_excess, max(0.0, excess))
        samples.append(
            {
                "leaf_point": [float(point[0]), float(point[2])],
                "contributions": per_source,
                "ratios": ratios,
                "sum_brightness": total,
                "sum_of_individual": float(individual_total),
                "ratio_sum": math.fsum(ratios),
            }
        )

    primary_camera = context.bundle.scene["cameras"][0]
    yy, xx = np.mgrid[0 : int(primary_camera["height"]), 0 : int(primary_camera["width"])]
    origin, directions = camera_rays(
        primary_camera,
        xx.reshape(-1),
        yy.reshape(-1),
    )
    t_leaf = (context.leaf_y - origin[1]) / np.where(
        np.abs(directions[:, 1]) > 1.0e-12,
        directions[:, 1],
        -1.0e-12,
    )
    leaf_points = origin[None, :] + t_leaf[:, None] * directions
    bounds = context.leaf_bounds
    leaf_mask = (
        (t_leaf > 1.0e-7)
        & (leaf_points[:, 0] >= bounds[0])
        & (leaf_points[:, 0] <= bounds[1])
        & (leaf_points[:, 2] >= bounds[2])
        & (leaf_points[:, 2] <= bounds[3])
    )
    audit_points = leaf_points[leaf_mask]
    audit_contributions = caustic_point_contributions(
        context.caustics, audit_points, context.caustic_gain
    )
    if audit_points.shape[0]:
        combined_rgb = np.zeros((audit_points.shape[0], 3), dtype=np.float64)
        individual_luminance = np.zeros((audit_points.shape[0],), dtype=np.float64)
        for source_id in source_ids:
            combined_rgb += audit_contributions[source_id]
            individual_luminance += luminance(audit_contributions[source_id])
        combined_luminance = luminance(combined_rgb)
        pixel_excess = combined_luminance - individual_luminance
        max_pixel_excess = float(np.max(pixel_excess))
        max_energy_excess = max(max_energy_excess, max(0.0, max_pixel_excess))
    else:
        max_pixel_excess = 0.0
    return {
        "canonical_light_order": source_ids,
        "sample_points": samples,
        "inactive_points": inactive,
        "active_sample_count": len(samples),
        "inactive_sample_count": len(inactive),
        "multi_light_sample_count": multi_light_sample_count,
        "max_ratio_error": max_ratio_error,
        "max_combined_vs_sum_excess": max_energy_excess,
        "max_light_order_delta": max_order_delta,
        "pixel_audit": {
            "camera_id": str(primary_camera["id"]),
            "leaf_pixel_count": int(audit_points.shape[0]),
            "max_combined_vs_sum_excess": max_pixel_excess,
        },
        "pass": bool(
            max_ratio_error <= 1.0e-6
            and max_energy_excess <= 1.0e-12
            and max_order_delta <= 1.0e-12
        ),
    }


def combined_image_digest(images: dict[str, dict[str, np.ndarray]]) -> str:
    digest = hashlib.sha256()
    for camera_id in sorted(images):
        for key in ("rgba", "face_id"):
            array = np.ascontiguousarray(images[camera_id][key])
            digest.update(camera_id.encode("utf-8"))
            digest.update(key.encode("ascii"))
            digest.update(str(array.dtype).encode("ascii"))
            digest.update(",".join(str(value) for value in array.shape).encode("ascii"))
            digest.update(array.tobytes(order="C"))
    return digest.hexdigest()


def build_report(
    bundle: SceneBundle,
    context: RenderContext,
    specs: list[BlockSpec],
    tiles: dict[str, TileResult],
    block_records: dict[str, dict[str, Any]],
    images: dict[str, dict[str, np.ndarray]],
    file_hashes: dict[str, dict[str, str]],
    hue: dict[str, Any],
) -> dict[str, Any]:
    scene = bundle.scene
    block_list = [block_records[spec.block_id] for spec in specs]
    image_entries: list[dict[str, Any]] = []
    for camera in scene["cameras"]:
        camera_id = str(camera["id"])
        camera_blocks = [
            block_records[spec.block_id]
            for spec in specs
            if spec.camera_id == camera_id
        ]
        thickness_sum = math.fsum(record["thickness_sum"] for record in camera_blocks)
        thickness_count = sum(record["thickness_count"] for record in camera_blocks)
        visible_faces = np.unique(images[camera_id]["face_id"])
        visible_faces = visible_faces[visible_faces > 0]
        image_entries.append(
            {
                "camera_id": camera_id,
                "width": int(camera["width"]),
                "height": int(camera["height"]),
                "visible_face_count": int(visible_faces.size),
                "mean_thickness_nm": (
                    thickness_sum / thickness_count if thickness_count else None
                ),
                "rgba_npy_sha256": file_hashes[camera_id]["rgba_npy"],
                "rgba_png_sha256": file_hashes[camera_id]["rgba_png"],
                "face_id_npy_sha256": file_hashes[camera_id]["face_npy"],
                "face_id_png_sha256": file_hashes[camera_id]["face_png"],
            }
        )

    caustics = caustic_report(context, scene["cameras"][0])
    energy = energy_report(context)
    checks = {
        "hue_continuity": bool(hue["pass"]),
        "caustic_penumbra_trend": bool(caustics["penumbra_trend_pass"]),
        "caustic_energy": bool(energy["pass"]),
    }
    return {
        "schema": "elytron-render-report/v1",
        "seed": int(scene["seed"]),
        "scene_digest": bundle.scene_digest,
        "film_signature": bundle.film_signature,
        "block_size": [context.block_w, context.block_h],
        "image_summary": {
            "combined_rgba_face_sha256": combined_image_digest(images),
            "cameras": image_entries,
        },
        "blocks": block_list,
        "hue_continuity": hue,
        "caustics": caustics,
        "energy_consistency": energy,
        "checks": checks,
        "pass": all(checks.values()),
    }
