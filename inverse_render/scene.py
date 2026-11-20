"""Local scene description parsing and structural signatures."""

from __future__ import annotations

import json
import math
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .errors import invalid_scene
from .hashing import sha256_json


@dataclass(frozen=True)
class CameraSpec:
    position: tuple[float, float, float]
    target: tuple[float, float, float]
    up: tuple[float, float, float]
    fov_deg: float


@dataclass(frozen=True)
class SphereSpec:
    name: str
    center: tuple[float, float, float]
    radius: float


@dataclass(frozen=True)
class PlaneSpec:
    name: str
    point: tuple[float, float, float]
    normal: tuple[float, float, float]


@dataclass(frozen=True)
class LightSpec:
    direction: tuple[float, float, float]
    intensity: float


@dataclass(frozen=True)
class Scene:
    name: str
    width: int
    height: int
    camera: CameraSpec
    spheres: tuple[SphereSpec, ...]
    plane: PlaneSpec
    albedos: Mapping[str, tuple[float, float, float]]
    light: LightSpec
    target: np.ndarray
    target_path: str
    shadow_softness: float
    coupling_level: int
    source_path: str
    raw: Mapping[str, Any]

    @property
    def resolution(self) -> tuple[int, int]:
        return (self.width, self.height)

    def parameter_kinds(self) -> OrderedDict[str, str]:
        paths: OrderedDict[str, str] = OrderedDict()
        paths["camera.position"] = "camera"
        paths["camera.target"] = "camera"
        paths["camera.up"] = "camera"
        paths["camera.fov_deg"] = "camera"
        for sphere in self.spheres:
            paths[f"geometry.{sphere.name}.center"] = "geometry"
            paths[f"geometry.{sphere.name}.radius"] = "geometry"
        paths[f"geometry.{self.plane.name}.point"] = "geometry"
        paths[f"geometry.{self.plane.name}.normal"] = "geometry"
        for sphere in self.spheres:
            paths[f"material.{sphere.name}.albedo"] = "material"
        paths[f"material.{self.plane.name}.albedo"] = "material"
        paths["light.direction"] = "light"
        paths["light.intensity"] = "light"
        return paths

    def initial_parameters(self) -> OrderedDict[str, np.ndarray]:
        values: OrderedDict[str, np.ndarray] = OrderedDict()
        values["camera.position"] = np.asarray(self.camera.position, dtype=np.float64)
        values["camera.target"] = np.asarray(self.camera.target, dtype=np.float64)
        values["camera.up"] = np.asarray(self.camera.up, dtype=np.float64)
        values["camera.fov_deg"] = np.asarray(self.camera.fov_deg, dtype=np.float64)
        for sphere in self.spheres:
            values[f"geometry.{sphere.name}.center"] = np.asarray(
                sphere.center, dtype=np.float64
            )
            values[f"geometry.{sphere.name}.radius"] = np.asarray(
                sphere.radius, dtype=np.float64
            )
        values[f"geometry.{self.plane.name}.point"] = np.asarray(
            self.plane.point, dtype=np.float64
        )
        values[f"geometry.{self.plane.name}.normal"] = np.asarray(
            self.plane.normal, dtype=np.float64
        )
        for sphere in self.spheres:
            values[f"material.{sphere.name}.albedo"] = np.asarray(
                self.albedos[sphere.name], dtype=np.float64
            )
        values[f"material.{self.plane.name}.albedo"] = np.asarray(
            self.albedos[self.plane.name], dtype=np.float64
        )
        values["light.direction"] = np.asarray(self.light.direction, dtype=np.float64)
        values["light.intensity"] = np.asarray(self.light.intensity, dtype=np.float64)
        return values

    def object_names(self) -> tuple[str, ...]:
        return tuple(sphere.name for sphere in self.spheres) + (self.plane.name,)

    def structure(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("name", self.name),
                ("resolution", [self.width, self.height]),
                ("spheres", [sphere.name for sphere in self.spheres]),
                ("plane", self.plane.name),
                ("objects", list(self.object_names())),
                ("parameter_paths", list(self.parameter_kinds())),
                ("shadow_softness", self.shadow_softness),
            )
        )

    def structure_hash(self) -> str:
        return sha256_json(self.structure())

    def scene_hash(self) -> str:
        """Hash every value that affects fitting, including the target array."""

        payload = OrderedDict(
            (
                ("structure", self.structure()),
                ("initial_parameters", self.initial_parameters()),
                ("target", self.target),
            )
        )
        return sha256_json(payload)


