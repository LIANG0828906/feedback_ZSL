from __future__ import annotations

import math
from dataclasses import dataclass
from functools import lru_cache

import numpy as np

from .directions import validate_direction_array
from .errors import sh_invalid


def validate_order(order: int) -> int:
    if isinstance(order, bool) or not isinstance(order, int) or not 2 <= order <= 6:
        raise sh_invalid("sh_order", "spherical-harmonic order must be an integer in [2, 6]")
    return order


def coefficient_count(order: int) -> int:
    validate_order(order)
    return (order + 1) * (order + 1)


def _associated_legendre(x: np.ndarray, order: int) -> dict[tuple[int, int], np.ndarray]:
    """Condon-Shortley associated Legendre functions P_l^m on [-1, 1]."""
    x = np.clip(np.asarray(x, dtype=np.float64), -1.0, 1.0)
    one_minus = np.maximum(0.0, 1.0 - x * x)
    values: dict[tuple[int, int], np.ndarray] = {}
    for m in range(order + 1):
        if m == 0:
            p_mm = np.ones_like(x)
        else:
            double_factorial = 1.0
            for value in range(1, 2 * m, 2):
                double_factorial *= value
            p_mm = ((-1.0) ** m) * double_factorial * np.power(one_minus, m * 0.5)
        values[(m, m)] = p_mm
        if m < order:
            values[(m + 1, m)] = x * (2 * m + 1) * p_mm
        for degree in range(m + 2, order + 1):
            values[(degree, m)] = (
                (2 * degree - 1) * x * values[(degree - 1, m)]
                - (degree + m - 1) * values[(degree - 2, m)]
            ) / (degree - m)
    return values


def real_sh_basis(directions: np.ndarray, order: int) -> np.ndarray:
    """Orthonormal real SH basis using y as the polar axis.

    Coefficient index is l*l + l + m, with m from -l through +l.
    """
    validate_order(order)
    directions = validate_direction_array(directions)
    y = directions[:, 1]
    phi = np.arctan2(directions[:, 2], directions[:, 0])
    legendre = _associated_legendre(y, order)
    count = directions.shape[0]
    basis = np.zeros((count, (order + 1) ** 2), dtype=np.float64)
    for degree in range(order + 1):
        for m in range(-degree, degree + 1):
            absolute_m = abs(m)
            p = legendre[(degree, absolute_m)]
            ratio = math.factorial(degree - absolute_m) / math.factorial(
                degree + absolute_m
            )
            normalization = math.sqrt(
                (2 * degree + 1) / (4.0 * math.pi) * ratio
            )
            if absolute_m == 0:
                value = normalization * p
            elif m > 0:
                value = math.sqrt(2.0) * normalization * p * np.cos(absolute_m * phi)
            else:
                value = math.sqrt(2.0) * normalization * p * np.sin(absolute_m * phi)
            basis[:, degree * degree + degree + m] = value
    return basis


@lru_cache(maxsize=8)
def cosine_band_factors(order: int) -> tuple[float, ...]:
    """SH coefficients of max(cos(theta), 0), up to the requested band."""
    validate_order(order)
    nodes, weights = np.polynomial.legendre.leggauss(128)
    x = 0.5 * (nodes + 1.0)
    w = 0.5 * weights
    factors: list[float] = []
    p0 = np.ones_like(x)
    p1 = x
    for degree in range(order + 1):
        if degree == 0:
            p = p0
        elif degree == 1:
            p = p1
        else:
            p = ((2 * degree - 1) * x * p1 - (degree - 1) * p0) / degree
            p0, p1 = p1, p
        factors.append(float(2.0 * math.pi * np.sum(w * p * x)))
    return tuple(factors)


