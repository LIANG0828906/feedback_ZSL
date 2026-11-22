"""Deterministic guided filtering and local contrast enhancement."""

from __future__ import annotations

from typing import Any

import numpy as np
from scipy import ndimage

from .errors import require


def _box_mean(array: np.ndarray, radius: int) -> np.ndarray:
    return ndimage.uniform_filter(
        np.asarray(array, dtype=np.float64),
        size=2 * int(radius) + 1,
        mode="reflect",
    )


def guided_filter(
    guide: np.ndarray,
    source: np.ndarray,
    *,
    radius: int,
    eps: float,
) -> np.ndarray:
    """Classic edge-aware guided filter with reflected boundaries."""

    guidance = np.asarray(guide, dtype=np.float64)
    payload = np.asarray(source, dtype=np.float64)
    require(
        guidance.shape == payload.shape and guidance.ndim == 2,
        "E_PARA",
        "Guided filter input shapes must match",
        field="detail",
    )
    require(isinstance(radius, int) and radius >= 1, "E_PARA", "guide_radius must be a positive integer", field="guide_radius")
    require(np.isfinite(eps) and eps > 0.0, "E_PARA", "guide_eps must be positive", field="guide_eps")

    mean_i = _box_mean(guidance, radius)
    mean_p = _box_mean(payload, radius)
    corr_i = _box_mean(guidance * guidance, radius)
    corr_ip = _box_mean(guidance * payload, radius)
    var_i = corr_i - mean_i * mean_i
    cov_ip = corr_ip - mean_i * mean_p
    a = cov_ip / (var_i + float(eps))
    b = mean_p - a * mean_i
    mean_a = _box_mean(a, radius)
    mean_b = _box_mean(b, radius)
    return mean_a * guidance + mean_b


def denoise_and_enhance_detail(
    guide: np.ndarray,
    detail: np.ndarray,
    *,
    radius: int,
    eps: float,
    gain: float,
    threshold: float,
) -> tuple[np.ndarray, np.ndarray]:
    """Denoise the detail layer, then apply thresholded local contrast gain."""

    cleaned = guided_filter(guide, detail, radius=radius, eps=eps)
    local_mean = _box_mean(cleaned, radius)
    delta = cleaned - local_mean
    magnitude = np.abs(delta)
    boosted_delta = np.sign(delta) * np.maximum(magnitude - float(threshold), 0.0)
    enhanced = cleaned + float(gain) * boosted_delta
    enhanced = np.clip(enhanced, 0.0, 2.0)
    return cleaned, enhanced


def _edge_magnitude(array: np.ndarray) -> np.ndarray:
    gy, gx = np.gradient(np.asarray(array, dtype=np.float64))
    return np.sqrt(gx * gx + gy * gy)


def filtering_quality(
    cleaned: np.ndarray,
    clean_detail: np.ndarray,
    *,
    noise_roi: list[int] | tuple[int, int, int, int],
    edge_roi: list[int] | tuple[int, int, int, int],
) -> dict[str, Any]:
    y0, x0, y1, x1 = (int(value) for value in noise_roi)
    ey0, ex0, ey1, ex1 = (int(value) for value in edge_roi)
    residual = np.asarray(cleaned, dtype=np.float64) - np.asarray(clean_detail, dtype=np.float64)
    residual_std = float(np.std(residual[y0:y1, x0:x1]))

    clean_edges = _edge_magnitude(clean_detail[ey0:ey1, ex0:ex1])
    filtered_edges = _edge_magnitude(cleaned[ey0:ey1, ex0:ex1])
    denominator = float(np.linalg.norm(clean_edges) * np.linalg.norm(filtered_edges))
    if denominator <= 1.0e-15:
        edge_fidelity = 1.0
    else:
        edge_fidelity = float(np.sum(clean_edges * filtered_edges) / denominator)
        edge_fidelity = float(np.clip(edge_fidelity, 0.0, 1.0))
    return {
        "residual_std": residual_std,
        "edge_fidelity": edge_fidelity,
        "noise_roi": [y0, x0, y1, x1],
        "edge_roi": [ey0, ex0, ey1, ex1],
        "residual_mean": float(np.mean(residual[y0:y1, x0:x1])),
    }

