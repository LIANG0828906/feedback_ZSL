"""Deterministic connected-component feature extraction and frame statistics."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .models import ExtractedFeature, NoiseEstimate


def detection_threshold(image: np.ndarray, noise: NoiseEstimate) -> float:
    background = float(noise.background)
    high = float(np.percentile(image, 99.5))
    return background + max(
        4.2 * max(noise.sigma, 1e-6),
        0.07 * (high - background),
    )


def _component_stats(
    frame_index: int,
    component_id: int,
    image: np.ndarray,
    pixels: list[tuple[int, int]],
    threshold: float,
) -> ExtractedFeature:
    ys = np.asarray([point[0] for point in pixels], dtype=np.float64)
    xs = np.asarray([point[1] for point in pixels], dtype=np.float64)
    values = image[ys.astype(np.int64), xs.astype(np.int64)].astype(np.float64)
    weights = np.maximum(values - threshold, 1e-6)
    total_weight = float(weights.sum())
    center_x = float((xs * weights).sum() / total_weight)
    center_y = float((ys * weights).sum() / total_weight)

    dx = xs - center_x
    dy = ys - center_y
    covariance = np.array(
        [
            [float((weights * dx * dx).sum() / total_weight), float((weights * dx * dy).sum() / total_weight)],
            [float((weights * dx * dy).sum() / total_weight), float((weights * dy * dy).sum() / total_weight)],
        ],
        dtype=np.float64,
    )
    eigenvalues, eigenvectors = np.linalg.eigh(covariance)
    major = max(float(eigenvalues[-1]), 1e-12)
    minor = max(float(eigenvalues[0]), 1e-12)
    elongation = math.sqrt(major / minor)
    vector = eigenvectors[:, -1]
    orientation = math.degrees(math.atan2(float(vector[1]), float(vector[0])))
    if orientation < 0.0:
        orientation += 180.0
    kind = "line" if elongation >= 2.3 else "spot"
    y0, y1 = int(ys.min()), int(ys.max())
    x0, x1 = int(xs.min()), int(xs.max())
    return ExtractedFeature(
        frame=frame_index,
        feature_id=f"{kind}_{component_id:02d}",
        kind=kind,
        center=(center_x, center_y),
        area=int(len(pixels)),
        peak=float(values.max()),
        bbox=(x0, y0, x1, y1),
        orientation_deg=float(orientation),
        elongation=float(elongation),
        mean_intensity=float(values.mean()),
    )


def extract_features(
    image: np.ndarray,
    noise: NoiseEstimate,
    *,
    frame_index: int = 0,
    min_area: int = 5,
) -> tuple[ExtractedFeature, ...]:
    """Extract bright spots and line-like components with weighted centroids."""

    if image.ndim != 2:
        raise ValueError("feature extraction requires a 2D image")
    work = image.astype(np.float64, copy=False)
    threshold = detection_threshold(work, noise)
    mask = work > threshold
    if not np.any(mask):
        return ()

    height, width = mask.shape
    visited = np.zeros_like(mask, dtype=bool)
    components: list[tuple[int, list[tuple[int, int]]]] = []
    flat_mask = mask.ravel()
    for flat_index in np.flatnonzero(flat_mask):
        y = int(flat_index // width)
        x = int(flat_index - y * width)
        if visited[y, x]:
            continue
        stack = [(y, x)]
        visited[y, x] = True
        pixels: list[tuple[int, int]] = []
        while stack:
            current_y, current_x = stack.pop()
            pixels.append((current_y, current_x))
            for dy in (-1, 0, 1):
                for dx in (-1, 0, 1):
                    if dx == 0 and dy == 0:
                        continue
                    ny = current_y + dy
                    nx = current_x + dx
                    if 0 <= ny < height and 0 <= nx < width and mask[ny, nx] and not visited[ny, nx]:
                        visited[ny, nx] = True
                        stack.append((ny, nx))
        if len(pixels) >= min_area:
            components.append((len(pixels), pixels))

    components.sort(key=lambda item: (-item[0], item[1][0][0], item[1][0][1]))
    extracted = [
        _component_stats(frame_index, index, work, pixels, threshold)
        for index, (_, pixels) in enumerate(components)
    ]
    extracted.sort(key=lambda feature: (feature.center[1], feature.center[0]))
    for index, feature in enumerate(extracted):
        object.__setattr__(feature, "feature_id", f"{feature.kind}_{index:02d}")
    return tuple(extracted)


def frame_statistics(
    frame_index: int,
    features: tuple[ExtractedFeature, ...],
    noise: NoiseEstimate,
    offset: tuple[float, float],
    threshold: float,
) -> dict[str, Any]:
    spots = [feature for feature in features if feature.kind == "spot"]
    lines = [feature for feature in features if feature.kind == "line"]
    centers = np.asarray([feature.center for feature in features], dtype=np.float64)
    return {
        "frame": int(frame_index),
        "state": "committed",
        "offset": [float(offset[0]), float(offset[1])],
        "noise_sigma": float(noise.sigma),
        "background": float(noise.background),
        "threshold": float(threshold),
        "feature_count": len(features),
        "spot_count": len(spots),
        "line_count": len(lines),
        "total_feature_area": int(sum(feature.area for feature in features)),
        "centroid_mean": (
            [float(centers[:, 0].mean()), float(centers[:, 1].mean())]
            if centers.size
            else None
        ),
        "features": [feature.to_dict() for feature in features],
    }


def aggregate_statistics(frame_stats: list[dict[str, Any]]) -> dict[str, Any]:
    if not frame_stats:
        return {"frame_count": 0}
    noise = np.asarray([item["noise_sigma"] for item in frame_stats], dtype=np.float64)
    offsets = np.asarray([item["offset"] for item in frame_stats], dtype=np.float64)
    total_features = sum(int(item["feature_count"]) for item in frame_stats)
    total_spots = sum(int(item["spot_count"]) for item in frame_stats)
    total_lines = sum(int(item["line_count"]) for item in frame_stats)
    return {
        "frame_count": len(frame_stats),
        "total_features": total_features,
        "total_spots": total_spots,
        "total_lines": total_lines,
        "mean_feature_count": total_features / len(frame_stats),
        "noise_sigma_min": float(noise.min()),
        "noise_sigma_max": float(noise.max()),
        "noise_sigma_mean": float(noise.mean()),
        "mean_abs_offset": [
            float(np.mean(np.abs(offsets[:, 0]))),
            float(np.mean(np.abs(offsets[:, 1]))),
        ],
        "max_abs_offset": [
            float(np.max(np.abs(offsets[:, 0]))),
            float(np.max(np.abs(offsets[:, 1]))),
        ],
        "frames": frame_stats,
    }