def project_radiance(
    directions: np.ndarray,
    samples: np.ndarray,
    order: int,
) -> tuple[np.ndarray, np.ndarray, float]:
    """Project directional radiance samples and return (coeff, reconstruction, rel_rmse)."""
    validate_order(order)
    directions = validate_direction_array(directions)
    samples = np.asarray(samples, dtype=np.float64)
    if samples.ndim == 1:
        samples = samples[:, None]
    if samples.ndim != 2 or samples.shape[0] != directions.shape[0]:
        raise sh_invalid(
            "samples", "sample count must match the non-empty direction set"
        )
    if samples.shape[1] != 3:
        raise sh_invalid("samples", "radiance samples must be RGB")
    if not np.all(np.isfinite(samples)):
        raise sh_invalid("samples", "radiance samples must be finite")
    basis = real_sh_basis(directions, order)
    coefficients = (4.0 * math.pi / directions.shape[0]) * (basis.T @ samples)
    reconstruction = basis @ coefficients
    residual = samples - reconstruction
    rms = float(math.sqrt(np.mean(residual * residual)))
    reference = float(math.sqrt(np.mean(samples * samples)))
    relative = rms / max(reference, 1.0e-12)
    if np.max(np.abs(coefficients)) <= 1.0e-15:
        raise sh_invalid(
            "samples", "projection produced zero coefficients; refusing a stale/empty bake"
        )
    return coefficients, reconstruction, relative


def radiance_to_irradiance(radiance_coefficients: np.ndarray, order: int) -> np.ndarray:
    validate_order(order)
    coefficients = np.array(radiance_coefficients, dtype=np.float64, copy=True)
    factors = cosine_band_factors(order)
    for degree in range(order + 1):
        start = degree * degree
        stop = (degree + 1) * (degree + 1)
        coefficients[start:stop, :] *= factors[degree]
    return coefficients


def irradiance_to_radiance(irradiance_coefficients: np.ndarray, order: int) -> np.ndarray:
    validate_order(order)
    coefficients = np.array(irradiance_coefficients, dtype=np.float64, copy=True)
    factors = cosine_band_factors(order)
    for degree in range(order + 1):
        if abs(factors[degree]) <= 1.0e-15:
            continue
        start = degree * degree
        stop = (degree + 1) * (degree + 1)
        coefficients[start:stop, :] /= factors[degree]
    return coefficients


def evaluate_irradiance(
    coefficients: np.ndarray, directions: np.ndarray, order: int
) -> np.ndarray:
    validate_order(order)
    coefficients = np.asarray(coefficients, dtype=np.float64)
    if coefficients.shape != (coefficient_count(order), 3):
        raise sh_invalid(
            "coefficients", "coefficient array has the wrong shape for the requested order"
        )
    directions = validate_direction_array(directions)
    return real_sh_basis(directions, order) @ coefficients


def reconstruct_radiance(
    radiance_coefficients: np.ndarray, directions: np.ndarray, order: int
) -> np.ndarray:
    validate_order(order)
    coefficients = np.asarray(radiance_coefficients, dtype=np.float64)
    directions = validate_direction_array(directions)
    return real_sh_basis(directions, order) @ coefficients


def direct_irradiance(
    directions: np.ndarray, radiance: np.ndarray, normals: np.ndarray
) -> np.ndarray:
    """Deterministic Monte-Carlo truth for Lambertian irradiance."""
    directions = validate_direction_array(directions)
    radiance = np.asarray(radiance, dtype=np.float64)
    normals = np.asarray(normals, dtype=np.float64)
    normals = normals / np.linalg.norm(normals, axis=1, keepdims=True)
    cosine = np.maximum(normals @ directions.T, 0.0)
    return (4.0 * math.pi / directions.shape[0]) * (cosine @ radiance)


def luminance(rgb: np.ndarray) -> np.ndarray:
    values = np.asarray(rgb, dtype=np.float64)
    weights = np.asarray((0.2126, 0.7152, 0.0722))
    return values @ weights


def negativity_probe_normals() -> np.ndarray:
    return np.asarray(
        [
            [0.0, 1.0, 0.0],
            [0.0, -1.0, 0.0],
            [1.0, 0.0, 0.0],
            [-1.0, 0.0, 0.0],
            [0.0, 0.0, 1.0],
            [0.0, 0.0, -1.0],
            [0.5773502691896258, 0.5773502691896258, 0.5773502691896258],
            [-0.5773502691896258, 0.5773502691896258, 0.5773502691896258],
        ],
        dtype=np.float64,
    )

