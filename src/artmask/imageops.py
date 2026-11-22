"""Deterministic NumPy-only image and mask operations."""

from __future__ import annotations

import numpy as np


def rgb_to_gray(image: np.ndarray) -> np.ndarray:
    rgb = image.astype(np.float32, copy=False)
    return 0.2126 * rgb[..., 0] + 0.7152 * rgb[..., 1] + 0.0722 * rgb[..., 2]


def gradient_magnitude(image: np.ndarray) -> np.ndarray:
    """Per-pixel maximum color-channel gradient magnitude."""

    rgb = image.astype(np.float32, copy=False)
    gy, gx = np.gradient(rgb, axis=(0, 1))
    magnitude = np.sqrt(gx * gx + gy * gy)
    return np.max(magnitude, axis=2).astype(np.float32, copy=False)


def estimate_border_color(image: np.ndarray, fraction: float = 0.04) -> np.ndarray:
    height, width = image.shape[:2]
    border = max(1, int(round(min(height, width) * fraction)))
    strips = [
        image[:border, :, :],
        image[-border:, :, :],
        image[:, :border, :],
        image[:, -border:, :],
    ]
    samples = np.concatenate([strip.reshape(-1, image.shape[2]) for strip in strips])
    return np.median(samples, axis=0).astype(np.float32)


def downsample_rgb_median(image: np.ndarray, factor: int) -> np.ndarray:
    if factor <= 1:
        return image
    height, width = image.shape[:2]
    target_h = max(1, height // factor)
    target_w = max(1, width // factor)
    cropped = image[: target_h * factor, : target_w * factor]
    blocks = cropped.reshape(target_h, factor, target_w, factor, image.shape[2])
    median = np.median(blocks, axis=(1, 3))
    return np.clip(np.rint(median), 0, 255).astype(np.uint8)


def upsample_labels_nearest(
    labels: np.ndarray, target_shape: tuple[int, int], factor: int
) -> np.ndarray:
    if factor <= 1:
        return labels.copy()
    target_h, target_w = target_shape
    source_h, source_w = labels.shape
    ys = np.minimum(np.arange(target_h, dtype=np.int64) // factor, source_h - 1)
    xs = np.minimum(np.arange(target_w, dtype=np.int64) // factor, source_w - 1)
    return labels[ys[:, None], xs[None, :]]


def _binary_erode(mask: np.ndarray, size: int) -> np.ndarray:
    if size <= 1:
        return mask.copy()
    pad = size // 2
    padded = np.pad(mask, pad, mode="constant", constant_values=False)
    result = np.ones_like(mask, dtype=bool)
    height, width = mask.shape
    for dy in range(size):
        for dx in range(size):
            result &= padded[dy : dy + height, dx : dx + width]
    return result


def _binary_dilate(mask: np.ndarray, size: int) -> np.ndarray:
    if size <= 1:
        return mask.copy()
    pad = size // 2
    padded = np.pad(mask, pad, mode="constant", constant_values=False)
    result = np.zeros_like(mask, dtype=bool)
    height, width = mask.shape
    for dy in range(size):
        for dx in range(size):
            result |= padded[dy : dy + height, dx : dx + width]
    return result


def binary_open_close(mask: np.ndarray, size: int) -> np.ndarray:
    if size <= 1:
        return mask.copy()
    opened = _binary_dilate(_binary_erode(mask, size), size)
    closed = _binary_erode(_binary_dilate(opened, size), size)
    return closed


def cleanup_label_mask(
    labels: np.ndarray, morph_size: int, min_area: int
) -> np.ndarray:
    """Apply label-wise open/close while preserving deterministic label identity."""

    if morph_size <= 1 and min_area <= 0:
        return labels.copy()
    result = np.zeros_like(labels)
    height, width = labels.shape
    pad = morph_size // 2
    for label in sorted(int(value) for value in np.unique(labels) if value != 0):
        ys, xs = np.nonzero(labels == label)
        if len(xs) == 0:
            continue
        y0 = max(0, int(ys.min()) - pad)
        y1 = min(height, int(ys.max()) + pad + 1)
        x0 = max(0, int(xs.min()) - pad)
        x1 = min(width, int(xs.max()) + pad + 1)
        local = labels[y0:y1, x0:x1] == label
        refined = binary_open_close(local, morph_size)
        if int(np.count_nonzero(refined)) < max(1, min_area):
            continue
        area = result[y0:y1, x0:x1]
        area[refined] = label
    return result


def label_colors(count: int) -> np.ndarray:
    palette = np.array(
        [
            [0, 0, 0],
            [231, 76, 60],
            [46, 204, 113],
            [52, 152, 219],
            [241, 196, 15],
            [155, 89, 182],
            [26, 188, 156],
            [230, 126, 34],
            [236, 240, 241],
            [149, 165, 166],
            [192, 57, 43],
            [39, 174, 96],
        ],
        dtype=np.uint8,
    )
    if count <= len(palette):
        return palette[:count]
    extra = np.random.default_rng(20261123).integers(
        32, 240, size=(count - len(palette), 3), dtype=np.uint8
    )
    return np.vstack([palette, extra])


def labels_to_rgb(labels: np.ndarray) -> np.ndarray:
    max_label = int(labels.max(initial=0))
    palette = label_colors(max_label + 1)
    return palette[labels]


def overlay_labels(
    image: np.ndarray, labels: np.ndarray, alpha: float = 0.52
) -> np.ndarray:
    colors = labels_to_rgb(labels)
    base = image.astype(np.float32)
    overlay = colors.astype(np.float32)
    active = labels > 0
    out = base.copy()
    out[active] = base[active] * (1.0 - alpha) + overlay[active] * alpha
    return np.clip(out, 0, 255).astype(np.uint8)


def deviation_heatmap(
    predicted: np.ndarray,
    truth: np.ndarray,
    region_errors: dict[int, float],
    saturation: float = 12.0,
) -> np.ndarray:
    """RGBA heatmap where red intensity follows each matched region's error."""

    height, width = predicted.shape
    rgba = np.zeros((height, width, 4), dtype=np.uint8)
    active = (predicted > 0) | (truth > 0)
    intensity = np.zeros((height, width), dtype=np.float32)
    for region_id, error in region_errors.items():
        intensity[predicted == region_id] = error
    extra_or_missing = (predicted > 0) != (truth > 0)
    normalized = np.clip(intensity / max(saturation, 1e-9), 0.0, 1.0)
    rgba[..., 0] = np.where(active, 70 + 185 * normalized, 0).astype(np.uint8)
    rgba[..., 1] = np.where(active, 200 * (1.0 - normalized), 0).astype(np.uint8)
    rgba[..., 2] = np.where(active, 80 * (1.0 - normalized), 0).astype(np.uint8)
    rgba[..., 3] = np.where(active, 150, 0).astype(np.uint8)
    rgba[extra_or_missing] = np.array([255, 30, 255, 220], dtype=np.uint8)
    return rgba
