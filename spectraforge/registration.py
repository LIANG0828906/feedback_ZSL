"""Deterministic subpixel waveband registration and residual statistics."""

from __future__ import annotations

from typing import Any

import numpy as np

from .errors import WorkbenchError
from .parameters import INTERPOLATIONS


def _nearest(image: np.ndarray, sx: np.ndarray, sy: np.ndarray) -> np.ndarray:
    xi = np.clip(np.rint(sx).astype(np.int64), 0, image.shape[1] - 1)
    yi = np.clip(np.rint(sy).astype(np.int64), 0, image.shape[0] - 1)
    return image[yi, xi]


def _bilinear(image: np.ndarray, sx: np.ndarray, sy: np.ndarray) -> np.ndarray:
    x0 = np.floor(sx)
    y0 = np.floor(sy)
    fx = sx - x0
    fy = sy - y0
    xi0 = np.clip(x0.astype(np.int64), 0, image.shape[1] - 1)
    yi0 = np.clip(y0.astype(np.int64), 0, image.shape[0] - 1)
    xi1 = np.clip(xi0 + 1, 0, image.shape[1] - 1)
    yi1 = np.clip(yi0 + 1, 0, image.shape[0] - 1)
    top = image[yi0, xi0] * (1.0 - fx) + image[yi0, xi1] * fx
    bottom = image[yi1, xi0] * (1.0 - fx) + image[yi1, xi1] * fx
    return top * (1.0 - fy) + bottom * fy


def _catmull_rom(value: float) -> np.ndarray:
    x = abs(value)
    if x <= 1.0:
        return 1.5 * x**3 - 2.5 * x**2 + 1.0
    if x < 2.0:
        return -0.5 * x**3 + 2.5 * x**2 - 4.0 * x + 2.0
    return 0.0


def _bicubic(image: np.ndarray, sx: np.ndarray, sy: np.ndarray) -> np.ndarray:
    x0 = np.floor(sx)
    y0 = np.floor(sy)
    fx = sx - x0
    fy = sy - y0
    result = np.zeros_like(sx, dtype=np.float64)
    height, width = image.shape
    for oy in range(-1, 3):
        wy = _catmull_rom(oy - fy)
        yi = np.clip(y0.astype(np.int64) + oy, 0, height - 1)
        for ox in range(-1, 3):
            wx = _catmull_rom(ox - fx)
            xi = np.clip(x0.astype(np.int64) + ox, 0, width - 1)
            result += image[yi, xi] * wx * wy
    return result


def warp_shift(
    image: np.ndarray,
    dx: float,
    dy: float,
    interpolation: str = "bilinear",
) -> np.ndarray:
    if interpolation not in INTERPOLATIONS:
        raise WorkbenchError(
            "E_PARA",
            "Invalid registration interpolation",
            {"field": "interpolation", "value": interpolation, "allowed": sorted(INTERPOLATIONS)},
        )
    values = np.asarray(image, dtype=np.float64)
    if values.ndim != 2:
        raise WorkbenchError("E_DIM", "Registration expects a 2-D raster", {"shape": list(values.shape)})
    yy, xx = np.mgrid[0 : values.shape[0], 0 : values.shape[1]]
    sx = xx.astype(np.float64) - float(dx)
    sy = yy.astype(np.float64) - float(dy)
    if interpolation == "nearest":
        return _nearest(values, sx, sy).astype(np.float32)
    if interpolation == "bicubic":
        return _bicubic(values, sx, sy).astype(np.float32)
    return _bilinear(values, sx, sy).astype(np.float32)


def _gradient_energy(image: np.ndarray) -> np.ndarray:
    values = np.asarray(image, dtype=np.float64)
    gx = np.zeros_like(values)
    gy = np.zeros_like(values)
    gx[:, 1:] = np.abs(np.diff(values, axis=1))
    gy[1:, :] = np.abs(np.diff(values, axis=0))
    energy = gx + gy
    mean = float(energy.mean())
    return energy / mean if mean > 1e-12 else energy


def align_bands(
    bands: np.ndarray,
    offsets: np.ndarray,
    interpolation: str = "bilinear",
) -> tuple[np.ndarray, dict[str, Any]]:
    cube = np.asarray(bands)
    registration = np.asarray(offsets, dtype=np.float64)
    if cube.ndim != 3:
        raise WorkbenchError("E_DIM", "Band cube must be B x H x W", {"shape": list(cube.shape)})
    if registration.shape != (cube.shape[0], 2):
        raise WorkbenchError(
            "E_META",
            "Registration offsets do not match the band count",
            {"offsets_shape": list(registration.shape), "band_count": int(cube.shape[0])},
        )
    aligned = np.empty_like(cube, dtype=np.float32)
    edge_residuals: list[float] = []
    for index in range(cube.shape[0]):
        dx, dy = registration[index]
        aligned[index] = warp_shift(cube[index], dx, dy, interpolation) if (dx or dy) else cube[index]
        edge = aligned[index]
        border = np.concatenate(
            [
                np.abs(np.diff(edge[:, :2], axis=1)).reshape(-1),
                np.abs(np.diff(edge[:, -2:], axis=1)).reshape(-1),
                np.abs(np.diff(edge[:2, :], axis=0)).reshape(-1),
                np.abs(np.diff(edge[-2:, :], axis=0)).reshape(-1),
            ]
        )
        edge_residuals.append(float(border.mean()) if border.size else 0.0)
    reference = _gradient_energy(aligned[0])
    structural_residuals: list[float] = []
    for index in range(aligned.shape[0]):
        energy = _gradient_energy(aligned[index])
        denom = float(np.abs(reference).mean()) + 1e-12
        structural_residuals.append(float(np.abs(energy - reference).mean() / denom))
    residual_by_band = [
        {
            "band": int(index),
            "structural": float(structural_residuals[index]),
            "edge": float(edge_residuals[index]),
        }
        for index in range(aligned.shape[0])
    ]
    mean_residual = float(np.mean(structural_residuals)) if structural_residuals else 0.0
    report = {
        "interpolation": interpolation,
        "mean_structural_residual": mean_residual,
        "mean_edge_residual": float(np.mean(edge_residuals)) if edge_residuals else 0.0,
        "per_band": residual_by_band,
    }
    return aligned, report
