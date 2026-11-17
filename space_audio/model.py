"""Per-frame acoustic model: occlusion, transmission, reverb zones, binaural cues."""

from __future__ import annotations

import math

import numpy as np

from .acoustics import binaural_params
from .geometry import build_scene_geometry, evaluate_all


def _interpolate_path(scene: dict, frame_times: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    path = scene["path"]
    times = np.array([point["t"] for point in path], dtype=np.float64)
    positions = np.array([point["position"] for point in path], dtype=np.float64)
    yaws = np.unwrap(np.array([point["yaw"] for point in path], dtype=np.float64))
    positions_out = np.empty((frame_times.size, 3), dtype=np.float64)
    yaws_out = np.empty(frame_times.size, dtype=np.float64)
    for axis in range(3):
        positions_out[:, axis] = np.interp(frame_times, times, positions[:, axis])
    yaws_out = np.interp(frame_times, times, yaws)
    return positions_out, yaws_out


def _wrap_angle(angle: float) -> float:
    return (angle + math.pi) % (2.0 * math.pi) - math.pi


def build_frame_table(scene: dict) -> dict:
    """Compute every per-frame, per-source acoustic parameter once."""
    project = scene["project"]
    frame_rate = float(project["frame_rate"])
    duration = float(project["duration"])
    sample_rate = int(project["sample_rate"])
    distance_ref = float(project["distance_ref"])
    exponent = float(project["distance_gain_exponent"])
    diffraction_db_per_m = float(project["diffraction_db_per_m"])
    max_rt60_rate = float(project["max_rt60_rate_s"])
    crossfade_width = float(project.get("crossfade_width", 0.5))

    frame_count = int(round(duration * frame_rate)) + 1
    frame_times = np.arange(frame_count, dtype=np.float64) / frame_rate
    listener_positions, listener_yaws = _interpolate_path(scene, frame_times)

    layers = sorted(scene["layers"], key=lambda layer: layer["plane"])
    planes = np.array([layer["plane"] for layer in layers], dtype=np.float64)
    rt60_list = np.array([layer["reverb"]["rt60"] for layer in layers], dtype=np.float64)
    wet_list = np.array([layer["reverb"]["wet"] for layer in layers], dtype=np.float64)
    geometries = build_scene_geometry(layers, int(project["seed"]))

    # Raw zone blending and rt60 slew limiting (rate limit in rt60 seconds per second).
    raw_rt60 = np.empty(frame_count, dtype=np.float64)
    raw_wet = np.empty(frame_count, dtype=np.float64)
    transition = []
    zone_ids = []
    for index, position in enumerate(listener_positions):
        x = float(position[0])
        zone = int(np.searchsorted(planes, x, side="right"))
        zone = min(zone, len(layers) - 1)
        next_zone = min(zone + 1, len(layers) - 1)
        alpha = 0.0
        state = f"stable:{layers[zone]['id']}"
        if next_zone != zone and crossfade_width > 0.0:
            plane = planes[next_zone]
            alpha = float(np.clip((x - (plane - crossfade_width)) / (2.0 * crossfade_width), 0.0, 1.0))
            if 0.0 < alpha < 1.0:
                state = f"blend:{layers[zone]['id']}->{layers[next_zone]['id']}"
        raw_rt60[index] = (1.0 - alpha) * rt60_list[zone] + alpha * rt60_list[next_zone]
        raw_wet[index] = (1.0 - alpha) * wet_list[zone] + alpha * wet_list[next_zone]
        transition.append(state)
        zone_ids.append(layers[zone]["id"])

    dt = 1.0 / frame_rate
    max_step = max_rt60_rate * dt
    actual_rt60 = np.empty(frame_count, dtype=np.float64)
    actual_rt60[0] = raw_rt60[0]
    for index in range(1, frame_count):
        delta = raw_rt60[index] - actual_rt60[index - 1]
        actual_rt60[index] = actual_rt60[index - 1] + np.clip(delta, -max_step, max_step)

    table: dict = {
        "sample_rate": sample_rate,
        "frame_rate": frame_rate,
        "frame_count": frame_count,
        "frame_times": frame_times,
        "listener_positions": listener_positions,
        "listener_yaws": listener_yaws,
        "zones": zone_ids,
        "transition": transition,
        "raw_rt60": raw_rt60,
        "rt60": actual_rt60,
        "wet": raw_wet,
        "layers": layers,
        "layer_ids": [layer["id"] for layer in layers],
        "sources": {},
    }

    for source in scene["sources"]:
        source_position = np.array(source["position"], dtype=np.float64)
        source_gain_db = float(source["gain"])
        source_gain = 10.0 ** (source_gain_db / 20.0)
        count = frame_count
        arrays = {
            "distance": np.zeros(count),
            "azimuth": np.zeros(count),
            "azimuth_raw": np.zeros(count),
            "elevation": np.zeros(count),
            "occlusion_count": np.zeros(count, dtype=np.int64),
            "transmission": np.ones(count),
            "detour": np.zeros(count),
            "diffraction_db": np.zeros(count),
            "distance_gain": np.zeros(count),
            "direct_gain": np.zeros(count),
            "itd_s": np.zeros(count),
            "ild_db": np.zeros(count),
            "gain_left": np.zeros(count),
            "gain_right": np.zeros(count),
            "occluded_layers": [[] for _ in range(count)],
        }
        for frame in range(frame_count):
            listener = listener_positions[frame]
            relative = source_position - listener
            distance = float(np.linalg.norm(relative))
            arrays["distance"][frame] = distance
            azimuth = _wrap_angle(
                math.atan2(relative[1], relative[0]) - float(listener_yaws[frame])
            )
            horizontal = math.hypot(relative[0], relative[1])
            elevation = math.atan2(relative[2], horizontal)
            arrays["azimuth"][frame] = azimuth
            arrays["elevation"][frame] = elevation

            results = evaluate_all(geometries, source_position, listener)
            transmission = 1.0
            detour = 0.0
            occluded = []
            # Multiplication is commutative, so source/result ordering cannot affect output.
            for layer, result in zip(layers, results):
                if result["occluded"]:
                    transmission *= float(layer["transmission"])
                    detour += float(result["detour"])
                    occluded.append(layer["id"])
            diffraction_db = -diffraction_db_per_m * detour
            diffraction_gain = 10.0 ** (diffraction_db / 20.0)
            distance_gain = (distance_ref / max(distance, 1e-9)) ** exponent
            direct_gain = source_gain * transmission * diffraction_gain * distance_gain
            arrays["occlusion_count"][frame] = len(occluded)
            arrays["transmission"][frame] = transmission
            arrays["detour"][frame] = detour
            arrays["diffraction_db"][frame] = diffraction_db
            arrays["distance_gain"][frame] = distance_gain
            arrays["direct_gain"][frame] = direct_gain
            arrays["occluded_layers"][frame] = occluded

            arrays["azimuth_raw"][frame] = azimuth
            arrays["azimuth"][frame] = azimuth
            arrays["elevation"][frame] = elevation
        # Unwrap azimuth across frames so a listener turning through +/- pi does
        # not produce a sample-level jump in ITD/ILD. The reported raw azimuth
        # stays wrapped; the binaural cues consume the continuous branch.
        arrays["azimuth"] = np.unwrap(arrays["azimuth_raw"])
        for frame in range(frame_count):
            continuous_azimuth = float(arrays["azimuth"][frame])
            elevation = float(arrays["elevation"][frame])
            itd, ild, gain_left, gain_right = binaural_params(continuous_azimuth, elevation)
            arrays["itd_s"][frame] = itd
            arrays["ild_db"][frame] = ild
            arrays["gain_left"][frame] = gain_left
            arrays["gain_right"][frame] = gain_right
        table["sources"][source["id"]] = arrays
    return table


def resample_frames_to_samples(frame_values: np.ndarray, sample_count: int,
                               frame_rate: float, sample_rate: int) -> np.ndarray:
    """Linearly resample a frame-rate control curve onto the audio sample grid."""
    sample_times = np.arange(sample_count, dtype=np.float64) / sample_rate
    frame_times = np.arange(frame_values.size, dtype=np.float64) / frame_rate
    return np.interp(sample_times, frame_times, frame_values)

