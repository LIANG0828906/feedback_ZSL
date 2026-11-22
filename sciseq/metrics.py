"""Quality metrics used by acceptance tests and preview/export gates."""

from __future__ import annotations

import math
from typing import Any, Iterable

import numpy as np

from .models import FeatureSpec


def _feature_mask(
    shape: tuple[int, int],
    features: Iterable[FeatureSpec],
    margin_scale: float = 3.5,
) -> np.ndarray:
    height, width = shape
    mask = np.zeros(shape, dtype=bool)
    yy = np.arange(height, dtype=np.float64)[:, None]
    xx = np.arange(width, dtype=np.float64)[None, :]
    for feature in features:
        cx, cy = feature.center
        if feature.kind == "line" and feature.endpoints is not None:
            (x0, y0), (x1, y1) = feature.endpoints
            vx = x1 - x0
            vy = y1 - y0
            length_sq = max(vx * vx + vy * vy, 1e-12)
            projection = np.clip(((xx - x0) * vx + (yy - y0) * vy) / length_sq, 0.0, 1.0)
            nearest_x = x0 + projection * vx
            nearest_y = y0 + projection * vy
            distance = np.hypot(xx - nearest_x, yy - nearest_y)
            radius = margin_scale * feature.sigma
        else:
            distance = np.hypot(xx - cx, yy - cy)
            radius = margin_scale * feature.sigma
        mask |= distance <= radius
    return mask


def _gradients(image: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    work = image.astype(np.float64, copy=False)
    gx = np.zeros_like(work)
    gy = np.zeros_like(work)
    gx[:, 1:-1] = 0.5 * (work[:, 2:] - work[:, :-2])
    gy[1:-1, :] = 0.5 * (work[2:, :] - work[:-2, :])
    return gx, gy


def quality_metrics(
    denoised: np.ndarray,
    clean_reference: np.ndarray,
    features: Iterable[FeatureSpec],
) -> dict[str, Any]:
    if denoised.shape != clean_reference.shape:
        raise ValueError("quality metric inputs must have equal shapes")
    feature_list = tuple(features)
    mask = _feature_mask(denoised.shape, feature_list)
    residual = denoised.astype(np.float64) - clean_reference.astype(np.float64)
    background_residual = residual[~mask]
    if background_residual.size == 0:
        residual_std = 0.0
    else:
        center = float(np.median(background_residual))
        residual_std = 1.4826 * float(np.median(np.abs(background_residual - center)))

    gx_clean, gy_clean = _gradients(clean_reference)
    gx_denoised, gy_denoised = _gradients(denoised)
    if np.any(mask):
        clean_vector = np.concatenate((gx_clean[mask], gy_clean[mask]))
        denoised_vector = np.concatenate((gx_denoised[mask], gy_denoised[mask]))
        numerator = float(np.dot(clean_vector, denoised_vector))
        clean_norm = float(np.linalg.norm(clean_vector))
        denoised_norm = float(np.linalg.norm(denoised_vector))
        if clean_norm <= 1e-12 or denoised_norm <= 1e-12:
            cosine = 0.0
            contrast_ratio = 0.0
        else:
            cosine = max(0.0, min(1.0, numerator / (clean_norm * denoised_norm)))
            contrast_ratio = min(1.0, denoised_norm / clean_norm)
        edge_fidelity = 0.5 * cosine + 0.5 * contrast_ratio
    else:
        cosine = 1.0
        contrast_ratio = 1.0
        edge_fidelity = 1.0
    return {
        "residual_std": float(residual_std),
        "residual_mean": float(np.mean(residual)) if residual.size else 0.0,
        "residual_max_abs": float(np.max(np.abs(residual))) if residual.size else 0.0,
        "edge_fidelity": float(edge_fidelity),
        "edge_gradient_cosine": float(cosine),
        "edge_contrast_ratio": float(contrast_ratio),
        "feature_mask_fraction": float(mask.mean()),
    }


def feature_center_errors(
    extracted: Iterable[Any],
    truth_features: Iterable[FeatureSpec],
    frame_index: int,
    *,
    max_distance: float = 20.0,
) -> dict[str, Any]:
    truth = list(truth_features)
    predictions = list(extracted)
    matched_predictions: set[int] = set()
    errors: list[float] = []
    matches: list[dict[str, Any]] = []
    for feature in truth:
        best_index: int | None = None
        best_distance = math.inf
        for index, prediction in enumerate(predictions):
            if index in matched_predictions:
                continue
            if prediction.kind != feature.kind:
                continue
            distance = math.hypot(
                prediction.center[0] - feature.center[0],
                prediction.center[1] - feature.center[1],
            )
            if distance < best_distance:
                best_distance = distance
                best_index = index
        if best_index is not None and best_distance <= max_distance:
            matched_predictions.add(best_index)
            errors.append(best_distance)
            matches.append(
                {
                    "truth_id": feature.feature_id,
                    "predicted_id": predictions[best_index].feature_id,
                    "kind": feature.kind,
                    "distance_px": float(best_distance),
                }
            )
    return {
        "frame": int(frame_index),
        "truth_count": len(truth),
        "predicted_count": len(predictions),
        "matched_count": len(matches),
        "mean_center_error_px": float(np.mean(errors)) if errors else None,
        "max_center_error_px": float(np.max(errors)) if errors else None,
        "matches": matches,
    }

