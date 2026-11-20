from __future__ import annotations

import math
from typing import Any

import numpy as np

from .errors import E_BAND_INVALID, E_CONFIG_INVALID, fail
from .geometry import event_wall_map, material_signature
from .model import Scene, band_centers


def _finite_number(value: Any, name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError):
        fail(E_BAND_INVALID, f"{name} must be numeric", value=value)
    if not math.isfinite(number):
        fail(E_BAND_INVALID, f"{name} must be finite", value=value)
    return number


def validate_early_config(
    scene: Scene,
    frames: int,
    centers: list[float] | None = None,
) -> dict[str, Any]:
    config = dict(scene.early)
    start_ms = _finite_number(config.get("start_ms"), "start_ms")
    end_ms = _finite_number(config.get("end_ms"), "end_ms")
    threshold_ratio = _finite_number(
        config.get("threshold_ratio"), "threshold_ratio"
    )
    min_gap_ms = _finite_number(config.get("min_gap_ms"), "min_gap_ms")
    fft_size = int(_finite_number(config.get("fft_size"), "fft_size"))
    fraction = _finite_number(
        scene.bands.get("fraction", 1.0),
        "bands.fraction",
    )
    mode = str(scene.bands.get("mode", "octave")).lower()
    duration_ms = 1000.0 * frames / scene.sample_rate
    if start_ms < 0.0 or end_ms <= start_ms or end_ms > duration_ms:
        fail(
            E_BAND_INVALID,
            "early-reflections time window is outside the impulse response",
            start_ms=start_ms,
            end_ms=end_ms,
            duration_ms=duration_ms,
        )
    if not 0.0 < threshold_ratio <= 1.0:
        fail(
            E_BAND_INVALID,
            "threshold_ratio must be in the interval (0, 1]",
            threshold_ratio=threshold_ratio,
        )
    if min_gap_ms <= 0.0:
        fail(E_BAND_INVALID, "min_gap_ms must be positive", value=min_gap_ms)
    if mode != "octave":
        fail(E_BAND_INVALID, "only octave band mode is supported", mode=mode)
    if fraction <= 0.0:
        fail(E_BAND_INVALID, "octave fraction must be positive", value=fraction)
    if fft_size < 32 or fft_size > 65536 or fft_size & (fft_size - 1):
        fail(
            E_BAND_INVALID,
            "fft_size must be a power of two between 32 and 65536",
            fft_size=fft_size,
        )
    centers = centers if centers is not None else band_centers(scene)
    if not centers:
        fail(E_BAND_INVALID, "at least one band center frequency is required")
    if any(not math.isfinite(value) or value <= 0.0 for value in centers):
        fail(E_BAND_INVALID, "band center frequencies must be positive and finite")
    if any(value >= scene.sample_rate / 2.0 for value in centers):
        fail(
            E_BAND_INVALID,
            "band center frequency must be below Nyquist",
            centers=centers,
            nyquist=scene.sample_rate / 2.0,
        )
    if list(centers) != sorted(set(centers)):
        fail(E_BAND_INVALID, "band center frequencies must be strictly increasing")
    return {
        "start_ms": start_ms,
        "end_ms": end_ms,
        "threshold_ratio": threshold_ratio,
        "min_gap_ms": min_gap_ms,
        "fft_size": fft_size,
        "start_sample": int(round(start_ms * scene.sample_rate / 1000.0)),
        "end_sample": int(round(end_ms * scene.sample_rate / 1000.0)),
        "min_gap_samples": max(
            1, int(round(min_gap_ms * scene.sample_rate / 1000.0))
        ),
        "centers": list(centers),
        "fraction": fraction,
        "mode": mode,
    }


def band_edges(centers: list[float], fraction: float = 1.0) -> list[tuple[float, float]]:
    ratio = 2.0 ** (1.0 / (2.0 * max(1.0, fraction)))
    return [(center / ratio, center * ratio) for center in centers]


