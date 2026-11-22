"""Deterministic quality metrics with exact block-boundary composition."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .errors import WorkbenchError, require

SSIM_C1 = (0.01 * 255.0) ** 2
SSIM_C2 = (0.03 * 255.0) ** 2
PSNR_CEILING = 100.0


def as_channels(image: np.ndarray) -> np.ndarray:
    array = np.asarray(image)
    if array.ndim == 2:
        return array[:, :, None]
    return array


def grayscale(image: np.ndarray) -> np.ndarray:
    array = as_channels(image).astype(np.float64)
    if array.shape[2] == 1:
        return array[:, :, 0]
    return 0.2126 * array[:, :, 0] + 0.7152 * array[:, :, 1] + 0.0722 * array[:, :, 2]


def _symmetric_indices(indices: np.ndarray, length: int) -> np.ndarray:
    if length <= 1:
        return np.zeros_like(indices)
    period = 2 * length
    folded = np.mod(indices, period)
    return np.where(folded < length, folded, period - 1 - folded)


def _extract_with_halo(
    image: np.ndarray,
    y0: int,
    y1: int,
    x0: int,
    x1: int,
    band: int,
) -> tuple[np.ndarray, tuple[slice, slice]]:
    height, width = image.shape[:2]
    ys = _symmetric_indices(np.arange(y0 - band, y1 + band), height)
    xs = _symmetric_indices(np.arange(x0 - band, x1 + band), width)
    extracted = image[ys][:, xs]
    crop = (slice(band, band + (y1 - y0)), slice(band, band + (x1 - x0)))
    return np.ascontiguousarray(extracted), crop


def _box_mean(values: np.ndarray, window: int) -> np.ndarray:
    radius = window // 2
    padded = np.pad(values, radius, mode="symmetric")
    integral = np.pad(padded, ((1, 0), (1, 0)), mode="constant").cumsum(axis=0).cumsum(axis=1)
    sums = (
        integral[window:, window:]
        - integral[:-window, window:]
        - integral[window:, :-window]
        + integral[:-window, :-window]
    )
    return sums / float(window * window)


def ssim_map(image: np.ndarray, reconstruction: np.ndarray, window: int) -> np.ndarray:
    require(image.shape == reconstruction.shape, "E_DIFF", "SSIM input shapes differ")
    height, width = image.shape[:2]
    require(window >= 3 and window % 2 == 1, "E_PARA", "evaluation window must be an odd integer >= 3", field="evaluation_window")
    require(window <= min(height, width), "E_BAND", "evaluation window exceeds image dimensions", window=window, height=height, width=width)
    maps = []
    for channel in range(as_channels(image).shape[2]):
        first = as_channels(image)[:, :, channel].astype(np.float64, copy=False)
        second = as_channels(reconstruction)[:, :, channel].astype(np.float64, copy=False)
        mean_first = _box_mean(first, window)
        mean_second = _box_mean(second, window)
        mean_first_sq = mean_first * mean_first
        mean_second_sq = mean_second * mean_second
        mean_product = mean_first * mean_second
        variance_first = np.maximum(_box_mean(first * first, window) - mean_first_sq, 0.0)
        variance_second = np.maximum(_box_mean(second * second, window) - mean_second_sq, 0.0)
        covariance = _box_mean(first * second, window) - mean_product
        numerator = (2.0 * mean_product + SSIM_C1) * (2.0 * covariance + SSIM_C2)
        denominator = (mean_first_sq + mean_second_sq + SSIM_C1) * (variance_first + variance_second + SSIM_C2)
        maps.append(numerator / denominator)
    return np.mean(np.stack(maps, axis=2), axis=2)


def ssim_region(
    image: np.ndarray,
    reconstruction: np.ndarray,
    window: int,
    y0: int,
    y1: int,
    x0: int,
    x1: int,
) -> np.ndarray:
    radius = window // 2
    extracted_first, crop = _extract_with_halo(image, y0, y1, x0, x1, radius)
    extracted_second, _ = _extract_with_halo(reconstruction, y0, y1, x0, x1, radius)
    return ssim_map(extracted_first, extracted_second, window)[crop]


def gradient_magnitude_region(
    monochrome: np.ndarray,
    y0: int,
    y1: int,
    x0: int,
    x1: int,
) -> np.ndarray:
    extracted, _ = _extract_with_halo(monochrome.astype(np.float64, copy=False), y0, y1, x0, x1, 1)
    gradient_x = (extracted[1:-1, 2:] - extracted[1:-1, :-2]) * 0.5
    gradient_y = (extracted[2:, 1:-1] - extracted[:-2, 1:-1]) * 0.5
    magnitude = np.sqrt(gradient_x * gradient_x + gradient_y * gradient_y)
    return magnitude


def edge_similarity_region(
    image: np.ndarray,
    reconstruction: np.ndarray,
    y0: int,
    y1: int,
    x0: int,
    x1: int,
) -> np.ndarray:
    original_gradient = gradient_magnitude_region(grayscale(image), y0, y1, x0, x1)
    reconstructed_gradient = gradient_magnitude_region(grayscale(reconstruction), y0, y1, x0, x1)
    return np.clip(1.0 - np.abs(original_gradient - reconstructed_gradient) / 255.0, 0.0, 1.0)


def squared_error_map(image: np.ndarray, reconstruction: np.ndarray) -> np.ndarray:
    require(image.shape == reconstruction.shape, "E_DIFF", "pixel maps require identical shapes")
    squared = (image.astype(np.float64) - reconstruction.astype(np.float64)) ** 2
    return np.mean(squared, axis=2) if squared.ndim == 3 else squared


def psnr_from_mse(mse: float) -> float:
    if mse <= 0.0:
        return PSNR_CEILING
    return min(PSNR_CEILING, 10.0 * math.log10((255.0 * 255.0) / mse))


def evaluate_region(
    image: np.ndarray,
    reconstruction: np.ndarray,
    truth_edge_mask: np.ndarray,
    *,
    evaluation_window: int,
    y0: int,
    y1: int,
    x0: int,
    x1: int,
) -> dict[str, Any]:
    """Evaluate one exclusive block while reading an overlapping halo."""

    error = squared_error_map(image, reconstruction)[y0:y1, x0:x1]
    ssim_slice = ssim_region(image, reconstruction, evaluation_window, y0, y1, x0, x1)
    edge_slice = edge_similarity_region(image, reconstruction, y0, y1, x0, x1)
    truth_slice = truth_edge_mask[y0:y1, x0:x1]
    edge_values = edge_slice[truth_slice]
    edge_count = int(edge_values.size)
    return {
        "pixel_count": int(error.size),
        "pixel_sse": float(error.sum(dtype=np.float64)),
        "pixel_mse": float(error.mean(dtype=np.float64)),
        "pixel_max_abs": float(
            np.abs(
                image[y0:y1, x0:x1].astype(np.float64)
                - reconstruction[y0:y1, x0:x1].astype(np.float64)
            ).max(initial=0.0)
        ),
        "ssim_sum": float(ssim_slice.sum(dtype=np.float64)),
        "ssim_mean": float(ssim_slice.mean(dtype=np.float64)),
        "edge_sum": float(edge_values.sum(dtype=np.float64)) if edge_count else 0.0,
        "edge_count": edge_count,
        "edge_fidelity": float(edge_values.mean(dtype=np.float64)) if edge_count else 1.0,
        "ssim_slice": ssim_slice.astype(np.float32),
        "error_slice": error.astype(np.float32),
        "edge_slice": edge_slice.astype(np.float32),
    }


def evaluate_whole(
    image: np.ndarray,
    reconstruction: np.ndarray,
    truth_edge_mask: np.ndarray,
    *,
    evaluation_window: int,
) -> dict[str, Any]:
    height, width = image.shape[:2]
    region = evaluate_region(
        image,
        reconstruction,
        truth_edge_mask,
        evaluation_window=evaluation_window,
        y0=0,
        y1=height,
        x0=0,
        x1=width,
    )
    mse = region["pixel_sse"] / region["pixel_count"]
    return {
        "psnr_db": psnr_from_mse(mse),
        "mse": float(mse),
        "ssim": region["ssim_mean"],
        "edge_fidelity": region["edge_fidelity"],
        "truth_edge_pixels": int(truth_edge_mask.sum()),
        "evaluated_truth_edge_pixels": region["edge_count"],
        "evaluation_window": evaluation_window,
    }


def compose_maps(
    image_shape: tuple[int, ...],
    block_entries: dict[int, dict[str, Any]],
    grid: tuple[int, int],
    block_size: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Merge exclusive block slices by block index; later writes never overlap."""

    height, width = image_shape[:2]
    ssim_full = np.empty((height, width), dtype=np.float32)
    error_full = np.empty((height, width), dtype=np.float32)
    edge_full = np.empty((height, width), dtype=np.float32)
    covered_ssim = np.zeros((height, width), dtype=np.uint8)
    covered_error = np.zeros((height, width), dtype=np.uint8)
    covered_edge = np.zeros((height, width), dtype=np.uint8)
    rows, columns = grid
    for block_index in sorted(block_entries):
        row, column = divmod(block_index, columns)
        y0 = row * block_size
        x0 = column * block_size
        y1 = min(height, y0 + block_size)
        x1 = min(width, x0 + block_size)
        entry = block_entries[block_index]
        ssim_slice = entry["ssim_slice"]
        error_slice = entry["error_slice"]
        edge_slice = entry["edge_slice"]
        expected_shape = (y1 - y0, x1 - x0)
        if ssim_slice.shape != expected_shape or error_slice.shape != expected_shape or edge_slice.shape != expected_shape:
            raise WorkbenchError(
                "E_DIFF",
                "block map slice shape mismatch",
                {"block_index": block_index, "expected": list(expected_shape), "ssim": list(ssim_slice.shape)},
            )
        if np.any(covered_ssim[y0:y1, x0:x1]):
            raise WorkbenchError("E_STATE", "block map regions overlap", {"block_index": block_index})
        ssim_full[y0:y1, x0:x1] = ssim_slice
        error_full[y0:y1, x0:x1] = error_slice
        edge_full[y0:y1, x0:x1] = edge_slice
        covered_ssim[y0:y1, x0:x1] = 1
        covered_error[y0:y1, x0:x1] = 1
        covered_edge[y0:y1, x0:x1] = 1
    if not covered_ssim.all() or not covered_error.all() or not covered_edge.all():
        raise WorkbenchError("E_STATE", "block maps do not cover the image", {"grid": list(grid)})
    return ssim_full, error_full, edge_full


def heatmap_png_array(error_map: np.ndarray) -> np.ndarray:
    """Map squared error to a deterministic RGB diagnostic heatmap."""

    values = np.asarray(error_map, dtype=np.float64)
    positive = values[values > 0.0]
    scale = float(np.percentile(positive, 95.0)) if positive.size else 1.0
    scale = max(scale, 1e-6)
    normalized = np.clip(np.log1p(values) / np.log1p(scale), 0.0, 1.0)
    red = np.clip(3.0 * normalized, 0.0, 1.0)
    green = np.clip(3.0 * normalized - 1.0, 0.0, 1.0)
    blue = np.clip(3.0 * normalized - 2.0, 0.0, 1.0)
    rgb = np.stack((red, green, blue), axis=2)
    return np.rint(rgb * 255.0).astype(np.uint8)
