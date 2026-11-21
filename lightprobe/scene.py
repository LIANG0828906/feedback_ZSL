from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import scene_invalid
from .util import finite_float, sha256_json, vec3


@dataclass(frozen=True)
class Material:
    name: str
    albedo: tuple[float, float, float]
    bounce_gain: float = 0.18

    def fingerprint(self) -> dict[str, Any]:
        return {
            "id": self.name,
            "albedo": list(self.albedo),
            "bounce_gain": self.bounce_gain,
        }

    def content_hash(self) -> str:
        return sha256_json(self.fingerprint())


@dataclass(frozen=True)
class AABB:
    id: str
    minimum: tuple[float, float, float]
    maximum: tuple[float, float, float]
    material: str
    kind: str = "aabb"

    def fingerprint(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.kind,
            "min": list(self.minimum),
            "max": list(self.maximum),
            "material": self.material,
        }

    @property
    def center(self) -> tuple[float, float, float]:
        return tuple((a + b) * 0.5 for a, b in zip(self.minimum, self.maximum))  # type: ignore[return-value]

    @property
    def radius(self) -> float:
        return 0.5 * math.dist(self.minimum, self.maximum)


@dataclass(frozen=True)
class Sphere:
    id: str
    center: tuple[float, float, float]
    radius: float
    material: str
    kind: str = "sphere"

    def fingerprint(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.kind,
            "center": list(self.center),
            "radius": self.radius,
            "material": self.material,
        }


Occluder = AABB | Sphere


@dataclass(frozen=True)
class Sky:
    zenith_radiance: tuple[float, float, float]
    horizon_radiance: tuple[float, float, float]
    ground_radiance: tuple[float, float, float]
    intensity: float = 1.0

    def fingerprint(self) -> dict[str, Any]:
        return {
            "zenith_radiance": list(self.zenith_radiance),
            "horizon_radiance": list(self.horizon_radiance),
            "ground_radiance": list(self.ground_radiance),
            "intensity": self.intensity,
        }

    def content_hash(self) -> str:
        return sha256_json(self.fingerprint())

    def radiance(self, directions: np.ndarray) -> np.ndarray:
        """Return RGB sky radiance for unit directions using y as elevation."""
        directions = np.asarray(directions, dtype=np.float64)
        elevation = np.clip(directions[:, 1], -1.0, 1.0)
        zenith = np.asarray(self.zenith_radiance, dtype=np.float64)
        horizon = np.asarray(self.horizon_radiance, dtype=np.float64)
        ground = np.asarray(self.ground_radiance, dtype=np.float64)

        up = elevation >= 0.0
        blend = np.zeros_like(elevation)
        blend[up] = np.sqrt(elevation[up])
        blend[~up] = np.sqrt(-elevation[~up])
        upper = horizon[None, :] * (1.0 - blend[:, None]) + zenith[None, :] * blend[:, None]
        lower = horizon[None, :] * (1.0 - blend[:, None]) + ground[None, :] * blend[:, None]
        result = np.where(up[:, None], upper, lower)
        return result * self.intensity

    def average_radiance(self) -> np.ndarray:
        return (
            np.asarray(self.zenith_radiance)
            + 2.0 * np.asarray(self.horizon_radiance)
            + np.asarray(self.ground_radiance)
        ) * (0.25 * self.intensity)


@dataclass(frozen=True)
class SceneDefaults:
    probe_spacing: float = 2.0
    sh_order: int = 3
    direction_count: int = 64
    direction_seed: int = 0
    compensation_strength: float = 0.35
    strategy: str = "auto"

    def fingerprint(self) -> dict[str, Any]:
        return {
            "probe_spacing": self.probe_spacing,
            "sh_order": self.sh_order,
            "direction_count": self.direction_count,
            "direction_seed": self.direction_seed,
            "compensation_strength": self.compensation_strength,
            "strategy": self.strategy,
        }


