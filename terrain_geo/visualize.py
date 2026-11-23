"""Visualization helpers: control-point overlays and difference heat maps."""

from __future__ import annotations

import numpy as np

from .io_utils import save_image


def overlay_points(image: np.ndarray, points: np.ndarray,
                   color=(255, 64, 64), radius: int = 2) -> np.ndarray:
    img = _to_rgb(image).copy()
    pts = np.atleast_2d(np.asarray(points, dtype=np.int64))
    for idx, (x, y) in enumerate(pts):
        for dy in range(-radius, radius + 1):
            for dx in range(-radius, radius + 1):
                if dx * dx + dy * dy <= radius * radius:
                    yy, xx = y + dy, x + dx
                    if 0 <= yy < img.shape[0] and 0 <= xx < img.shape[1]:
                        img[yy, xx] = color
    return img


def diff_heatmap(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    """Absolute per-pixel difference scaled to an 8-bit heat map."""
    aa = _to_rgb(a).astype(np.int16)
    bb = _to_rgb(b).astype(np.int16)
    diff = np.max(np.abs(aa - bb), axis=2)
    if diff.max() > 0:
        scaled = diff * (255.0 / max(diff.max(), 1))
    else:
        scaled = diff.astype(np.float64)
    heat = np.zeros(diff.shape + (3,), dtype=np.uint8)
    heat[..., 0] = np.clip(scaled, 0, 255).astype(np.uint8)
    heat[..., 1] = np.clip(scaled * 0.4, 0, 255).astype(np.uint8)
    return heat


def _to_rgb(image: np.ndarray) -> np.ndarray:
    if image.ndim == 2:
        return np.repeat(image[..., None], 3, axis=2)
    if image.shape[2] == 1:
        return np.repeat(image, 3, axis=2)
    return image


def save_overlay(path: str, image: np.ndarray, points) -> None:
    save_image(path, overlay_points(image, points))
