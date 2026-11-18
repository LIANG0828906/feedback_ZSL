from __future__ import annotations

from typing import Any, Iterable

import numpy as np

from .projection import ProjectionData, penumbra_stats
from .render import BlockResult
from .scene import Scene
from .util import format_percentage, stable_float


def _channel_stats(values: np.ndarray) -> dict[str, Any]:
    return {
        "min": int(np.min(values)),
        "max": int(np.max(values)),
        "mean": stable_float(float(np.mean(values)), 6),
    }


def build_stats(
    scene: Scene,
    rgba: np.ndarray,
    layer_map: np.ndarray,
    blocks: Iterable[BlockResult],
    projection: ProjectionData,
) -> dict[str, Any]:
    block_list = list(blocks)
    layer_count = len(scene.layers)
    transmission = [sum(int(block.stats["transmission_energy_ppm"][i]) for block in block_list) for i in range(layer_count)]
    reflection = [sum(int(block.stats["reflection_energy_ppm"][i]) for block in block_list) for i in range(layer_count)]
    layer_projection = [sum(int(block.stats["projection_energy_ppm"][i]) for block in block_list) for i in range(layer_count)]
    layer_samples = [sum(int(block.stats["layer_samples"][i]) for block in block_list) for i in range(layer_count)]
    screen_projection = sum(int(block.stats["screen_projection_energy_ppm"]) for block in block_list)

    contributions = []
    for layer in scene.layers:
        index = layer.layer_index
        transmitted_total = transmission[index] + layer_projection[index]
        reflected_total = reflection[index]
        denominator = transmitted_total + reflected_total
        contributions.append(
            {
                "layer_index": index,
                "sample_count": layer_samples[index],
                "transmission_energy_ppm": transmitted_total,
                "direct_transmission_energy_ppm": transmission[index],
                "layer_projection_energy_ppm": layer_projection[index],
                "reflection_energy_ppm": reflected_total,
                "transmission_ratio_percent": format_percentage(transmitted_total, denominator),
                "reflection_ratio_percent": format_percentage(reflected_total, denominator),
            }
        )

    rgb_premultiplied = rgba[..., :3]
    alpha = rgba[..., 3]
    covered = (layer_map != 255) & (alpha > 0)
    straight = np.zeros_like(rgb_premultiplied, dtype=np.float64)
    straight[alpha > 0] = (
        rgb_premultiplied[alpha > 0].astype(np.float64) / alpha[alpha > 0, None].astype(np.float64)
    )
    if bool(np.any(covered)):
        covered_rgb = straight[covered]
        maximum = np.max(covered_rgb, axis=1)
        minimum = np.min(covered_rgb, axis=1)
        saturation = np.where(maximum > 1e-9, (maximum - minimum) / np.maximum(maximum, 1e-9), 0.0)
        mean_saturation = stable_float(float(np.mean(saturation)), 6)
    else:
        mean_saturation = 0.0

    unique_layers, counts = np.unique(layer_map, return_counts=True)
    layer_pixels = {
        ("background" if int(layer) == 255 else str(int(layer))): int(count)
        for layer, count in zip(unique_layers, counts)
    }

    light_stats = []
    supersampling = scene.render.supersampling
    for light_index, light in enumerate(scene.lights):
        cases = [case for case in projection.cases if case.light_index == light_index]
        statuses: dict[str, int] = {}
        for case in cases:
            statuses[case.status] = statuses.get(case.status, 0) + 1
        coverage = projection.screen_light_coverage[light_index]
        if supersampling > 1:
            output_coverage = coverage.reshape(
                scene.height,
                supersampling,
                scene.width,
                supersampling,
            ).max(axis=(1, 3))
        else:
            output_coverage = coverage
        light_stats.append(
            {
                "light_id": light.light_id,
                "projection_cases": len(cases),
                "status_counts": {key: int(value) for key, value in sorted(statuses.items())},
                "screen_lit_output_pixels": int(np.count_nonzero(output_coverage >= 0.1)),
                "max_screen_coverage": stable_float(float(np.max(coverage)) if coverage.size else 0.0, 6),
            }
        )
    screen_cov = projection.screen_light_coverage
    active_lights = np.sum(screen_cov >= 0.1, axis=0)
    overlap_pixels = int(np.count_nonzero(active_lights >= 2))
    if supersampling > 1:
        active_output = active_lights.reshape(
            scene.height,
            supersampling,
            scene.width,
            supersampling,
        ).max(axis=(1, 3))
    else:
        active_output = active_lights

    return {
        "schema_version": 1,
        "scene_sha256": scene.digest,
        "stable": True,
        "image": {
            "width": scene.width,
            "height": scene.height,
            "premultiplied_alpha": True,
            "rgba_channels": {
                "r": _channel_stats(rgba[..., 0]),
                "g": _channel_stats(rgba[..., 1]),
                "b": _channel_stats(rgba[..., 2]),
                "a": _channel_stats(rgba[..., 3]),
            },
            "rgb_max": int(np.max(rgba[..., :3])),
            "clipped_rgb_pixels": int(np.count_nonzero(np.any(rgba[..., :3] >= 250, axis=2))),
            "fully_white_pixels": int(
                np.count_nonzero(np.all(rgba[..., :3] >= 250, axis=2) & (rgba[..., 3] == 255))
            ),
            "mean_covered_saturation": mean_saturation,
            "layer_pixel_counts": layer_pixels,
        },
        "layer_shading_contributions": contributions,
        "projection": {
            "total_cases": len(projection.cases),
            "in_bounds_cases": sum(1 for case in projection.cases if case.in_bounds),
            "out_of_bounds_cases": sum(
                1 for case in projection.cases if case.status == "outside_receiver"
            ),
            "screen_lit_output_pixels_any_light": int(np.count_nonzero(active_output >= 1)),
            "screen_multi_light_overlap_output_pixels": int(
                np.count_nonzero(active_output >= 2)
            ),
            "screen_multi_light_overlap_supersamples": overlap_pixels,
            "screen_projection_energy_ppm": screen_projection,
            "lights": light_stats,
            "penumbra": penumbra_stats(projection, scene),
        },
    }