@dataclass(frozen=True)
class Scene:
    scene_id: str
    bounds_min: tuple[float, float, float]
    bounds_max: tuple[float, float, float]
    occluders: tuple[Occluder, ...]
    materials: dict[str, Material]
    sky: Sky
    defaults: SceneDefaults
    source_path: str | None = None

    def sorted_occluders(self) -> tuple[Occluder, ...]:
        return tuple(sorted(self.occluders, key=lambda item: item.id))

    def sorted_materials(self) -> tuple[Material, ...]:
        return tuple(self.materials[name] for name in sorted(self.materials))

    @property
    def occluder_ids(self) -> tuple[str, ...]:
        return tuple(item.id for item in self.sorted_occluders())

    def occluder(self, object_id: str) -> Occluder | None:
        return next((item for item in self.occluders if item.id == object_id), None)

    def object_fingerprint(self, object_id: str) -> dict[str, Any] | None:
        occluder = self.occluder(object_id)
        if occluder is None:
            return None
        return {
            "geometry": occluder.fingerprint(),
            "material": self.materials[occluder.material].fingerprint(),
        }

    def object_hash(self, object_id: str) -> str | None:
        fingerprint = self.object_fingerprint(object_id)
        return None if fingerprint is None else sha256_json(fingerprint)

    def geometry_hash(self, object_id: str) -> str | None:
        occluder = self.occluder(object_id)
        return None if occluder is None else sha256_json(occluder.fingerprint())

    def content_hash(self) -> str:
        return sha256_json(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "scene_id": self.scene_id,
            "bounds": {"min": list(self.bounds_min), "max": list(self.bounds_max)},
            "settings": self.defaults.fingerprint(),
            "sky": self.sky.fingerprint(),
            "materials": {
                name: material.fingerprint() for name, material in sorted(self.materials.items())
            },
            "occluders": [item.fingerprint() for item in self.occluders],
        }

    def with_updates(self, updates: dict[str, Any]) -> "Scene":
        """Apply a UI/API update document and fully revalidate the scene."""
        payload = self.to_dict()
        settings = dict(payload["settings"])
        nested_settings = updates.get("settings", {})
        if not isinstance(nested_settings, dict):
            raise scene_invalid("settings", "expected an object")
        settings.update(nested_settings)
        for key in (
            "probe_spacing",
            "sh_order",
            "direction_count",
            "direction_seed",
            "compensation_strength",
            "strategy",
        ):
            if key in updates:
                settings[key] = updates[key]
        payload["settings"] = settings
        if "bounds" in updates:
            bounds_updates = updates["bounds"]
            if not isinstance(bounds_updates, dict):
                raise scene_invalid("bounds", "expected an object")
            payload["bounds"].update(bounds_updates)
        if "sky" in updates:
            payload["sky"].update(updates["sky"])
        if "materials" in updates:
            for name, material_updates in updates["materials"].items():
                if name not in payload["materials"]:
                    raise scene_invalid(f"materials.{name}", "unknown material", object_id=name)
                payload["materials"][name].update(material_updates)
        occluder_updates = updates.get("occluders", {})
        if occluder_updates:
            known = {item["id"] for item in payload["occluders"]}
            for object_id, object_updates in occluder_updates.items():
                if object_id not in known:
                    raise scene_invalid(
                        f"occluders.{object_id}", "unknown occluder", object_id=object_id
                    )
                for item in payload["occluders"]:
                    if item["id"] == object_id:
                        item.update(object_updates)
                        break
        return load_scene_dict(payload, source_path=self.source_path)


