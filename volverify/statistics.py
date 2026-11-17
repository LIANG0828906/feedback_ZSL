from __future__ import annotations

from typing import Any, Callable

import numpy as np

from .renderer import ImageResult, render_image
from .scene import Scene


def _luminance(radiance: np.ndarray) -> np.ndarray:
    return (
        0.2126 * radiance[:, :, 0]
        + 0.7152 * radiance[:, :, 1]
        + 0.0722 * radiance[:, :, 2]
    )


def convergence_report(
    scene: Scene,
    should_stop: Callable[[], bool] | None = None,
) -> dict[str, Any]:
    steps: list[float] = []
    deviations: list[float] = []
    samples: list[int] = []
    luminances: list[np.ndarray] = []

    for level in range(scene.sampling.convergence_levels):
        step = scene.sampling.reference_step / (
            scene.sampling.refinement_factor**level
        )
        result = render_image(
            scene, step, should_stop=should_stop, keep_auxiliary=False
        )
        steps.append(float(step))
        samples.append(int(result.sample_count))
        luminances.append(_luminance(result.radiance))

    for level in range(1, len(luminances)):
        coarse = luminances[level - 1]
        fine = luminances[level]
        signal = fine > 1e-12
        if np.any(signal):
            deviation = float(
                np.mean(
                    np.abs(coarse[signal] - fine[signal]) / fine[signal]
                )
            )
        else:
            deviation = float(np.mean(np.abs(coarse - fine)))
        deviations.append(deviation)

    monotonic = all(
        current <= previous + 1e-12
        for previous, current in zip(deviations, deviations[1:])
    )
    final_deviation = deviations[-1] if deviations else float("inf")
    passed = bool(
        monotonic
        and final_deviation <= scene.sampling.convergence_tolerance
    )
    return {
        "reference_step": scene.sampling.reference_step,
        "refined_step": steps[1] if len(steps) > 1 else None,
        "refinement_factor": scene.sampling.refinement_factor,
        "levels": scene.sampling.convergence_levels,
        "steps": steps,
        "mean_relative_deviations": deviations,
        "samples_per_level": samples,
        "tolerance": scene.sampling.convergence_tolerance,
        "monotonic": bool(monotonic),
        "final_deviation": final_deviation,
        "passed": passed,
    }


def _sampling_interval(
    entry_t: float,
    exit_t: float,
    peak_density: float,
    peak_t: float,
    peak_index: np.ndarray,
    index_min: np.ndarray,
    index_max: np.ndarray,
) -> dict[str, Any] | None:
    if entry_t < 0.0 or exit_t < 0.0:
        return None
    payload: dict[str, Any] = {
        "t_enter": float(entry_t),
        "t_exit": float(exit_t),
        "voxel_grid_min": [int(value) for value in index_min],
        "voxel_grid_max": [int(value) for value in index_max],
        "peak_density": float(peak_density),
        "peak_t": float(peak_t),
        "peak_voxel": (
            [int(value) for value in peak_index]
            if np.all(peak_index >= 0)
            else None
        ),
    }
    return payload


def suspicious_pixels(
    scene: Scene, result: ImageResult
) -> dict[str, Any]:
    depth = result.depth.astype(np.float64)
    opacity = -np.expm1(-np.maximum(depth, 0.0))
    threshold = scene.sampling.suspicious_opacity_jump_threshold
    height, width = opacity.shape
    pairs: list[dict[str, Any]] = []
    unique_pixels: set[tuple[int, int]] = set()

    def interval_at(x: int, y: int) -> dict[str, Any] | None:
        return _sampling_interval(
            float(result.entry_t[y, x]),
            float(result.exit_t[y, x]),
            float(result.peak_density[y, x]),
            float(result.peak_t[y, x]),
            result.peak_index[y, x],
            result.index_min[y, x],
            result.index_max[y, x],
        )

    for y in range(height):
        for x in range(width):
            if x + 1 < width:
                delta = abs(float(opacity[y, x] - opacity[y, x + 1]))
                if delta > threshold:
                    unique_pixels.add((x, y))
                    unique_pixels.add((x + 1, y))
                    pairs.append(
                        {
                            "pixel_a": [x, y],
                            "pixel_b": [x + 1, y],
                            "direction": "right",
                            "opacity_a": float(opacity[y, x]),
                            "opacity_b": float(opacity[y, x + 1]),
                            "opacity_delta": delta,
                            "sampling_interval_a": interval_at(x, y),
                            "sampling_interval_b": interval_at(x + 1, y),
                        }
                    )
            if y + 1 < height:
                delta = abs(float(opacity[y, x] - opacity[y + 1, x]))
                if delta > threshold:
                    unique_pixels.add((x, y))
                    unique_pixels.add((x, y + 1))
                    pairs.append(
                        {
                            "pixel_a": [x, y],
                            "pixel_b": [x, y + 1],
                            "direction": "down",
                            "opacity_a": float(opacity[y, x]),
                            "opacity_b": float(opacity[y + 1, x]),
                            "opacity_delta": delta,
                            "sampling_interval_a": interval_at(x, y),
                            "sampling_interval_b": interval_at(x, y + 1),
                        }
                    )

    return {
        "threshold": threshold,
        "pair_count": len(pairs),
        "pixel_count": len(unique_pixels),
        "pairs": pairs,
    }


def contribution_statistics(
    scene: Scene,
    light_rgb: dict[str, np.ndarray],
    self_rgb: np.ndarray,
    decimals: int = 12,
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    entries: list[tuple[str, str, np.ndarray]] = []
    for light in scene.lights:
        entries.append(
            (light.id, light.kind, light_rgb[light.id])
        )
    light_energy = sum(
        float(np.sum(values)) for _, _, values in entries
    )
    self_energy = float(np.sum(self_rgb))
    total = light_energy + self_energy

    lights_payload = []
    for light_id, kind, values in entries:
        energy = float(np.sum(values))
        lights_payload.append(
            {
                "id": light_id,
                "type": kind,
                "rgb_sum": [round(float(item), decimals) for item in values],
                "fraction": (
                    round(energy / total, decimals) if total > 0.0 else 0.0
                ),
            }
        )
    self_payload = {
        "rgb_sum": [round(float(item), decimals) for item in self_rgb],
        "fraction": (
            round(self_energy / total, decimals) if total > 0.0 else 0.0
        ),
    }
    return lights_payload, self_payload