def _vec3(value: Any, field: str) -> tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise invalid_scene(f"{field} must contain exactly three numbers", field=field)
    result: list[float] = []
    for item in value:
        if isinstance(item, bool) or not isinstance(item, (int, float)):
            raise invalid_scene(f"{field} contains a non-numeric value", field=field)
        number = float(item)
        if not math.isfinite(number):
            raise invalid_scene(f"{field} contains a non-finite value", field=field)
        result.append(number)
    return (result[0], result[1], result[2])


def _finite(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise invalid_scene(f"{field} must be numeric", field=field)
    number = float(value)
    if not math.isfinite(number):
        raise invalid_scene(f"{field} must be finite", field=field)
    return number


def _object_name(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise invalid_scene(f"{field} must be a non-empty string", field=field)
    if any(character in value for character in (".", "/", "\\", "\0")):
        raise invalid_scene(
            f"{field} may not contain '.', slash, or NUL characters", field=field
        )
    return value


def _mapping(value: Any, field: str) -> Mapping[str, Any]:
    if not isinstance(value, dict):
        raise invalid_scene(f"{field} must be an object", field=field)
    return value


def load_scene(path: str | Path, *, target_override: np.ndarray | None = None) -> Scene:
    scene_path = Path(path).resolve()
    try:
        with open(scene_path, "r", encoding="utf-8") as handle:
            raw = json.load(handle, object_pairs_hook=OrderedDict)
    except FileNotFoundError as exc:
        raise invalid_scene("scene file does not exist", path=str(scene_path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise invalid_scene("scene file is not valid JSON", path=str(scene_path)) from exc

    if raw.get("schema_version", 1) != 1:
        raise invalid_scene("unsupported scene schema_version")

    name = raw.get("name", scene_path.stem)
    if not isinstance(name, str) or not name:
        raise invalid_scene("scene name must be a non-empty string")

    resolution = raw.get("resolution")
    if not isinstance(resolution, (list, tuple)) or len(resolution) != 2:
        raise invalid_scene("resolution must be [width, height]")
    width, height = resolution
    if (
        isinstance(width, bool)
        or isinstance(height, bool)
        or not isinstance(width, int)
        or not isinstance(height, int)
        or width <= 0
        or height <= 0
        or width > 1024
        or height > 1024
    ):
        raise invalid_scene("resolution dimensions must be integers in [1, 1024]")

    camera_raw = _mapping(raw.get("camera"), "camera")
    camera = CameraSpec(
        position=_vec3(camera_raw.get("position"), "camera.position"),
        target=_vec3(camera_raw.get("target"), "camera.target"),
        up=_vec3(camera_raw.get("up"), "camera.up"),
        fov_deg=_finite(camera_raw.get("fov_deg"), "camera.fov_deg"),
    )
    if camera.fov_deg <= 1.0 or camera.fov_deg >= 179.0:
        raise invalid_scene("camera.fov_deg must be in (1, 179)")
    if camera.position == camera.target:
        raise invalid_scene("camera position and target must differ")
    if float(np.linalg.norm(np.cross(camera.target - np.asarray(camera.position), camera.up))) <= 1e-9:
        raise invalid_scene("camera.up may not be parallel to the view direction")

    geometry_raw = _mapping(raw.get("geometry"), "geometry")
    spheres_raw = geometry_raw.get("spheres")
    if not isinstance(spheres_raw, list) or len(spheres_raw) < 2:
        raise invalid_scene("geometry.spheres must contain at least two spheres")
    spheres: list[SphereSpec] = []
    inline_albedos: dict[str, Any] = {}
    seen_names: set[str] = set()
    for index, item in enumerate(spheres_raw):
        item = _mapping(item, f"geometry.spheres[{index}]")
        sphere_name = _object_name(item.get("name"), f"geometry.spheres[{index}].name")
        if sphere_name in seen_names:
            raise invalid_scene("object names must be unique", name=sphere_name)
        seen_names.add(sphere_name)
        if item.get("albedo") is not None:
            inline_albedos[sphere_name] = item.get("albedo")
        radius = _finite(item.get("radius"), f"geometry.spheres[{index}].radius")
        if radius <= 1e-6:
            raise invalid_scene("sphere radius must be positive", name=sphere_name)
        spheres.append(
            SphereSpec(
                name=sphere_name,
                center=_vec3(item.get("center"), f"geometry.spheres[{index}].center"),
                radius=radius,
            )
        )

    plane_raw = _mapping(geometry_raw.get("plane"), "geometry.plane")
    plane_name = _object_name(plane_raw.get("name"), "geometry.plane.name")
    if plane_name in seen_names:
        raise invalid_scene("object names must be unique", name=plane_name)
    seen_names.add(plane_name)
    plane = PlaneSpec(
        name=plane_name,
        point=_vec3(plane_raw.get("point"), "geometry.plane.point"),
        normal=_vec3(plane_raw.get("normal"), "geometry.plane.normal"),
    )
    if float(np.linalg.norm(np.asarray(plane.normal))) <= 1e-9:
        raise invalid_scene("plane normal may not be zero")

    materials_value = raw.get("materials", {})
    materials_raw = _mapping(materials_value, "materials")
    if plane_raw.get("albedo") is not None:
        inline_albedos[plane_name] = plane_raw.get("albedo")
    albedos: OrderedDict[str, tuple[float, float, float]] = OrderedDict()
    for object_name in tuple(sphere.name for sphere in spheres) + (plane.name,):
        material_value = materials_raw.get(object_name)
        if material_value is None:
            if object_name in inline_albedos:
                material_value = {"albedo": inline_albedos[object_name]}
            else:
                raise invalid_scene(
                    "object has no material or inline albedo",
                    object=object_name,
                )
        material = _mapping(material_value, f"materials.{object_name}")
        albedo = _vec3(material.get("albedo"), f"materials.{object_name}.albedo")
        if any(channel < 0.0 or channel > 4.0 for channel in albedo):
            raise invalid_scene(
                "albedo channels must be in [0, 4]",
                object=object_name,
            )
        albedos[object_name] = albedo
    extra_materials = set(materials_raw) - set(albedos)
    if extra_materials:
        raise invalid_scene(
            "materials contains unknown objects", objects=sorted(extra_materials)
        )

    light_raw = _mapping(raw.get("light"), "light")
    light = LightSpec(
        direction=_vec3(light_raw.get("direction"), "light.direction"),
        intensity=_finite(light_raw.get("intensity"), "light.intensity"),
    )
    if float(np.linalg.norm(np.asarray(light.direction))) <= 1e-9:
        raise invalid_scene("light.direction may not be zero")
    if light.intensity < 0.0:
        raise invalid_scene("light.intensity must be non-negative")

    shadow_softness = _finite(raw.get("shadow_softness", 0.035), "shadow_softness")
    if shadow_softness <= 0.0 or shadow_softness > 1.0:
        raise invalid_scene("shadow_softness must be in (0, 1]")
    coupling_level = raw.get("coupling_level", 0)
    if coupling_level not in (0, 1, 2):
        raise invalid_scene("coupling_level must be 0, 1, or 2")

    target_raw = _mapping(raw.get("target"), "target")
    target_path = target_raw.get("path")
    if not isinstance(target_path, str) or not target_path:
        raise invalid_scene("target.path must be a non-empty string")
    resolved_target = (scene_path.parent / target_path).resolve()
    if target_override is None:
        try:
            target = np.load(resolved_target, allow_pickle=False)
        except FileNotFoundError as exc:
            raise invalid_scene(
                "target array does not exist", target_path=str(resolved_target)
            ) from exc
        except (OSError, ValueError) as exc:
            raise invalid_scene(
                "target array is not a valid .npy file",
                target_path=str(resolved_target),
            ) from exc
    else:
        target = np.asarray(target_override)
    target = np.asarray(target, dtype=np.float64)
    expected_shape = (height, width, 3)
    if target.shape != expected_shape:
        raise invalid_scene(
            "target shape does not match resolution",
            expected=list(expected_shape),
            actual=list(target.shape),
        )
    if not np.all(np.isfinite(target)):
        raise invalid_scene("target contains non-finite values")
    target.setflags(write=False)

    return Scene(
        name=name,
        width=width,
        height=height,
        camera=camera,
        spheres=tuple(spheres),
        plane=plane,
        albedos=albedos,
        light=light,
        target=target,
        target_path=str(resolved_target),
        shadow_softness=shadow_softness,
        coupling_level=int(coupling_level),
        source_path=str(scene_path),
        raw=raw,
    )