def _require_mapping(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise scene_invalid(path, "expected an object")
    return value


def _require_key(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        raise scene_invalid(f"{path}.{key}", "missing required field")
    return mapping[key]


def load_scene(path: str | Path) -> Scene:
    scene_path = Path(path)
    try:
        payload = json.loads(scene_path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise scene_invalid("$", f"scene file not found: {scene_path}") from exc
    except json.JSONDecodeError as exc:
        raise scene_invalid("$", f"invalid JSON at line {exc.lineno}, column {exc.colno}") from exc
    return load_scene_dict(payload, source_path=str(scene_path))


def load_scene_dict(payload: Any, *, source_path: str | None = None) -> Scene:
    root = _require_mapping(payload, "$")
    scene_id = _require_key(root, "scene_id", "$")
    if not isinstance(scene_id, str) or not scene_id.strip():
        raise scene_invalid("$.scene_id", "expected a non-empty string")

    bounds = _require_mapping(_require_key(root, "bounds", "$"), "$.bounds")
    bounds_min = vec3(_require_key(bounds, "min", "$.bounds"), "$.bounds.min")
    bounds_max = vec3(_require_key(bounds, "max", "$.bounds"), "$.bounds.max")
    if any(high <= low for low, high in zip(bounds_min, bounds_max)):
        raise scene_invalid("$.bounds", "bounds must have positive extent on every axis")

    materials_raw = _require_mapping(_require_key(root, "materials", "$"), "$.materials")
    if not materials_raw:
        raise scene_invalid("$.materials", "at least one material is required")
    materials: dict[str, Material] = {}
    for name, item in materials_raw.items():
        path = f"$.materials.{name}"
        if not isinstance(name, str) or not name:
            raise scene_invalid("$.materials", "material names must be non-empty strings")
        mapping = _require_mapping(item, path)
        albedo = vec3(_require_key(mapping, "albedo", path), f"{path}.albedo", nonnegative=True)
        bounce_gain = finite_float(mapping.get("bounce_gain", 0.18), f"{path}.bounce_gain")
        if bounce_gain < 0.0:
            raise scene_invalid(f"{path}.bounce_gain", "must be non-negative", object_id=name)
        materials[name] = Material(name=name, albedo=albedo, bounce_gain=bounce_gain)

    sky_raw = _require_mapping(_require_key(root, "sky", "$"), "$.sky")
    sky = Sky(
        zenith_radiance=vec3(
            _require_key(sky_raw, "zenith_radiance", "$.sky"),
            "$.sky.zenith_radiance",
            nonnegative=True,
        ),
        horizon_radiance=vec3(
            _require_key(sky_raw, "horizon_radiance", "$.sky"),
            "$.sky.horizon_radiance",
            nonnegative=True,
        ),
        ground_radiance=vec3(
            _require_key(sky_raw, "ground_radiance", "$.sky"),
            "$.sky.ground_radiance",
            nonnegative=True,
        ),
        intensity=finite_float(sky_raw.get("intensity", 1.0), "$.sky.intensity"),
    )
    if sky.intensity <= 0.0:
        raise scene_invalid("$.sky.intensity", "must be positive")
    if float(np.sum(sky.average_radiance())) <= 0.0:
        raise scene_invalid("$.sky", "sky radiance must not be identically zero")

    occluders_raw = _require_key(root, "occluders", "$")
    if not isinstance(occluders_raw, list):
        raise scene_invalid("$.occluders", "expected an array")
    occluders: list[Occluder] = []
    seen_ids: set[str] = set()
    for index, item in enumerate(occluders_raw):
        path = f"$.occluders[{index}]"
        mapping = _require_mapping(item, path)
        object_id = _require_key(mapping, "id", path)
        if not isinstance(object_id, str) or not object_id:
            raise scene_invalid(f"{path}.id", "expected a non-empty string")
        if object_id in seen_ids:
            raise scene_invalid(f"{path}.id", "duplicate occluder id", object_id=object_id)
        seen_ids.add(object_id)
        kind = _require_key(mapping, "type", path)
        material_name = _require_key(mapping, "material", path)
        if material_name not in materials:
            raise scene_invalid(
                f"{path}.material", "references an unknown material", object_id=object_id
            )
        if kind in {"aabb", "box"}:
            minimum = vec3(
                _require_key(mapping, "min", path), f"{path}.min"
            )
            maximum = vec3(
                _require_key(mapping, "max", path), f"{path}.max"
            )
            if any(high <= low for low, high in zip(minimum, maximum)):
                raise scene_invalid(
                    path, "degenerate axis-aligned box (max must exceed min)", object_id=object_id
                )
            occluders.append(
                AABB(id=object_id, minimum=minimum, maximum=maximum, material=material_name)
            )
        elif kind == "sphere":
            center = vec3(_require_key(mapping, "center", path), f"{path}.center")
            radius = finite_float(_require_key(mapping, "radius", path), f"{path}.radius")
            if radius <= 0.0:
                raise scene_invalid(f"{path}.radius", "must be positive", object_id=object_id)
            occluders.append(
                Sphere(
                    id=object_id,
                    center=center,
                    radius=radius,
                    material=material_name,
                )
            )
        else:
            raise scene_invalid(f"{path}.type", "supported types are 'aabb' and 'sphere'", object_id=object_id)

    settings_raw = root.get("settings", {})
    settings = _require_mapping(settings_raw, "$.settings")
    defaults = SceneDefaults(
        probe_spacing=finite_float(settings.get("probe_spacing", 2.0), "$.settings.probe_spacing"),
        sh_order=_parse_int(settings.get("sh_order", 3), "$.settings.sh_order"),
        direction_count=_parse_int(
            settings.get("direction_count", 64), "$.settings.direction_count"
        ),
        direction_seed=_parse_int(
            settings.get("direction_seed", 0), "$.settings.direction_seed"
        ),
        compensation_strength=finite_float(
            settings.get("compensation_strength", 0.35),
            "$.settings.compensation_strength",
        ),
        strategy=str(settings.get("strategy", "auto")),
    )
    if defaults.probe_spacing <= 0.0:
        raise scene_invalid("$.settings.probe_spacing", "must be positive")
    if not 0.0 <= defaults.compensation_strength <= 2.0:
        raise scene_invalid("$.settings.compensation_strength", "must be in [0, 2]")

    return Scene(
        scene_id=scene_id,
        bounds_min=bounds_min,
        bounds_max=bounds_max,
        occluders=tuple(occluders),
        materials=materials,
        sky=sky,
        defaults=defaults,
        source_path=source_path,
    )


def _parse_int(value: Any, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise scene_invalid(path, "expected an integer")
    return value


def apply_occluder_updates(scene: Scene, updates: Iterable[dict[str, Any]]) -> Scene:
    """Convenience API used by deterministic sample mutation tests."""
    payload = scene.to_dict()
    by_id = {item["id"]: item for item in payload["occluders"]}
    for update in updates:
        object_id = update.get("id")
        if object_id not in by_id:
            raise scene_invalid("$.occluders", "unknown occluder", object_id=str(object_id))
        target = by_id[object_id]
        for key, value in update.items():
            if key != "id":
                target[key] = value
    return load_scene_dict(payload, source_path=scene.source_path)

