"""Block noise estimation and adaptive denoising filters."""

from __future__ import annotations

import math

import numpy as np

from .config import DenoiseConfig
from .models import NoiseEstimate


def _robust_sigma(values: np.ndarray) -> float:
    if values.size == 0:
        return 0.0
    center = float(np.median(values))
    mad = float(np.median(np.abs(values - center)))
    return 1.4826 * mad


def estimate_noise(image: np.ndarray, block_size: int = 32) -> NoiseEstimate:
    """Estimate a robust noise floor from local first differences."""

    if image.ndim != 2:
        raise ValueError("noise estimation requires a 2D image")
    if not 4 <= block_size <= min(image.shape):
        raise ValueError("block_size must be in [4, min(image.shape)]")
    height, width = image.shape
    work = image.astype(np.float64, copy=False)
    block_sigmas: list[float] = []
    block_means: list[float] = []
    for y0 in range(0, height - block_size + 1, block_size):
        for x0 in range(0, width - block_size + 1, block_size):
            block = work[y0 : y0 + block_size, x0 : x0 + block_size]
            differences = np.concatenate(
                (np.diff(block, axis=0).ravel(), np.diff(block, axis=1).ravel())
            )
            differences -= float(np.median(differences))
            sigma = 1.4826 * float(np.median(np.abs(differences))) / math.sqrt(2.0)
            block_sigmas.append(sigma)
            block_means.append(float(np.median(block)))
    if not block_sigmas:
        return NoiseEstimate(0.0, block_size, np.zeros((0, 0), dtype=np.float32), float(np.median(work)))
    sigma_array = np.asarray(block_sigmas, dtype=np.float64)
    # The low percentile rejects blocks dominated by feature edges.
    sigma = float(np.percentile(sigma_array, 20))
    blocks_per_row = (width - block_size) // block_size + 1
    block_count = len(block_sigmas)
    rows = (block_count + blocks_per_row - 1) // blocks_per_row
    padded = np.zeros(rows * blocks_per_row, dtype=np.float64)
    padded[:block_count] = sigma_array
    block_map = padded.reshape(rows, blocks_per_row).astype(np.float32)
    background = float(np.percentile(np.asarray(block_means), 20))
    return NoiseEstimate(sigma, block_size, block_map, background)


def _reflect_pad(image: np.ndarray, pad_y: int, pad_x: int) -> np.ndarray:
    mode = "reflect" if min(image.shape) > 1 else "edge"
    return np.pad(image, ((pad_y, pad_y), (pad_x, pad_x)), mode=mode)


def gaussian_blur(image: np.ndarray, sigma: float) -> np.ndarray:
    if sigma <= 0:
        return image.astype(np.float32, copy=True)
    radius = max(1, int(math.ceil(3.0 * sigma)))
    axis = np.arange(-radius, radius + 1, dtype=np.float64)
    kernel = np.exp(-(axis**2) / (2.0 * sigma**2))
    kernel /= kernel.sum()
    work = image.astype(np.float64, copy=False)
    padded = _reflect_pad(work, radius, radius)
    height, width = work.shape
    horizontal = np.zeros_like(work)
    for index, weight in enumerate(kernel):
        horizontal += weight * padded[radius : radius + height, index : index + width]
    vertical_input = _reflect_pad(horizontal, radius, 0)
    output = np.zeros_like(work)
    for index, weight in enumerate(kernel):
        output += weight * vertical_input[index : index + height, :]
    return output.astype(np.float32)


def median_filter(image: np.ndarray, radius: int) -> np.ndarray:
    work = image.astype(np.float64, copy=False)
    height, width = work.shape
    padded = _reflect_pad(work, radius, radius)
    stack = np.empty(
        ((2 * radius + 1) ** 2, height, width),
        dtype=np.float64,
    )
    cursor = 0
    for dy in range(-radius, radius + 1):
        for dx in range(-radius, radius + 1):
            stack[cursor] = padded[
                radius + dy : radius + dy + height,
                radius + dx : radius + dx + width,
            ]
            cursor += 1
    return np.median(stack, axis=0).astype(np.float32)


def _box_mean(image: np.ndarray, radius: int) -> np.ndarray:
    height, width = image.shape
    padded = _reflect_pad(image.astype(np.float64, copy=False), radius, radius)
    integral = np.pad(
        padded.cumsum(axis=0).cumsum(axis=1),
        ((1, 0), (1, 0)),
        mode="constant",
    )
    size = 2 * radius + 1
    total = (
        integral[size : size + height, size : size + width]
        - integral[:height, size : size + width]
        - integral[size : size + height, :width]
        + integral[:height, :width]
    )
    return total / float(size * size)


def guided_filter(image: np.ndarray, radius: int, epsilon: float) -> np.ndarray:
    work = image.astype(np.float64, copy=False)
    mean = _box_mean(work, radius)
    mean_square = _box_mean(work * work, radius)
    variance = np.maximum(mean_square - mean * mean, 0.0)
    covariance = variance
    coefficient = covariance / (covariance + max(epsilon, 1e-12))
    intercept = mean - coefficient * mean
    output = _box_mean(coefficient, radius) * work + _box_mean(intercept, radius)
    return output.astype(np.float32)


def adaptive_denoise(
    image: np.ndarray,
    config: DenoiseConfig,
    noise: NoiseEstimate,
) -> tuple[np.ndarray, dict[str, float | int | str]]:
    """Denoise using strength selected from the measured per-frame noise floor."""

    config.validate(image.shape)
    if not np.all(np.isfinite(image)):
        raise ValueError("denoise input contains non-finite pixels")

    noise_sigma = max(float(noise.sigma), 1e-6)
    adaptive_scale = min(2.0, max(0.55, 0.88 + 0.045 * noise_sigma))
    details: dict[str, float | int | str] = {
        "filter": config.filter,
        "measured_noise_sigma": float(noise.sigma),
        "adaptive_scale": float(adaptive_scale),
    }
    if config.filter == "gaussian":
        spatial_sigma = min(2.4, max(0.35, float(config.strength) * adaptive_scale))
        details["spatial_sigma"] = float(spatial_sigma)
        return gaussian_blur(image, spatial_sigma), details
    if config.filter == "median":
        radius = int(config.median_radius)
        if noise_sigma > 12.0 and radius < 2:
            radius += 1
        details["median_radius"] = radius
        return median_filter(image, radius), details
    epsilon = max(0.25, float(config.strength) * noise_sigma * 0.65) ** 2
    details["guided_radius"] = int(config.guided_radius)
    details["guided_epsilon"] = float(epsilon)
    return guided_filter(image, int(config.guided_radius), epsilon), details

