"""Deterministic per-channel filters forming the processing chain."""

from __future__ import annotations

import math
from typing import Callable, Mapping

import numpy as np


Checkpoint = Callable[[], None]


def _noop() -> None:
    return None


def _as_hwc(array: np.ndarray) -> tuple[np.ndarray, bool]:
    if array.ndim == 2:
        return array[..., None], True
    if array.ndim == 3:
        return array, False
    raise ValueError(f"仅支持二维或三维图像数组，实际 {array.shape}")


def _restore_shape(array: np.ndarray, squeezed: bool) -> np.ndarray:
    return array[..., 0] if squeezed else array


def _box_mean(
    channel: np.ndarray,
    radius: int,
    checkpoint: Checkpoint,
) -> np.ndarray:
    if radius <= 0:
        return channel.astype(np.float64, copy=True)
    checkpoint()
    height, width = channel.shape
    padded = np.pad(
        channel.astype(np.float64, copy=False),
        ((radius, radius), (radius, radius)),
        mode="reflect" if height > 1 and width > 1 else "edge",
    )
    integral = padded.cumsum(axis=0).cumsum(axis=1)
    integral = np.pad(integral, ((1, 0), (1, 0)))
    span = 2 * radius + 1
    total = (
        integral[span:, span:]
        - integral[:-span, span:]
        - integral[span:, :-span]
        + integral[:-span, :-span]
    )
    return total / float(span * span)


def _finish_channel(channel: np.ndarray) -> np.ndarray:
    return np.clip(np.rint(channel), 0, 255).astype(np.uint8)


def denoise(
    array: np.ndarray,
    params: Mapping[str, int | float],
    checkpoint: Checkpoint = _noop,
) -> np.ndarray:
    radius = int(params["radius"])
    strength = float(params["strength"])
    source, squeezed = _as_hwc(array)
    output = np.empty_like(source)
    for channel_index in range(source.shape[2]):
        checkpoint()
        channel = source[..., channel_index].astype(np.float64)
        if radius == 0 or strength == 0.0:
            blended = channel
        else:
            mean = _box_mean(channel, radius, checkpoint)
            blended = channel * (1.0 - strength) + mean * strength
        output[..., channel_index] = _finish_channel(blended)
    return np.ascontiguousarray(_restore_shape(output, squeezed))


def sharpen(
    array: np.ndarray,
    params: Mapping[str, int | float],
    checkpoint: Checkpoint = _noop,
) -> np.ndarray:
    radius = int(params["radius"])
    amount = float(params["amount"])
    threshold = float(params["threshold"])
    source, squeezed = _as_hwc(array)
    output = np.empty_like(source)
    for channel_index in range(source.shape[2]):
        checkpoint()
        channel = source[..., channel_index].astype(np.float64)
        if amount == 0.0:
            sharpened = channel
        else:
            mean = _box_mean(channel, radius, checkpoint)
            detail = channel - mean
            candidate = channel + amount * detail
            sharpened = np.where(np.abs(detail) >= threshold, candidate, channel)
        output[..., channel_index] = _finish_channel(sharpened)
    return np.ascontiguousarray(_restore_shape(output, squeezed))


def _tile_edges(length: int, tile_size: int) -> np.ndarray:
    tile_count = min(length, max(1, math.ceil(length / tile_size)))
    edges = np.unique(np.linspace(0, length, tile_count + 1, dtype=np.int64))
    if edges[-1] != length:
        edges = np.append(edges, length)
    return edges


def _clahe_luts(
    channel: np.ndarray,
    tile_size: int,
    clip_limit: float,
    checkpoint: Checkpoint,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    height, width = channel.shape
    y_edges = _tile_edges(height, tile_size)
    x_edges = _tile_edges(width, tile_size)
    tiles_y = len(y_edges) - 1
    tiles_x = len(x_edges) - 1
    luts = np.empty((tiles_y, tiles_x, 256), dtype=np.float64)
    for tile_y in range(tiles_y):
        checkpoint()
        y0, y1 = int(y_edges[tile_y]), int(y_edges[tile_y + 1])
        for tile_x in range(tiles_x):
            x0, x1 = int(x_edges[tile_x]), int(x_edges[tile_x + 1])
            tile = channel[y0:y1, x0:x1]
            histogram = np.bincount(tile.ravel(), minlength=256).astype(np.float64)
            area = max(1, tile.size)
            clip_count = max(1.0, clip_limit * area / 256.0)
            clipped = np.minimum(histogram, clip_count)
            redistributed = clipped + (histogram.sum() - clipped.sum()) / 256.0
            cdf = redistributed.cumsum()
            denominator = max(cdf[-1], 1.0)
            luts[tile_y, tile_x] = np.rint(cdf / denominator * 255.0)
    centers_y = (y_edges[:-1] + y_edges[1:]) / 2.0
    centers_x = (x_edges[:-1] + x_edges[1:]) / 2.0
    return luts, centers_y, centers_x


def _axis_interpolation(
    length: int,
    centers: np.ndarray,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    positions = np.arange(length, dtype=np.float64) + 0.5
    upper = np.searchsorted(centers, positions, side="right")
    lower = upper - 1
    lower = np.clip(lower, 0, len(centers) - 1)
    upper = np.clip(upper, 0, len(centers) - 1)
    same = lower == upper
    denominator = centers[upper] - centers[lower]
    weights = np.zeros(length, dtype=np.float64)
    valid = ~same
    weights[valid] = (
        positions[valid] - centers[lower[valid]]
    ) / denominator[valid]
    return lower, upper, np.clip(weights, 0.0, 1.0)


def _local_contrast_channel(
    channel: np.ndarray,
    tile_size: int,
    clip_limit: float,
    blend: float,
    checkpoint: Checkpoint,
) -> np.ndarray:
    checkpoint()
    luts, centers_y, centers_x = _clahe_luts(
        channel, tile_size, clip_limit, checkpoint
    )
    y0, y1, wy = _axis_interpolation(channel.shape[0], centers_y)
    x0, x1, wx = _axis_interpolation(channel.shape[1], centers_x)
    checkpoint()
    col0 = luts[y0[:, None], x0[None, :], channel]
    col1 = luts[y0[:, None], x1[None, :], channel]
    row0 = luts[y1[:, None], x0[None, :], channel]
    row1 = luts[y1[:, None], x1[None, :], channel]
    top = col0 * (1.0 - wx[None, :]) + col1 * wx[None, :]
    bottom = row0 * (1.0 - wx[None, :]) + row1 * wx[None, :]
    equalized = top * (1.0 - wy[:, None]) + bottom * wy[:, None]
    checkpoint()
    return channel.astype(np.float64) * (1.0 - blend) + equalized * blend


def local_contrast(
    array: np.ndarray,
    params: Mapping[str, int | float],
    checkpoint: Checkpoint = _noop,
) -> np.ndarray:
    tile_size = int(params["tile_size"])
    clip_limit = float(params["clip_limit"])
    blend = float(params["blend"])
    source, squeezed = _as_hwc(array)
    output = np.empty_like(source)
    for channel_index in range(source.shape[2]):
        equalized = _local_contrast_channel(
            source[..., channel_index],
            tile_size,
            clip_limit,
            blend,
            checkpoint,
        )
        output[..., channel_index] = _finish_channel(equalized)
    return np.ascontiguousarray(_restore_shape(output, squeezed))


STAGE_FUNCTIONS: dict[str, Callable[..., np.ndarray]] = {
    "denoise": denoise,
    "sharpen": sharpen,
    "local_contrast": local_contrast,
}
