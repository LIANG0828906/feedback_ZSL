"""Parameter grouping, canonical serialization and version tracking."""

from __future__ import annotations

import copy
import hashlib
import json
import math
from dataclasses import dataclass, field
from typing import Iterable, Mapping

from .errors import E_SCENE_INVALID, AppError


GROUPS: tuple[str, ...] = ("camera", "geometry", "material", "light")


def canonical_json_bytes(value: object) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _finite(value: object, field_name: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise AppError(E_SCENE_INVALID, "expected a finite number", field_name) from exc
    if not math.isfinite(number):
        raise AppError(E_SCENE_INVALID, "expected a finite number", field_name)
    return number


def _vector3(value: object, field_name: str) -> list[float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise AppError(E_SCENE_INVALID, "expected a three-element vector", field_name)
    return [_finite(item, f"{field_name}.{index}") for index, item in enumerate(value)]


@dataclass
class ParameterSet:
    """Flat scalar parameters grouped by optimisation family.

    A flat representation keeps cache keys, reduction order and checkpoint
    serialization independent from Python dictionary insertion order.
    """

    values: dict[str, dict[str, float]]
    versions: dict[str, int] = field(default_factory=lambda: {group: 0 for group in GROUPS})
    sphere_count: int = 0
    _order: dict[str, tuple[str, ...]] = field(default_factory=dict, repr=False)

    def __post_init__(self) -> None:
        missing = [group for group in GROUPS if group not in self.values]
        if missing:
            raise ValueError(f"missing parameter groups: {missing}")
        # Sorted keys make vector order independent of dict insertion order.
        # Checkpoints use sort_keys=True, so this is required for byte-stable
        # resume and gradient-to-key mapping.
        self._order = {
            group: tuple(sorted(self.values[group].keys())) for group in GROUPS
        }

    @classmethod
    def from_scene_dict(cls, scene: Mapping[str, object]) -> "ParameterSet":
        camera = _mapping(scene.get("camera"), "camera")
        geometry = _mapping(scene.get("geometry"), "geometry")
        material = _mapping(scene.get("material"), "material")
        light = _mapping(scene.get("light"), "light")

        position = _vector3(camera.get("position"), "camera.position")
        rotation = _vector3(camera.get("rotation"), "camera.rotation")
        intrinsics = camera.get("intrinsics", camera)
        intrinsics = _mapping(intrinsics, "camera.intrinsics")
        camera_values: dict[str, float] = {}
        for axis in range(3):
            camera_values[f"position.{axis}"] = position[axis]
            camera_values[f"rotation.{axis}"] = rotation[axis]
        for key in ("fx", "fy", "cx", "cy"):
            camera_values[key] = _finite(intrinsics.get(key), f"camera.intrinsics.{key}")

        spheres_raw = geometry.get("spheres")
        if not isinstance(spheres_raw, list) or not spheres_raw:
            raise AppError(E_SCENE_INVALID, "at least one sphere is required", "geometry.spheres")
        sphere_albedos = material.get("sphere_albedo")
        if not isinstance(sphere_albedos, list) or len(sphere_albedos) != len(spheres_raw):
            raise AppError(
                E_SCENE_INVALID,
                "sphere_albedo count must equal geometry.spheres count",
                "material.sphere_albedo",
            )

        geometry_values: dict[str, float] = {}
        material_values: dict[str, float] = {}
        for index, sphere_raw in enumerate(spheres_raw):
            sphere = _mapping(sphere_raw, f"geometry.spheres.{index}")
            center = _vector3(sphere.get("center"), f"geometry.spheres.{index}.center")
            radius = _finite(sphere.get("radius"), f"geometry.spheres.{index}.radius")
            if radius <= 1.0e-9:
                raise AppError(
                    E_SCENE_INVALID,
                    "sphere radius must be positive",
                    f"geometry.spheres.{index}.radius",
                )
            for axis in range(3):
                geometry_values[f"sphere.{index}.center.{axis}"] = center[axis]
            geometry_values[f"sphere.{index}.radius"] = radius
            albedo = _vector3(
                sphere_albedos[index], f"material.sphere_albedo.{index}"
            )
            for channel in range(3):
                material_values[f"sphere.{index}.albedo.{channel}"] = albedo[channel]

        plane = _mapping(geometry.get("plane"), "geometry.plane")
        plane_point = _vector3(plane.get("point"), "geometry.plane.point")
        plane_normal = _vector3(plane.get("normal"), "geometry.plane.normal")
        normal_norm = math.sqrt(sum(component * component for component in plane_normal))
        if normal_norm <= 1.0e-9:
            raise AppError(
                E_SCENE_INVALID,
                "plane normal must be non-degenerate",
                "geometry.plane.normal",
            )
        for axis in range(3):
            geometry_values[f"plane.point.{axis}"] = plane_point[axis]
            geometry_values[f"plane.normal.{axis}"] = plane_normal[axis]

        plane_albedo = _vector3(material.get("plane_albedo"), "material.plane_albedo")
        for channel in range(3):
            material_values[f"plane.albedo.{channel}"] = plane_albedo[channel]
        material_values["ambient"] = _finite(
            material.get("ambient", 0.04), "material.ambient"
        )

        direction = _vector3(light.get("direction"), "light.direction")
        direction_norm = math.sqrt(sum(component * component for component in direction))
        if direction_norm <= 1.0e-9:
            raise AppError(
                E_SCENE_INVALID,
                "light direction must be non-degenerate",
                "light.direction",
            )
        light_values = {
            f"direction.{axis}": direction[axis] for axis in range(3)
        }
        light_values["intensity"] = _finite(
            light.get("intensity"), "light.intensity"
        )
        if light_values["intensity"] < 0.0:
            raise AppError(
                E_SCENE_INVALID,
                "light intensity must be non-negative",
                "light.intensity",
            )

        return cls(
            values={
                "camera": camera_values,
                "geometry": geometry_values,
                "material": material_values,
                "light": light_values,
            },
            sphere_count=len(spheres_raw),
        )

    def clone(self) -> "ParameterSet":
        return ParameterSet(
            values=copy.deepcopy(self.values),
            versions=dict(self.versions),
            sphere_count=self.sphere_count,
        )

    def keys(self, group: str) -> tuple[str, ...]:
        return self._order[group]

    def vector(self, group: str) -> list[float]:
        return [float(self.values[group][key]) for key in self._order[group]]

    def content_hash(self, group: str) -> str:
        payload = [[key, self.values[group][key]] for key in self._order[group]]
        return sha256_bytes(canonical_json_bytes(payload))

    def content_hashes(self) -> dict[str, str]:
        return {group: self.content_hash(group) for group in GROUPS}

    def signature(self, groups: Iterable[str] = GROUPS) -> tuple[tuple[str, int, str], ...]:
        selected = tuple(groups)
        return tuple(
            (group, self.versions[group], self.content_hash(group))
            for group in selected
        )

    def bump(self, groups: Iterable[str]) -> None:
        for group in groups:
            if group not in self.versions:
                raise KeyError(group)
            self.versions[group] += 1

    def update(self, group: str, updates: Mapping[str, float]) -> None:
        if group not in self.values:
            raise KeyError(group)
        unknown = sorted(set(updates) - set(self.values[group]))
        if unknown:
            raise AppError(
                E_SCENE_INVALID,
                "unknown parameter key",
                f"{group}.{unknown[0]}",
            )
        for key, value in updates.items():
            self.values[group][key] = _finite(value, f"{group}.{key}")
        self.versions[group] += 1

    def add_vector(self, group: str, delta: list[float], scale: float = 1.0) -> None:
        keys = self._order[group]
        if len(delta) != len(keys):
            raise ValueError("gradient length does not match parameter group")
        for key, component in zip(keys, delta):
            self.values[group][key] += scale * float(component)
        self.versions[group] += 1

    def nested(self) -> dict[str, object]:
        result: dict[str, object] = {
            "camera": {
                "position": [self.values["camera"][f"position.{axis}"] for axis in range(3)],
                "rotation": [self.values["camera"][f"rotation.{axis}"] for axis in range(3)],
                "intrinsics": {
                    key: self.values["camera"][key] for key in ("fx", "fy", "cx", "cy")
                },
            },
            "geometry": {
                "spheres": [],
                "plane": {
                    "point": [
                        self.values["geometry"][f"plane.point.{axis}"]
                        for axis in range(3)
                    ],
                    "normal": [
                        self.values["geometry"][f"plane.normal.{axis}"]
                        for axis in range(3)
                    ],
                },
            },
            "material": {
                "sphere_albedo": [],
                "plane_albedo": [
                    self.values["material"][f"plane.albedo.{channel}"]
                    for channel in range(3)
                ],
                "ambient": self.values["material"]["ambient"],
            },
            "light": {
                "direction": [
                    self.values["light"][f"direction.{axis}"] for axis in range(3)
                ],
                "intensity": self.values["light"]["intensity"],
            },
        }
        for index in range(self.sphere_count):
            result["geometry"]["spheres"].append(
                {
                    "center": [
                        self.values["geometry"][f"sphere.{index}.center.{axis}"]
                        for axis in range(3)
                    ],
                    "radius": self.values["geometry"][f"sphere.{index}.radius"],
                }
            )
            result["material"]["sphere_albedo"].append(
                [
                    self.values["material"][f"sphere.{index}.albedo.{channel}"]
                    for channel in range(3)
                ]
            )
        return result

    def to_dict(self) -> dict[str, object]:
        return {
            "values": {
                group: {key: self.values[group][key] for key in self._order[group]}
                for group in GROUPS
            },
            "versions": dict(self.versions),
            "sphere_count": self.sphere_count,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> "ParameterSet":
        values = data.get("values")
        versions = data.get("versions")
        sphere_count = data.get("sphere_count")
        if not isinstance(values, dict) or not isinstance(versions, dict):
            raise ValueError("invalid parameter checkpoint data")
        if not isinstance(sphere_count, int) or sphere_count < 1:
            raise ValueError("invalid sphere_count")
        converted: dict[str, dict[str, float]] = {}
        for group in GROUPS:
            source = values.get(group)
            if not isinstance(source, dict):
                raise ValueError(f"missing parameter group {group}")
            converted[group] = {
                str(key): _finite(value, f"{group}.{key}")
                for key, value in source.items()
            }
        converted_versions = {
            group: int(versions.get(group, 0)) for group in GROUPS
        }
        return cls(
            values=converted,
            versions=converted_versions,
            sphere_count=sphere_count,
        )


def _mapping(value: object, field_name: str) -> Mapping[str, object]:
    if not isinstance(value, Mapping):
        raise AppError(E_SCENE_INVALID, "expected an object", field_name)
    return value

