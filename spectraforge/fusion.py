"""Camera inverse response and reliability-weighted exposure fusion."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .models import ExposureInfo
from .parameters import validate_parameters


@dataclass
class FusionResult:
    radiance: np.ndarray
    weights_sum: np.ndarray
    saturated_fraction: float
    underexposed_fraction: float
    fallback_fraction: float
    selected_indices: list[int]


def linearize_exposure(
    raw: np.ndarray,
    exposure_time: float,
    bit_depth: int,
    *,
    gamma: float,
    black_level: float,
) -> tuple[np.ndarray, np.ndarray]:
    if exposure_time <= 0 or not np.isfinite(exposure_time):
        raise WorkbenchError(
            "E_META",
            "Exposure time must be finite and positive",
            {"field": "exposure_time", "value": exposure_time},
        )
    max_code = float((1 << int(bit_depth)) - 1)
    normalized = np.asarray(raw, dtype=np.float64) / max_code
    adjusted = np.clip((normalized - black_level) / max(1.0 - black_level, 1e-12), 0.0, 1.0)
    radiance = np.power(adjusted, float(gamma), dtype=np.float64) / float(exposure_time)
    return radiance, adjusted


def _fusion_weights(
    adjusted: np.ndarray,
    *,
    weight_power: float,
    saturation_threshold: float,
    underexposure_threshold: float,
) -> np.ndarray:
    response = np.clip(4.0 * adjusted * (1.0 - adjusted), 0.0, 1.0)
    weights = np.power(response, weight_power)
    valid = (adjusted > underexposure_threshold) & (adjusted < saturation_threshold)
    return np.where(valid, weights, 0.0)


def fuse_exposures(
    exposure_raw: np.ndarray,
    exposures: list[ExposureInfo],
    params: dict[str, Any],
    camera_response: dict[str, float],
) -> FusionResult:
    count = len(exposures)
    normalized = validate_parameters(params, count)
    indices = normalized["exposure_indices"]
    gamma = float(camera_response.get("gamma", 2.2))
    black_level = float(normalized["black_level"])
    sat_threshold = float(normalized["saturation_threshold"])
    under_threshold = float(normalized["underexposure_threshold"])
    weight_power = float(normalized["weight_power"])
    exposure_bias = float(normalized["exposure_bias"])
    reference_time = max(info.exposure_time for info in exposures)
    numerator = None
    denominator = None
    adjusted_stack = []
    radiance_stack = []
    for index in indices:
        info = exposures[index]
        radiance, adjusted = linearize_exposure(
            exposure_raw[index],
            info.exposure_time,
            info.bit_depth,
            gamma=gamma,
            black_level=black_level,
        )
        weights = _fusion_weights(
            adjusted,
            weight_power=weight_power,
            saturation_threshold=sat_threshold,
            underexposure_threshold=under_threshold,
        )
        weights *= (info.exposure_time / reference_time) ** exposure_bias
        adjusted_stack.append(adjusted)
        radiance_stack.append(radiance)
        if numerator is None:
            numerator = radiance * weights
            denominator = weights
        else:
            numerator += radiance * weights
            denominator += weights
    assert numerator is not None and denominator is not None
    fallback = denominator <= 1e-12
    fallback_fraction = float(fallback.mean())
    if np.any(fallback):
        adjusted_cube = np.stack(adjusted_stack, axis=0)
        radiance_cube = np.stack(radiance_stack, axis=0)
        distance = np.abs(adjusted_cube - 0.5)
        nearest_mid = np.argmin(distance, axis=0)
        fallback_values = np.take_along_axis(
            radiance_cube, nearest_mid[np.newaxis, ...], axis=0
        )[0]
        numerator = np.where(fallback, fallback_values, numerator)
        denominator = np.where(fallback, 1.0, denominator)
    radiance = np.maximum(numerator / denominator, 0.0).astype(np.float32)
    all_saturated = np.all(
        np.stack(adjusted_stack, axis=0) >= sat_threshold, axis=0
    )
    all_under = np.all(
        np.stack(adjusted_stack, axis=0) <= under_threshold, axis=0
    )
    return FusionResult(
        radiance=radiance,
        weights_sum=denominator.astype(np.float32),
        saturated_fraction=float(all_saturated.mean()),
        underexposed_fraction=float(all_under.mean()),
        fallback_fraction=fallback_fraction,
        selected_indices=[int(i) for i in indices],
    )
