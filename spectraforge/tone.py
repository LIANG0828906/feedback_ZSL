"""Deterministic HDR tone mapping and display encoding."""

from __future__ import annotations

from typing import Any

import numpy as np

from .errors import WorkbenchError

LUMA_WEIGHTS = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)


def validate_tone_parameters(params: dict[str, Any]) -> None:
    curve = str(params.get("tone_curve", "reinhard"))
    if curve not in {"reinhard", "aces", "filmic"}:
        raise WorkbenchError(
            "E_PARA",
            "Unknown tone curve",
            {"field": "tone_curve", "value": curve, "allowed": ["reinhard", "aces", "filmic"]},
        )
    checks = [
        ("tone_exposure", -8.0, 8.0),
        ("tone_key", 0.05, 8.0),
        ("tone_white", 0.1, 100.0),
        ("saturation", 0.0, 2.5),
        ("display_gamma", 1.0, 3.0),
    ]
    for field, low, high in checks:
        try:
            value = float(params.get(field))
        except (TypeError, ValueError) as exc:
            raise WorkbenchError("E_PARA", "Tone parameter must be numeric", {"field": field}) from exc
        if not np.isfinite(value) or value < low or value > high:
            raise WorkbenchError(
                "E_PARA",
                "Tone parameter is outside its allowed range",
                {"field": field, "value": value, "min": low, "max": high},
            )


def _filmic(x: np.ndarray) -> np.ndarray:
    a, b, c, d, e, f = 0.15, 0.50, 0.10, 0.20, 0.02, 0.30
    return ((x * (a * x + c * b) + d * e) / (x * (a * x + b) + d * f)) - e / f


def tone_curve_luminance(luminance: np.ndarray, params: dict[str, Any]) -> np.ndarray:
    validate_tone_parameters(params)
    value = np.maximum(np.asarray(luminance, dtype=np.float64), 0.0)
    exposure_gain = float(2.0 ** float(params["tone_exposure"]))
    key = float(params["tone_key"])
    white = float(params["tone_white"])
    x = value * exposure_gain * key
    curve = str(params["tone_curve"])
    if curve == "reinhard":
        mapped = x * (1.0 + x / (white * white)) / (1.0 + x)
    elif curve == "aces":
        mapped = (x * (2.51 * x + 0.03)) / (x * (2.43 * x + 0.59) + 0.14)
    else:
        mapped = _filmic(x) / max(float(_filmic(np.array([white]))[0]), 1e-9)
    return np.clip(mapped, 0.0, 1.0)


def tone_map(linear_rgb: np.ndarray, params: dict[str, Any]) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Return display RGB (uint8), normalized tone-mapped RGB, and mapped luminance."""
    validate_tone_parameters(params)
    values = np.asarray(linear_rgb, dtype=np.float64)
    luminance = np.maximum(np.sum(values * LUMA_WEIGHTS, axis=2), 0.0)
    mapped_luminance = tone_curve_luminance(luminance, params)
    safe = np.maximum(luminance, 1e-9)
    ratio = np.where(luminance > 1e-9, mapped_luminance / safe, 0.0)
    toned = values * ratio[..., np.newaxis]
    luma_toned = np.sum(toned * LUMA_WEIGHTS, axis=2)
    saturation = float(params["saturation"])
    toned = luma_toned[..., np.newaxis] + (toned - luma_toned[..., np.newaxis]) * saturation
    toned = np.clip(toned, 0.0, 1.0)
    gamma = float(params["display_gamma"])
    encoded = np.power(toned, 1.0 / gamma)
    display = np.clip(np.rint(encoded * 255.0), 0, 255).astype(np.uint8)
    return display, encoded.astype(np.float32), mapped_luminance.astype(np.float32)


def display_luminance_histogram(luminance: np.ndarray, bins: int = 64) -> dict[str, Any]:
    values = np.asarray(luminance, dtype=np.float64).reshape(-1)
    finite = values[np.isfinite(values)]
    if finite.size == 0:
        return {"bins": bins, "counts": [0] * bins, "edges": [0.0] * (bins + 1)}
    low = max(float(finite.min()), 1e-6)
    high = max(float(finite.max()), low * 1.000001)
    edges = np.geomspace(low, high, bins + 1)
    counts, edges = np.histogram(finite, bins=edges)
    return {
        "bins": int(bins),
        "counts": counts.astype(int).tolist(),
        "edges": edges.astype(float).tolist(),
        "min": low,
        "max": high,
    }
