"""Anisotropic phase functions and single-scattering quadrature."""

from __future__ import annotations

import math
from typing import Literal

import numpy as np

from .errors import parameter_error

SamplingStrategy = Literal["midpoint", "left", "right", "trapezoid"]
VALID_STRATEGIES = ("midpoint", "left", "right", "trapezoid")


def hg_phase(cos_theta: float, anisotropy: float) -> float:
    if not math.isfinite(anisotropy) or anisotropy < -1.0 or anisotropy > 1.0:
        raise parameter_error("light.anisotropy", "anisotropy must lie in [-1, 1]")
    g = max(-0.999999, min(0.999999, float(anisotropy)))
    cosine = max(-1.0, min(1.0, float(cos_theta)))
    denominator = 1.0 + g * g - 2.0 * g * cosine
    denominator = max(1.0e-15, denominator)
    return (1.0 - g * g) / (4.0 * math.pi * denominator ** 1.5)


def phase_cosine(view_direction: np.ndarray, light_direction: np.ndarray) -> float:
    view = np.asarray(view_direction, dtype=np.float64)
    light = np.asarray(light_direction, dtype=np.float64)
    view_norm = float(np.linalg.norm(view))
    light_norm = float(np.linalg.norm(light))
    if view_norm <= 0.0 or light_norm <= 0.0:
        raise parameter_error("scattering.direction", "view and light directions must be non-zero")
    # Light direction is propagation direction, so incoming radiance travels -d.
    return float(np.dot(view / view_norm, -light / light_norm))


def _validate_quadrature(length: float, step_size: float, strategy: str) -> None:
    if not math.isfinite(length) or length < 0.0:
        raise parameter_error("scattering.length", "segment length must be finite and non-negative")
    if not math.isfinite(step_size) or step_size <= 0.0 or step_size > 100.0:
        raise parameter_error("render.step_size", "step size must be finite and in (0, 100]")
    if strategy not in VALID_STRATEGIES:
        raise parameter_error("render.sampling_strategy", f"expected one of {VALID_STRATEGIES}")


def integrate_segment(
    *,
    length: float,
    sigma_s: float,
    sigma_t: float,
    radiance: float,
    phase: float,
    step_size: float,
    strategy: SamplingStrategy = "midpoint",
    base_optical_depth: float = 0.0,
) -> float:
    """Numerically integrate T(entry -> x) * sigma_s * phase * radiance ds."""

    _validate_quadrature(length, step_size, strategy)
    if not math.isfinite(sigma_s) or sigma_s < 0.0:
        raise parameter_error("medium.scattering", "scattering coefficient must be finite and non-negative")
    if not math.isfinite(sigma_t) or sigma_t < 0.0:
        raise parameter_error("medium.extinction", "extinction coefficient must be finite and non-negative")
    if not math.isfinite(radiance) or radiance < 0.0:
        raise parameter_error("light.radiance", "radiance must be finite and non-negative")
    if not math.isfinite(phase) or phase < 0.0:
        raise parameter_error("scattering.phase", "phase function value must be finite and non-negative")
    if length <= 0.0 or sigma_s <= 0.0 or radiance <= 0.0 or phase <= 0.0:
        return 0.0

    steps = max(1, int(math.ceil(length / step_size - 1.0e-12)))
    delta = length / steps
    scale = math.exp(-max(0.0, base_optical_depth)) * sigma_s * radiance * phase
    if sigma_t == 0.0:
        return scale * length

    if strategy == "trapezoid":
        positions = np.arange(steps + 1, dtype=np.float64) * delta
        values = np.exp(-sigma_t * positions)
        integral = float(
            delta
            * (
                0.5 * values[0]
                + np.sum(values[1:-1], dtype=np.float64)
                + 0.5 * values[-1]
            )
        )
    else:
        positions = np.arange(steps, dtype=np.float64) * delta
        if strategy == "midpoint":
            positions += 0.5 * delta
        elif strategy == "right":
            positions += delta
        integral = float(np.sum(np.exp(-sigma_t * positions)) * delta)
    return scale * integral


def analytic_segment(
    *,
    length: float,
    sigma_s: float,
    sigma_t: float,
    radiance: float,
    phase: float,
    base_optical_depth: float = 0.0,
) -> float:
    if length < 0.0 or sigma_s < 0.0 or sigma_t < 0.0 or radiance < 0.0 or phase < 0.0:
        raise parameter_error("scattering.analytic", "inputs must be non-negative")
    scale = math.exp(-base_optical_depth) * sigma_s * radiance * phase
    if sigma_t <= 0.0:
        return scale * length
    return scale * (1.0 - math.exp(-sigma_t * length)) / sigma_t


def preview_error_bound(step_size: float, length: float, sigma_t: float, sigma_s: float, radiance: float, phase: float) -> float:
    """Conservative midpoint-rule bound recorded for reduced-step previews.

    The bound uses the second derivative envelope of exp(-sigma_t s). It is an
    upper bound for the quadrature term multiplied by the remaining factors.
    """

    effective_second_derivative = sigma_t * sigma_t
    coefficient = sigma_s * radiance * phase
    return 0.5 * length * step_size * step_size * coefficient * effective_second_derivative


__all__ = [
    "VALID_STRATEGIES",
    "analytic_segment",
    "hg_phase",
    "integrate_segment",
    "phase_cosine",
    "preview_error_bound",
]
