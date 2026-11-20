from __future__ import annotations

import math
from typing import Any

import numpy as np

from .errors import E_BAND_INVALID, fail
from .model import Scene, band_centers


def _fallback_curve(index: int, initial_energy: float = 0.0) -> dict[str, Any]:
    return {
        "band_index": index,
        "slope_log10_per_s": -2.0,
        "intercept_log10": 0.0,
        "t60_s": 0.8,
        "r2": 0.0,
        "valid": False,
        "initial_energy": float(initial_energy),
        "sample_count": 0,
    }


def fit_decay_curve(
    times_s: list[float],
    energies: list[float],
    index: int,
    min_t60: float,
    max_t60: float,
) -> dict[str, Any]:
    initial_energy = float(
        sum(
            float(value)
            for value in energies
            if math.isfinite(float(value)) and float(value) > 0.0
        )
    )
    if len(times_s) < 2:
        return _fallback_curve(index, initial_energy)
    t = np.asarray(times_s, dtype=np.float64)
    e = np.asarray(energies, dtype=np.float64)
    mask = np.isfinite(t) & np.isfinite(e) & (e > 1e-30)
    if int(np.sum(mask)) < 2:
        return _fallback_curve(index, initial_energy)
    t = t[mask]
    e = e[mask]
    baseline = max(float(np.max(e)), 1e-30)
    y = np.log10(e / baseline)
    design = np.column_stack([t, np.ones_like(t)])
    try:
        slope, intercept = np.linalg.lstsq(design, y, rcond=None)[0]
    except np.linalg.LinAlgError:
        return _fallback_curve(index, initial_energy)
    if not math.isfinite(float(slope)) or float(slope) >= -1e-9:
        return _fallback_curve(index, initial_energy)
    fitted = design @ np.asarray([slope, intercept])
    residual = float(np.sum((y - fitted) ** 2))
    total = float(np.sum((y - np.mean(y)) ** 2))
    r2 = 0.0 if total <= 1e-30 else max(0.0, 1.0 - residual / total)
    t60 = -60.0 / (10.0 * float(slope))
    t60 = min(max(t60, min_t60), max_t60)
    return {
        "band_index": index,
        "slope_log10_per_s": float(slope),
        "intercept_log10": float(intercept),
        "t60_s": float(t60),
        "r2": float(r2),
        "valid": True,
        "initial_energy": float(np.sum(e)),
        "sample_count": int(np.sum(mask)),
    }


def model_band_decay(
    events: list[dict[str, Any]],
    scene: Scene,
    listener_id: str,
) -> dict[str, Any]:
    centers = band_centers(scene)
    if not centers:
        fail(E_BAND_INVALID, "cannot model decay without frequency bands")
    if not events:
        fail(E_BAND_INVALID, "no early reflection events are available for decay")
    min_t60 = float(scene.late.get("min_t60_s", 0.08))
    max_t60 = float(scene.late.get("max_t60_s", 8.0))
    if min_t60 <= 0.0 or max_t60 <= min_t60:
        fail(E_BAND_INVALID, "invalid late/decay T60 limits")
    duration_ms = max(
        float(scene.early.get("end_ms", 80.0)),
        float(scene.early.get("start_ms", 1.0)) + 1.0,
    )
    times_all = [
        float(event["sample_index"]) / scene.sample_rate for event in events
    ]

    bands: list[dict[str, Any]] = []
    for band_index, center in enumerate(centers):
        energies = [
            float(event["band_energy"][band_index]) for event in events
        ]
        curve = fit_decay_curve(times_all, energies, band_index, min_t60, max_t60)
        curve["center_hz"] = center
        bands.append(curve)

    channels: dict[str, list[dict[str, Any]]] = {}
    for channel_name in scene.channel_layout:
        channel_events = [
            event for event in events if event["channel"] == channel_name
        ]
        channel_curves = []
        for band_index, center in enumerate(centers):
            times = [
                float(event["sample_index"]) / scene.sample_rate
                for event in channel_events
            ]
            energies = [
                float(event["band_energy"][band_index])
                for event in channel_events
                if band_index < len(event["band_energy"])
            ]
            curve = fit_decay_curve(
                times,
                energies,
                band_index,
                min_t60,
                max_t60,
            )
            curve["center_hz"] = center
            channel_curves.append(curve)
        channels[channel_name] = channel_curves

    return {
        "listener_id": listener_id,
        "band_centers_hz": centers,
        "early_window_end_ms": duration_ms,
        "bands": bands,
        "channels": channels,
        "min_t60_s": min_t60,
        "max_t60_s": max_t60,
    }

