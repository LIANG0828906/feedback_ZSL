"""Real spherical-harmonic basis, projection, and irradiance reconstruction."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .errors import ErrorCode, ProbeError


def validate_order(order: int) -> int:
    if isinstance(order, bool) or not isinstance(order, (int, np.integer)):
        raise ProbeError(ErrorCode.SH_INVALID, "SH order must be an integer")
    actual = int(order)
    if actual < 2 or actual > 6:
        raise ProbeError(ErrorCode.SH_INVALID, "SH order must be in [2, 6]")
    return actual


def sh_coefficient_count(order: int) -> int:
    actual = validate_order(order)
    return (actual + 1) ** 2


def normalize_directions(directions: np.ndarray | list[list[float]] | tuple[tuple[float, ...], ...]) -> np.ndarray:
    array = np.asarray(directions, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] != 3 or array.shape[0] == 0:
        raise ProbeError(ErrorCode.SH_INVALID, "sampling directions must have shape (N, 3)")
    if not np.isfinite(array).all():
        raise ProbeError(ErrorCode.SH_INVALID, "sampling directions contain non-finite values")
    lengths = np.linalg.norm(array, axis=1)
    if np.any(lengths <= 1e-15):
        raise ProbeError(ErrorCode.SH_INVALID, "sampling directions must be non-zero")
    normalized = array / lengths[:, None]
    normalized.setflags(write=False)
    return normalized


def fibonacci_directions(count: int) -> np.ndarray:
    if isinstance(count, bool) or not isinstance(count, (int, np.integer)) or int(count) <= 0:
        raise ProbeError(ErrorCode.SH_INVALID, "direction count must be a positive integer")
    total = int(count)
    indices = np.arange(total, dtype=np.float64)
    y = 1.0 - 2.0 * (indices + 0.5) / total
    radius = np.sqrt(np.maximum(0.0, 1.0 - y * y))
    golden_angle = math.pi * (3.0 - math.sqrt(5.0))
    phi = indices * golden_angle
    directions = np.stack([np.cos(phi) * radius, y, np.sin(phi) * radius], axis=1)
    directions.setflags(write=False)
    return directions


def _associated_legendre(lmax: int, x: np.ndarray) -> dict[tuple[int, int], np.ndarray]:
    """Return P_l^m(x) including the Condon-Shortley phase."""

    values: dict[tuple[int, int], np.ndarray] = {}
    values[(0, 0)] = np.ones_like(x)
    if lmax >= 1:
        values[(1, 0)] = x
    for l_value in range(2, lmax + 1):
        values[(l_value, 0)] = (
            x * (2.0 * l_value - 1.0) * values[(l_value - 1, 0)]
            - (l_value - 1.0) * values[(l_value - 2, 0)]
        ) / float(l_value)
    root = np.sqrt(np.maximum(0.0, 1.0 - x * x))
    for m in range(1, lmax + 1):
        pmm = -(2.0 * m - 1.0) * root * values[(m - 1, m - 1)]
        values[(m, m)] = pmm
        if m + 1 <= lmax:
            pmmp1 = x * (2.0 * m + 1.0) * pmm
            values[(m + 1, m)] = pmmp1
            for l_value in range(m + 2, lmax + 1):
                pll = (
                    x * (2.0 * l_value - 1.0) * values[(l_value - 1, m)]
                    - (l_value + m - 1.0) * values[(l_value - 2, m)]
                ) / float(l_value - m)
                values[(l_value, m)] = pll
    return values


def real_sh_basis(directions: np.ndarray, order: int) -> np.ndarray:
    """Evaluate an orthonormal real SH basis with indices l*l+l+m."""

    actual_order = validate_order(order)
    dirs = normalize_directions(directions)
    x = np.clip(dirs[:, 1], -1.0, 1.0)
    phi = np.arctan2(dirs[:, 2], dirs[:, 0])
    legendre = _associated_legendre(actual_order, x)
    basis = np.empty((dirs.shape[0], sh_coefficient_count(actual_order)), dtype=np.float64)
    for l_value in range(actual_order + 1):
        for m in range(-l_value, l_value + 1):
            abs_m = abs(m)
            p_value = legendre[(l_value, abs_m)]
            normalization = math.sqrt(
                (2.0 * l_value + 1.0)
                / (4.0 * math.pi)
                * math.factorial(l_value - abs_m)
                / math.factorial(l_value + abs_m)
            )
            if abs_m == 0:
                harmonic = normalization * p_value
            elif m > 0:
                harmonic = math.sqrt(2.0) * normalization * np.cos(abs_m * phi) * p_value
            else:
                harmonic = math.sqrt(2.0) * normalization * np.sin(abs_m * phi) * p_value
            basis[:, l_value * l_value + l_value + m] = harmonic
    return basis


@dataclass(frozen=True)
class ProjectionResult:
    coefficients: np.ndarray
    truncation_error: np.ndarray
    visibility_mean: np.ndarray


def project_radiance(
    radiance: np.ndarray,
    directions: np.ndarray,
    order: int,
    visibility: np.ndarray | None = None,
) -> ProjectionResult:
    """Project per-probe sampled radiance.

    This function never mutates caller-owned arrays. Every semantic validation is
    completed before a result is returned, which makes it safe for transactional
    callers.
    """

    actual_order = validate_order(order)
    dirs = normalize_directions(directions)
    coefficient_count = sh_coefficient_count(actual_order)
    if dirs.shape[0] < coefficient_count:
        raise ProbeError(
            ErrorCode.SH_INVALID,
            "direction set does not cover the requested SH basis",
            details={"direction_count": int(dirs.shape[0]), "required": coefficient_count},
        )

    samples = np.asarray(radiance, dtype=np.float64)
    if samples.ndim != 3 or samples.shape[1:] != (dirs.shape[0], 3):
        raise ProbeError(
            ErrorCode.SH_INVALID,
            "radiance must have shape (probe_count, direction_count, 3)",
        )
    if not np.isfinite(samples).all():
        raise ProbeError(ErrorCode.SH_INVALID, "sampled radiance contains non-finite values")
    if np.any(samples < 0.0):
        raise ProbeError(ErrorCode.SH_INVALID, "sampled radiance must be non-negative")
    if visibility is not None:
        vis = np.asarray(visibility, dtype=np.float64)
        if vis.shape != samples.shape[:2] or not np.isfinite(vis).all():
            raise ProbeError(ErrorCode.SH_INVALID, "visibility summary has an invalid shape or value")

    basis = real_sh_basis(dirs, actual_order)
    weights = (4.0 * math.pi) / float(dirs.shape[0])
    coefficients = np.einsum("dk,ndc->nkc", basis, samples, optimize=True) * weights
    reconstructed = np.einsum("dk,nkc->ndc", basis, coefficients, optimize=True)
    residual = reconstructed - samples
    numerator = np.linalg.norm(residual.reshape(samples.shape[0], -1), axis=1)
    denominator = np.maximum(
        np.linalg.norm(samples.reshape(samples.shape[0], -1), axis=1), 1e-12
    )
    truncation_error = numerator / denominator
    visibility_mean = (
        np.mean(visibility, axis=1)
        if visibility is not None
        else np.ones(samples.shape[0], dtype=np.float64)
    )
    coefficients.setflags(write=False)
    truncation_error.setflags(write=False)
    visibility_mean.setflags(write=False)
    return ProjectionResult(coefficients, truncation_error, visibility_mean)


def reconstruct_radiance(coefficients: np.ndarray, directions: np.ndarray) -> np.ndarray:
    coeffs = np.asarray(coefficients, dtype=np.float64)
    dirs = normalize_directions(directions)
    if coeffs.ndim == 2:
        root_count = int(round(math.sqrt(coeffs.shape[0]))) - 1
        actual_order = validate_order(root_count)
        if sh_coefficient_count(actual_order) != coeffs.shape[0]:
            raise ProbeError(ErrorCode.SH_INVALID, "coefficient count is not a valid SH order")
        basis = real_sh_basis(dirs, actual_order)
        return basis @ coeffs
    if coeffs.ndim != 3 or coeffs.shape[2] != 3:
        raise ProbeError(ErrorCode.SH_INVALID, "SH coefficients must have shape (N, K, 3)")
    root_count = int(round(math.sqrt(coeffs.shape[1]))) - 1
    actual_order = validate_order(root_count)
    if sh_coefficient_count(actual_order) != coeffs.shape[1]:
        raise ProbeError(ErrorCode.SH_INVALID, "coefficient count is not a valid SH order")
    basis = real_sh_basis(dirs, actual_order)
    return np.einsum("dk,nkc->ndc", basis, coeffs, optimize=True)


def irradiance_for_normals(
    coefficients: np.ndarray,
    directions: np.ndarray,
    normals: np.ndarray | None = None,
) -> np.ndarray:
    """Cosine-convolved RGB irradiance for each probe and surface normal."""

    coeffs = np.asarray(coefficients, dtype=np.float64)
    dirs = normalize_directions(directions)
    if normals is None:
        normal_array = np.asarray(
            [
                [0.0, 1.0, 0.0],
                [0.0, -1.0, 0.0],
                [1.0, 0.0, 0.0],
                [-1.0, 0.0, 0.0],
                [0.0, 0.0, 1.0],
                [0.0, 0.0, -1.0],
            ],
            dtype=np.float64,
        )
    else:
        normal_array = np.asarray(normals, dtype=np.float64)
    if normal_array.ndim != 2 or normal_array.shape[1] != 3 or not np.isfinite(normal_array).all():
        raise ProbeError(ErrorCode.SH_INVALID, "normals must have shape (N, 3) and be finite")
    normal_lengths = np.linalg.norm(normal_array, axis=1)
    if np.any(normal_lengths <= 1e-15):
        raise ProbeError(ErrorCode.SH_INVALID, "normals must be non-zero")
    normal_array = normal_array / normal_lengths[:, None]

    if coeffs.ndim == 2:
        coeffs = coeffs[None, :, :]
    if coeffs.ndim != 3 or coeffs.shape[2] != 3:
        raise ProbeError(ErrorCode.SH_INVALID, "SH coefficients must have shape (N, K, 3)")
    radiance = reconstruct_radiance(coeffs, dirs)
    weights = (4.0 * math.pi) / float(dirs.shape[0])
    cosine = np.maximum(normal_array @ dirs.T, 0.0) * weights
    return np.einsum("ndc,mq->nmc", radiance, cosine, optimize=True)


def irradiance_up(coefficients: np.ndarray, directions: np.ndarray) -> np.ndarray:
    return irradiance_for_normals(
        coefficients, directions, np.asarray([[0.0, 1.0, 0.0]], dtype=np.float64)
    )[:, 0, :]