def event_band_energies(
    signal: np.ndarray,
    event_index: int,
    sample_rate: int,
    centers: list[float],
    fft_size: int,
    fraction: float = 1.0,
) -> list[float]:
    pre = min(event_index, fft_size // 4)
    start = event_index - pre
    segment = signal[start : start + fft_size]
    if segment.size < fft_size:
        segment = np.pad(segment, (0, fft_size - segment.size))
    window = np.hanning(fft_size)
    spectrum = np.fft.rfft(segment * window)
    power = np.abs(spectrum) ** 2
    window_energy = max(float(np.sum(window**2)), 1e-30)
    power = power / window_energy
    frequencies = np.fft.rfftfreq(fft_size, d=1.0 / sample_rate)
    values: list[float] = []
    for low, high in band_edges(centers, fraction):
        mask = (frequencies >= low) & (frequencies < high)
        values.append(float(np.sum(power[mask])) if np.any(mask) else 0.0)
    return values


def detect_events(
    segment: np.ndarray,
    scene: Scene,
    config: dict[str, Any],
) -> list[dict[str, Any]]:
    start = int(config["start_sample"])
    end = min(int(config["end_sample"]), segment.shape[1])
    gap = int(config["min_gap_samples"])
    fft_size = int(config["fft_size"])
    centers = list(config["centers"])
    events: list[dict[str, Any]] = []
    for channel_index, channel_name in enumerate(scene.channel_layout):
        channel = segment[channel_index]
        if end <= start:
            continue
        local = np.abs(channel[start:end])
        if local.size == 0:
            continue
        threshold = float(np.max(local)) * float(config["threshold_ratio"])
        if threshold <= 0.0:
            continue
        cursor = 0
        while cursor < local.size:
            stop = min(local.size, cursor + gap)
            local_index = cursor + int(np.argmax(local[cursor:stop]))
            if local[local_index] > threshold:
                absolute_index = start + local_index
                energies = event_band_energies(
                    channel,
                    absolute_index,
                    scene.sample_rate,
                    centers,
                    fft_size,
                    float(config["fraction"]),
                )
                events.append(
                    {
                        "event_id": (
                            f"{scene.scene_id}:{channel_name}:"
                            f"{absolute_index:08d}"
                        ),
                        "channel": channel_name,
                        "sample_index": absolute_index,
                        "time_ms": 1000.0 * absolute_index / scene.sample_rate,
                        "amplitude": float(channel[absolute_index]),
                        "band_energy": energies,
                        "band_energy_db": [
                            10.0 * math.log10(value + 1e-30) for value in energies
                        ],
                    }
                )
            cursor = local_index + gap
    for event in events:
        event["band_centers_hz"] = centers
    return events


def _material_alpha(
    scene: Scene,
    materials: dict[str, Any],
    wall_id: str,
    number_of_bands: int,
) -> list[float]:
    wall = scene.wall(wall_id)
    material = materials.get(wall.material)
    if material is None:
        fail(E_CONFIG_INVALID, f"unknown material {wall.material} on wall {wall.id}")
    absorption = (
        material.get("absorption", []) if isinstance(material, dict) else material
    )
    values = [float(value) for value in absorption]
    if not values:
        fail(E_CONFIG_INVALID, f"material {wall.material} has no absorption data")
    if len(values) < number_of_bands:
        values.extend([values[-1]] * (number_of_bands - len(values)))
    return [min(0.999, max(0.0, value)) for value in values[:number_of_bands]]


def _apply_material_corrections(
    segment: np.ndarray,
    scene: Scene,
    materials: dict[str, Any],
    config: dict[str, Any],
    preliminary: list[dict[str, Any]],
    listener_id: str,
) -> tuple[np.ndarray, list[dict[str, Any]]]:
    if not preliminary:
        return segment, []
    corrected = segment.copy()
    times = [float(event["time_ms"]) for event in preliminary]
    mapping = event_wall_map(scene, listener_id, times)
    corrections: list[dict[str, Any]] = []
    duration = 1000.0 * segment.shape[1] / scene.sample_rate
    for event_index, event in enumerate(preliminary):
        wall_id = mapping.get(event_index)
        if wall_id is None:
            event["wall_id"] = None
            continue
        alpha = _material_alpha(
            scene,
            materials,
            wall_id,
            len(config["centers"]),
        )
        band_gain = math.sqrt(
            max(0.02, float(np.mean([1.0 - value for value in alpha])))
        )
        channel_index = scene.channel_layout.index(str(event["channel"]))
        event_sample = int(event["sample_index"])
        left = max(0, event_sample - int(0.001 * scene.sample_rate))
        right = min(
            segment.shape[1],
            event_sample + int(0.006 * scene.sample_rate),
        )
        corrected[channel_index, left:right] *= band_gain
        event["wall_id"] = wall_id
        corrections.append(
            {
                "event_id": event["event_id"],
                "wall_id": wall_id,
                "material": scene.wall(wall_id).material,
                "gain": band_gain,
                "band_alpha": alpha,
            }
        )
    final_events = detect_events(corrected, scene, config)
    final_times = [float(event["time_ms"]) for event in final_events]
    final_mapping = event_wall_map(scene, listener_id, final_times)
    for index, event in enumerate(final_events):
        event["wall_id"] = final_mapping.get(index)
    return corrected, corrections


def extract_early_reflections(
    aligned: np.ndarray,
    scene: Scene,
    materials: dict[str, Any],
    listener_id: str,
) -> tuple[np.ndarray, list[dict[str, Any]], dict[str, Any]]:
    centers = band_centers(scene)
    config = validate_early_config(scene, aligned.shape[1], centers)
    end_sample = min(int(config["end_sample"]), aligned.shape[1])
    segment = np.ascontiguousarray(aligned[:, :end_sample])
    preliminary = detect_events(segment, scene, config)
    corrected, corrections = _apply_material_corrections(
        segment,
        scene,
        materials,
        config,
        preliminary,
        listener_id,
    )
    events = detect_events(corrected, scene, config)
    times = [float(event["time_ms"]) for event in events]
    mapping = event_wall_map(scene, listener_id, times)
    for index, event in enumerate(events):
        event["wall_id"] = mapping.get(index)
    material_state = material_signature(scene, materials, centers)
    metadata = {
        "config": config,
        "events": events,
        "material_corrections": corrections,
        "material_signature": material_state,
        "listener_id": listener_id,
        "preliminary_event_count": len(preliminary),
    }
    return corrected, events, metadata


def affected_reflection_events(
    events: list[dict[str, Any]],
    *,
    wall_id: str | None = None,
    all_events: bool = False,
) -> list[str]:
    if all_events:
        return [str(event["event_id"]) for event in events]
    if wall_id is None:
        return []
    return [
        str(event["event_id"])
        for event in events
        if event.get("wall_id") == wall_id
    ]

