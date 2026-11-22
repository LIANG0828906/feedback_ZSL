"""Processing parameter schema, validation, and immutable version hashing."""

from __future__ import annotations

import copy
from typing import Any

import numpy as np

from .color import GAMUT_PRIMARIES_XY
from .errors import WorkbenchError
from .tone import validate_tone_parameters
from .util import hash_object

DEFAULT_PARAMETERS: dict[str, Any] = {
    "interpolation": "bilinear",
    "exposure_indices": [],
    "weight_power": 2.0,
    "exposure_bias": 1.0,
    "black_level": 0.002,
    "saturation_threshold": 0.995,
    "underexposure_threshold": 0.008,
    "target_gamut": "sRGB",
    "gamut_intent": "compress",
    "tone_curve": "reinhard",
    "tone_exposure": 0.0,
    "tone_key": 0.70,
    "tone_white": 8.0,
    "saturation": 1.0,
    "display_gamma": 2.2,
}

INTERPOLATIONS = {"nearest", "bilinear", "bicubic"}
GAMUT_INTENTS = {"clip", "compress", "strict"}


def default_parameters(exposure_count: int) -> dict[str, Any]:
    params = copy.deepcopy(DEFAULT_PARAMETERS)
    params["exposure_indices"] = list(range(exposure_count))
    return params


def _numeric(params: dict[str, Any], field: str, low: float, high: float) -> float:
    try:
        value = float(params[field])
    except (KeyError, TypeError, ValueError) as exc:
        raise WorkbenchError("E_PARA", "Parameter must be numeric", {"field": field}) from exc
    if not np.isfinite(value) or value < low or value > high:
        raise WorkbenchError(
            "E_PARA",
            "Parameter is outside its allowed range",
            {"field": field, "value": value, "min": low, "max": high},
        )
    return value


def validate_parameters(params: dict[str, Any], exposure_count: int) -> dict[str, Any]:
    if not isinstance(params, dict):
        raise WorkbenchError("E_PARA", "Parameters must be an object", {"field": "parameters"})
    merged = default_parameters(exposure_count)
    for key, value in params.items():
        if key in merged:
            merged[key] = value
    interpolation = str(merged["interpolation"])
    if interpolation not in INTERPOLATIONS:
        raise WorkbenchError(
            "E_PARA",
            "Invalid interpolation method",
            {"field": "interpolation", "value": interpolation, "allowed": sorted(INTERPOLATIONS)},
        )
    indices_value = merged["exposure_indices"]
    if not isinstance(indices_value, (list, tuple)) or not indices_value:
        raise WorkbenchError(
            "E_PARA",
            "At least one exposure frame must be selected",
            {"field": "exposure_indices", "value": indices_value},
        )
    indices: list[int] = []
    for item in indices_value:
        if isinstance(item, bool) or not isinstance(item, (int, np.integer)):
            raise WorkbenchError(
                "E_PARA",
                "Exposure index must be an integer",
                {"field": "exposure_indices", "value": item},
            )
        index = int(item)
        if index < 0 or index >= exposure_count:
            raise WorkbenchError(
                "E_PARA",
                "Exposure index is outside the scene exposure stack",
                {"field": "exposure_indices", "value": index, "count": exposure_count},
            )
        if index in indices:
            raise WorkbenchError(
                "E_PARA",
                "Exposure indices must be unique",
                {"field": "exposure_indices", "value": index},
            )
        indices.append(index)
    merged["exposure_indices"] = sorted(indices)
    merged["weight_power"] = _numeric(merged, "weight_power", 0.25, 12.0)
    merged["exposure_bias"] = _numeric(merged, "exposure_bias", 0.0, 4.0)
    merged["black_level"] = _numeric(merged, "black_level", 0.0, 0.1)
    merged["saturation_threshold"] = _numeric(merged, "saturation_threshold", 0.5, 1.0)
    merged["underexposure_threshold"] = _numeric(merged, "underexposure_threshold", 0.0, 0.2)
    if merged["underexposure_threshold"] >= merged["saturation_threshold"]:
        raise WorkbenchError(
            "E_PARA",
            "Underexposure threshold must be below saturation threshold",
            {
                "field": "underexposure_threshold",
                "value": merged["underexposure_threshold"],
                "saturation_threshold": merged["saturation_threshold"],
            },
        )
    gamut = str(merged["target_gamut"])
    if gamut not in GAMUT_PRIMARIES_XY:
        raise WorkbenchError(
            "E_PARA",
            "Unknown target gamut",
            {"field": "target_gamut", "value": gamut, "allowed": sorted(GAMUT_PRIMARIES_XY)},
        )
    intent = str(merged["gamut_intent"])
    if intent not in GAMUT_INTENTS:
        raise WorkbenchError(
            "E_PARA",
            "Unknown gamut mapping intent",
            {"field": "gamut_intent", "value": intent, "allowed": sorted(GAMUT_INTENTS)},
        )
    validate_tone_parameters(merged)
    return merged


def parameter_version(params: dict[str, Any], exposure_count: int) -> str:
    normalized = validate_parameters(params, exposure_count)
    return hash_object(normalized)
