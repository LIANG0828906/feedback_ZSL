"""Orthonormal 2-D DCT-II primitives used by the block codec."""

from __future__ import annotations

from functools import lru_cache

import numpy as np


@lru_cache(maxsize=32)
def dct_matrix(size: int) -> np.ndarray:
    if size < 2:
        raise ValueError("DCT size must be at least 2")
    indices = np.arange(size, dtype=np.float64)
    matrix = np.empty((size, size), dtype=np.float64)
    for frequency in range(size):
        alpha = np.sqrt(1.0 / size) if frequency == 0 else np.sqrt(2.0 / size)
        matrix[frequency] = alpha * np.cos(np.pi * (2.0 * indices + 1.0) * frequency / (2.0 * size))
    return matrix


def dct2(block: np.ndarray) -> np.ndarray:
    basis = dct_matrix(block.shape[0])
    return basis @ block @ basis.T


def idct2(coefficients: np.ndarray) -> np.ndarray:
    basis = dct_matrix(coefficients.shape[0])
    return basis.T @ coefficients @ basis


def dct2_channels(block: np.ndarray) -> np.ndarray:
    """Transform HxW or HxWxC into the same shape."""

    if block.ndim == 2:
        return dct2(block)
    return np.stack([dct2(block[:, :, channel]) for channel in range(block.shape[2])], axis=2)


def idct2_channels(coefficients: np.ndarray) -> np.ndarray:
    if coefficients.ndim == 2:
        return idct2(coefficients)
    return np.stack(
        [idct2(coefficients[:, :, channel]) for channel in range(coefficients.shape[2])],
        axis=2,
    )
