"""Sampled grout-band brightness and penumbra width trend reporting."""

from __future__ import annotations

import numpy as np

from .geometry import penumbra_width, project_to_plane
from .io_utils import write_json
from .model import Scene


def band_trend_samples(scene: Scene) -> list[dict]:
    """Evaluate brightness/penumbra samples spanning layer and light ranges.

    Samples deliberately cover increasing layer-to-receiver and
    light-to-brick distances so the monotonic widening is directly visible.
    """
    records = []
    receiver = scene.receiver
    for layer in scene.layers:
        gap_width = max(layer.grout_width, 1e-3)
        gap_z = layer.plane_z + 0.05
        source = np.array([0.0, 0.0, gap_z])
        for light in scene.lights:
            landing = project_to_plane(source, light, receiver.z)
            if landing is None:
                continue
            in_bounds = receiver.contains(landing)
            width = penumbra_width(light, gap_z, receiver.z, gap_width)
            distance_light_gap = abs(gap_z - light.position[2])
            distance_gap_receiver = abs(receiver.z - gap_z)
            # Deterministic analytic band profile at fixed sample density.
            offsets = np.linspace(-2.5 * width, 2.5 * width, 11)
            profile = np.exp(-0.5 * (offsets / max(width / 2.0, 1e-9)) ** 2)
            brightness = [round(float(v), 6) for v in profile]
            records.append(
                {
                    "light_id": light.light_id,
                    "layer": layer.index,
                    "landing_xy": [round(float(landing[0]), 6),
                                   round(float(landing[1]), 6)],
                    "in_bounds": bool(in_bounds),
                    "light_to_gap_distance": round(float(distance_light_gap), 6),
                    "gap_to_receiver_distance": round(float(distance_gap_receiver), 6),
                    "penumbra_width": round(float(width), 6),
                    "profile_offsets": [round(float(v), 6) for v in offsets],
                    "band_brightness": brightness,
                }
            )
    return records


def verify_monotonic_trend(records: list[dict]) -> dict:
    """Check penumbra width widens with both driving distances."""
    result = {"monotonic": True, "violations": []}
    # Fix the light and compare across layers (gap-to-receiver distance).
    per_light: dict[int, list[dict]] = {}
    for record in records:
        per_light.setdefault(record["light_id"], []).append(record)
    for light_id, entries in per_light.items():
        ordered = sorted(entries, key=lambda item: item["gap_to_receiver_distance"])
        widths = [item["penumbra_width"] for item in ordered]
        if any(widths[i] >= widths[i + 1] for i in range(len(widths) - 1)):
            result["monotonic"] = False
            result["violations"].append(
                {"light_id": light_id, "widths": widths,
                 "reason": "width did not widen with layer-to-receiver distance"}
            )
    # Fix the layer/gap and compare across lights (light-to-gap distance).
    per_layer: dict[int, list[dict]] = {}
    for record in records:
        per_layer.setdefault(record["layer"], []).append(record)
    for layer, entries in per_layer.items():
        ordered = sorted(entries, key=lambda item: item["light_to_gap_distance"])
        widths = [item["penumbra_width"] for item in ordered]
        if any(widths[i] >= widths[i + 1] for i in range(len(widths) - 1)):
            result["monotonic"] = False
            result["violations"].append(
                {
                    "reason": "width did not widen with light-to-brick distance",
                    "layer": layer,
                    "widths": widths,
                }
            )
    return result


def write_band_report(scene: Scene, output_dir: str) -> dict:
    records = band_trend_samples(scene)
    trend = verify_monotonic_trend(records)
    payload = {
        "sample_density": scene.config.sample_density,
        "samples": records,
        "trend_check": trend,
    }
    write_json(f"{output_dir}/band_trend.json", payload)
    return payload
