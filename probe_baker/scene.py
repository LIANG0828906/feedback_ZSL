"""Strict local scene parsing and physical input validation."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, TypeAlias

from .canonical import hash_object
from .errors import ErrorCode, ProbeError


def _finite_number(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ProbeError(ErrorCode.SCENE_INVALID, f"{field} must be a number")
    number = float(value)
    if not math.isfinite(number):
        raise ProbeError(ErrorCode.SCENE_INVALID, f"{field} must be finite")
    return number


def _vec3(value: Any, field: str) -> tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise ProbeError(ErrorCode.SCENE_INVALID, f"{field} must contain 3 numbers")
    return tuple(_finite_number(component, f"{field}[{index}]") for index, component in enumerate(value))


def _required(mapping: dict[str, Any], key: str, context: str) -> Any:
    if key not in mapping:
        raise ProbeError(ErrorCode.SCENE_INVALID, f"missing required field {context}.{key}")
    return mapping[key]


def _object(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ProbeError(ErrorCode.SCENE_INVALID, f"{field} must be an object")
    return value


@dataclass(frozen=True)
class BoxOccluder:
    occluder_id: str
    lower: tuple[float, float, float]
    upper: tuple[float, float, float]
    kind: str = "aabb"

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.occluder_id,
            "type": self.kind,
            "min": list(self.lower),
            "max": list(self.upper),
        }

    def radius(self) -> float:
        half = [
            (self.upper[index] - self.lower[index]) * 0.5
            for index in range(3)
        ]
        return math.sqrt(sum(component * component for component in half))


@dataclass(frozen=True)
class SphereOccluder:
    occluder_id: str
    center: tuple[float, float, float]
    radius_value: float
    kind: str = "sphere"

    @property
    def radius(self) -> float:
        return self.radius_value

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.occluder_id,
            "type": self.kind,
            "center": list(self.center),
            "radius": self.radius_value,
        }


Occluder: TypeAlias = BoxOccluder | SphereOccluder


@dataclass(frozen=True)
class Material:
    albedo: tuple[float, float, float]

    def to_dict(self) -> dict[str, Any]:
        return {"albedo": list(self.albedo)}


@dataclass(frozen=True)
class Sky:
    zenith: tuple[float, float, float]
    horizon: tuple[float, float, float]
    ground: tuple[float, float, float]
    intensity: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "zenith": list(self.zenith),
            "horizon": list(self.horizon),
            "ground": list(self.ground),
            "intensity": self.intensity,
        }


@dataclass(frozen=True)
class Scene:
    scene_id: str
    bounds_min: tuple[float, float, float]
    bounds_max: tuple[float, float, float]
    spacing: float
    occluders: tuple[Occluder, ...]
    material: Material
    sky: Sky
    dynamic_mask: frozenset[str] = frozenset()

    def to_dict(self) -> dict[str, Any]:
        return {
            "scene_id": self.scene_id,
            "bounds": {"min": list(self.bounds_min), "max": list(self.bounds_max)},
            "spacing": self.spacing,
            "occluders": [occluder.to_dict() for occluder in self.occluders],
            "material": self.material.to_dict(),
            "sky": self.sky.to_dict(),
            "dynamic_mask": sorted(self.dynamic_mask),
        }

    def occluder_by_id(self, occluder_id: str) -> Occluder | None:
        for occluder in self.occluders:
            if occluder.occluder_id == occluder_id:
                return occluder
        return None

    def active_occluders(self) -> tuple[Occluder, ...]:
        return tuple(
            occluder
            for occluder in self.occluders
            if occluder.occluder_id not in self.dynamic_mask
        )

    def occluder_hash(self, occluder: Occluder) -> str:
        return hash_object(
            {
                "occluder": occluder.to_dict(),
                "masked": occluder.occluder_id in self.dynamic_mask,
            }
        )

    def occluder_hashes(self) -> dict[str, str]:
        return {
            occluder.occluder_id: self.occluder_hash(occluder)
            for occluder in self.occluders
        }

    def factor_hashes(self) -> dict[str, str]:
        return {
            "bounds": hash_object(
                {"min": self.bounds_min, "max": self.bounds_max}
            ),
            "occluders": hash_object(self.occluder_hashes()),
            "material": hash_object(self.material.to_dict()),
            "sky": hash_object(self.sky.to_dict()),
            "dynamic_mask": hash_object(sorted(self.dynamic_mask)),
        }


def parse_scene(payload: Any) -> Scene:
    root = _object(payload, "scene")
    scene_id = _required(root, "scene_id", "scene")
    if not isinstance(scene_id, str) or not scene_id.strip():
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.scene_id must be a non-empty string")

    bounds = _object(_required(root, "bounds", "scene"), "scene.bounds")
    bounds_min = _vec3(_required(bounds, "min", "scene.bounds"), "scene.bounds.min")
    bounds_max = _vec3(_required(bounds, "max", "scene.bounds"), "scene.bounds.max")
    if any(bounds_min[index] >= bounds_max[index] for index in range(3)):
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.bounds.min must be smaller than max on every axis")

    spacing = _finite_number(_required(root, "spacing", "scene"), "scene.spacing")
    if spacing <= 0.0:
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.spacing must be positive")

    raw_occluders = _required(root, "occluders", "scene")
    if not isinstance(raw_occluders, list):
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.occluders must be an array")
    occluders: list[Occluder] = []
    seen_ids: set[str] = set()
    for index, raw_value in enumerate(raw_occluders):
        raw = _object(raw_value, f"scene.occluders[{index}]")
        occluder_id = _required(raw, "id", f"scene.occluders[{index}]")
        if not isinstance(occluder_id, str) or not occluder_id:
            raise ProbeError(ErrorCode.SCENE_INVALID, f"scene.occluders[{index}].id must be a non-empty string")
        if occluder_id in seen_ids:
            raise ProbeError(ErrorCode.SCENE_INVALID, f"duplicate occluder id {occluder_id}")
        seen_ids.add(occluder_id)
        kind = _required(raw, "type", f"scene.occluders[{index}]")
        if kind == "aabb":
            lower = _vec3(_required(raw, "min", f"scene.occluders[{index}]"), f"scene.occluders[{index}].min")
            upper = _vec3(_required(raw, "max", f"scene.occluders[{index}]"), f"scene.occluders[{index}].max")
            if any(lower[axis] >= upper[axis] for axis in range(3)):
                raise ProbeError(
                    ErrorCode.SCENE_INVALID,
                    f"scene.occluders[{index}] has an empty AABB",
                )
            occluders.append(BoxOccluder(occluder_id, lower, upper))
        elif kind == "sphere":
            center = _vec3(_required(raw, "center", f"scene.occluders[{index}]"), f"scene.occluders[{index}].center")
            radius = _finite_number(
                _required(raw, "radius", f"scene.occluders[{index}]"),
                f"scene.occluders[{index}].radius",
            )
            if radius <= 0.0:
                raise ProbeError(ErrorCode.SCENE_INVALID, f"scene.occluders[{index}].radius must be positive")
            occluders.append(SphereOccluder(occluder_id, center, radius))
        else:
            raise ProbeError(
                ErrorCode.SCENE_INVALID,
                f"scene.occluders[{index}].type must be 'aabb' or 'sphere'",
            )

    material_raw = _object(_required(root, "material", "scene"), "scene.material")
    albedo = _vec3(_required(material_raw, "albedo", "scene.material"), "scene.material.albedo")
    if any(component < 0.0 or component > 1.0 for component in albedo):
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.material.albedo must be in [0, 1]")

    sky_raw = _object(_required(root, "sky", "scene"), "scene.sky")
    zenith = _vec3(_required(sky_raw, "zenith", "scene.sky"), "scene.sky.zenith")
    horizon = _vec3(_required(sky_raw, "horizon", "scene.sky"), "scene.sky.horizon")
    ground = _vec3(_required(sky_raw, "ground", "scene.sky"), "scene.sky.ground")
    intensity = _finite_number(_required(sky_raw, "intensity", "scene.sky"), "scene.sky.intensity")
    if intensity < 0.0:
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.sky.intensity must be non-negative")
    if any(component < 0.0 for component in (*zenith, *horizon, *ground)):
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.sky colors must be non-negative")

    raw_mask = root.get("dynamic_mask", [])
    if not isinstance(raw_mask, list) or any(not isinstance(item, str) for item in raw_mask):
        raise ProbeError(ErrorCode.SCENE_INVALID, "scene.dynamic_mask must be an array of strings")
    unknown = sorted(set(raw_mask) - seen_ids)
    if unknown:
        raise ProbeError(
            ErrorCode.SCENE_INVALID,
            f"scene.dynamic_mask references unknown occluders: {unknown}",
        )

    return Scene(
        scene_id=scene_id,
        bounds_min=bounds_min,
        bounds_max=bounds_max,
        spacing=spacing,
        occluders=tuple(occluders),
        material=Material(albedo),
        sky=Sky(zenith, horizon, ground, intensity),
        dynamic_mask=frozenset(raw_mask),
    )


def load_scene(path: str | Path) -> Scene:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise ProbeError(ErrorCode.SCENE_INVALID, f"cannot load scene {path}: {exc}") from exc
    return parse_scene(payload)

