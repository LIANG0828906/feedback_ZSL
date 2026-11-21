"""Immutable render and scheduling configuration."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import AppError, ErrorCode


@dataclass(frozen=True, slots=True)
class DegradationStage:
    name: str
    step_scale: float


@dataclass(frozen=True, slots=True)
class RenderConfig:
    tile_width: int
    tile_height: int
    max_workers: int
    sample_budget: int
    base_step: float
    shadow_samples: int
    shadow_distance: float
    max_distance: float
    seam_tolerance: float
    degradation_sequence: tuple[DegradationStage, ...]
    precision_paths: tuple[str, ...] = ("float64", "fixed")
    fixed_scale: int = 1 << 20

    def digest(self) -> str:
        payload = {
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
            "max_workers": self.max_workers,
            "sample_budget": self.sample_budget,
            "base_step": self.base_step,
            "shadow_samples": self.shadow_samples,
            "shadow_distance": self.shadow_distance,
            "max_distance": self.max_distance,
            "seam_tolerance": self.seam_tolerance,
            "degradation_sequence": [
                {"name": stage.name, "step_scale": stage.step_scale}
                for stage in self.degradation_sequence
            ],
            "precision_paths": list(self.precision_paths),
            "fixed_scale": self.fixed_scale,
        }
        encoded = json.dumps(
            payload, sort_keys=True, separators=(",", ":"), allow_nan=False
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()

    def stage(self, index: int) -> DegradationStage:
        return self.degradation_sequence[index]

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
            "max_workers": self.max_workers,
            "sample_budget": self.sample_budget,
            "base_step": self.base_step,
            "shadow_samples": self.shadow_samples,
            "shadow_distance": self.shadow_distance,
            "max_distance": self.max_distance,
            "seam_tolerance": self.seam_tolerance,
            "degradation_sequence": [
                {"name": stage.name, "step_scale": stage.step_scale}
                for stage in self.degradation_sequence
            ],
            "precision_paths": list(self.precision_paths),
            "fixed_scale": self.fixed_scale,
        }


def _require(data: dict[str, Any], key: str, expected: type, field_name: str) -> Any:
    if key not in data:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            f"missing configuration field: {key}",
            field=field_name,
            injection_point=field_name,
        )
    value = data[key]
    if expected is int and (isinstance(value, bool) or not isinstance(value, int)):
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            f"configuration field {key} must be an integer",
            field=field_name,
            injection_point=field_name,
        )
    if expected is float and (
        isinstance(value, bool) or not isinstance(value, (int, float))
    ):
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            f"configuration field {key} must be numeric",
            field=field_name,
            injection_point=field_name,
        )
    return value


def parse_config(data: dict[str, Any]) -> RenderConfig:
    if not isinstance(data, dict):
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "configuration root must be an object",
            field="config",
        )

    tile_width = _require(data, "tile_width", int, "tile_width")
    tile_height = _require(data, "tile_height", int, "tile_height")
    max_workers = _require(data, "max_workers", int, "max_workers")
    sample_budget = _require(data, "sample_budget", int, "sample_budget")
    base_step = float(_require(data, "base_step", float, "base_step"))
    shadow_samples = _require(data, "shadow_samples", int, "shadow_samples")
    shadow_distance = float(
        _require(data, "shadow_distance", float, "shadow_distance")
    )
    max_distance = float(_require(data, "max_distance", float, "max_distance"))
    seam_tolerance = float(
        _require(data, "seam_tolerance", float, "seam_tolerance")
    )

    if tile_width <= 0 or tile_height <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "tile dimensions must be positive",
            field="tile_width" if tile_width <= 0 else "tile_height",
        )
    if max_workers <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "max_workers must be positive",
            field="max_workers",
        )
    if sample_budget <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "sample_budget must be positive",
            field="sample_budget",
            injection_point="config.sample_budget",
        )
    if not math.isfinite(base_step) or base_step <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "base_step must be finite and positive",
            field="base_step",
        )
    if shadow_samples < 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "shadow_samples must be non-negative",
            field="shadow_samples",
        )
    if not math.isfinite(shadow_distance) or shadow_distance <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "shadow_distance must be finite and positive",
            field="shadow_distance",
        )
    if not math.isfinite(max_distance) or max_distance <= 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "max_distance must be finite and positive",
            field="max_distance",
        )
    if not math.isfinite(seam_tolerance) or seam_tolerance < 0:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "seam_tolerance must be finite and non-negative",
            field="seam_tolerance",
        )

    raw_stages = data.get("degradation_sequence")
    if not isinstance(raw_stages, list) or not raw_stages:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "degradation_sequence must be a non-empty array",
            field="degradation_sequence",
        )
    stages: list[DegradationStage] = []
    previous_scale = 0.0
    seen_names: set[str] = set()
    for index, item in enumerate(raw_stages):
        point = f"degradation_sequence[{index}]"
        if not isinstance(item, dict):
            raise AppError(
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "degradation stage must be an object",
                field=point,
            )
        name = item.get("name")
        scale = item.get("step_scale")
        if not isinstance(name, str) or not name:
            raise AppError(
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "degradation stage name must be non-empty",
                field=f"{point}.name",
            )
        if name in seen_names:
            raise AppError(
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "degradation stage names must be unique",
                field=f"{point}.name",
            )
        if (
            isinstance(scale, bool)
            or not isinstance(scale, (int, float))
            or not math.isfinite(float(scale))
            or float(scale) < 1.0
        ):
            raise AppError(
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "step_scale must be finite and >= 1.0",
                field=f"{point}.step_scale",
            )
        numeric_scale = float(scale)
        if numeric_scale < previous_scale:
            raise AppError(
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "degradation step_scale must be non-decreasing",
                field=f"{point}.step_scale",
            )
        previous_scale = numeric_scale
        seen_names.add(name)
        stages.append(DegradationStage(name=name, step_scale=numeric_scale))

    paths = data.get("precision_paths", ["float64", "fixed"])
    if (
        not isinstance(paths, list)
        or not paths
        or any(path not in {"float64", "fixed"} for path in paths)
        or len(set(paths)) != len(paths)
    ):
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "precision_paths must be unique values from float64/fixed",
            field="precision_paths",
        )

    fixed_scale = data.get("fixed_scale", 1 << 20)
    if (
        isinstance(fixed_scale, bool)
        or not isinstance(fixed_scale, int)
        or fixed_scale < 256
    ):
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            "fixed_scale must be an integer >= 256",
            field="fixed_scale",
        )

    return RenderConfig(
        tile_width=tile_width,
        tile_height=tile_height,
        max_workers=max_workers,
        sample_budget=sample_budget,
        base_step=base_step,
        shadow_samples=shadow_samples,
        shadow_distance=shadow_distance,
        max_distance=max_distance,
        seam_tolerance=seam_tolerance,
        degradation_sequence=tuple(stages),
        precision_paths=tuple(paths),
        fixed_scale=fixed_scale,
    )


def load_config(path: str | Path) -> RenderConfig:
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise AppError(
            ErrorCode.E_IO,
            f"cannot read configuration: {exc}",
            field="config",
        ) from exc
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            f"configuration is not valid UTF-8 JSON: {exc}",
            field="config",
        ) from exc
    return parse_config(data)
