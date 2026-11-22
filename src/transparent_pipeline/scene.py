"""Scene loading and structural validation."""

from __future__ import annotations

import copy
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import (
    PipelineError,
    geometry_error,
    parameter_error,
    schema_error,
)
from .util import finite_float, read_json, sha256_json, vec3


SCENE_SCHEMA_VERSION = "1.0"
TRUTH_SCHEMA_VERSION = "1.0"


def _require(mapping: dict[str, Any], key: str, field: str) -> Any:
    if key not in mapping:
        raise schema_error(f"{field}.{key}", "required field is missing")
    return mapping[key]


def _require_object(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise schema_error(field, "expected an object")
    return value


def _require_list(value: Any, field: str, min_length: int = 0) -> list[Any]:
    if not isinstance(value, list):
        raise schema_error(field, "expected an array")
    if len(value) < min_length:
        raise schema_error(field, f"expected at least {min_length} item(s)")
    return value


def _finite_array(value: Any, field: str, *, nonnegative: bool = False) -> np.ndarray:
    if not isinstance(value, list) or not value:
        raise parameter_error(field, "expected a non-empty numeric array")
    for index, item in enumerate(value):
        if isinstance(item, bool) or not isinstance(item, (int, float)):
            raise parameter_error(f"{field}[{index}]", "expected a finite number")
    array = np.asarray(value, dtype=np.float64)
    if not np.all(np.isfinite(array)):
        raise parameter_error(field, "all values must be finite")
    if nonnegative and np.any(array < 0.0):
        raise parameter_error(field, "all values must be non-negative")
    return array


def _validate_boundary(boundary: dict[str, Any], field: str) -> None:
    kind = _require(boundary, "type", field)
    if not isinstance(kind, str):
        raise schema_error(f"{field}.type", "expected a string")
    if kind == "sphere":
        center = vec3(_require(boundary, "center", field), f"{field}.center")
        radius = finite_float(_require(boundary, "radius", field), f"{field}.radius")
        if radius <= 0.0 or not np.all(np.isfinite(center)):
            raise geometry_error(f"{field}.radius", "sphere boundary must have finite center and radius > 0")
    elif kind == "box":
        lower = vec3(_require(boundary, "min", field), f"{field}.min")
        upper = vec3(_require(boundary, "max", field), f"{field}.max")
        if not np.all(np.isfinite(lower)) or not np.all(np.isfinite(upper)):
            raise geometry_error(f"{field}.min", "box bounds must be finite")
        if np.any(upper <= lower):
            raise geometry_error(f"{field}.max", "box boundary is not closed: max must exceed min on every axis")
    elif kind == "mesh":
        vertices = _require_list(_require(boundary, "vertices", field), f"{field}.vertices", 3)
        faces = _require_list(_require(boundary, "faces", field), f"{field}.faces", 1)
        for index, vertex in enumerate(vertices):
            vec3(vertex, f"{field}.vertices[{index}]")
        edge_counts: dict[tuple[int, int], int] = {}
        for face_index, face in enumerate(faces):
            if not isinstance(face, list) or len(face) != 3:
                raise schema_error(f"{field}.faces[{face_index}]", "mesh faces must contain three vertex indices")
            if any(isinstance(item, bool) or not isinstance(item, int) for item in face):
                raise schema_error(f"{field}.faces[{face_index}]", "mesh vertex indices must be integers")
            if any(item < 0 or item >= len(vertices) for item in face):
                raise geometry_error(f"{field}.faces[{face_index}]", "mesh vertex index is out of range")
            if len(set(face)) != 3:
                raise geometry_error(f"{field}.faces[{face_index}]", "mesh face must reference three distinct vertices")
            for first, second in ((face[0], face[1]), (face[1], face[2]), (face[2], face[0])):
                edge = (first, second) if first < second else (second, first)
                edge_counts[edge] = edge_counts.get(edge, 0) + 1
        open_edges = sorted(edge for edge, count in edge_counts.items() if count != 2)
        if open_edges:
            raise geometry_error(
                f"{field}.faces",
                "mesh boundary is not closed: every undirected edge must occur exactly twice",
                open_edges=open_edges,
            )
    else:
        raise geometry_error(f"{field}.type", f"unsupported boundary type {kind!r}")


def _validate_camera(camera: dict[str, Any], field: str) -> None:
    width = _require(camera, "width", field)
    height = _require(camera, "height", field)
    if isinstance(width, bool) or not isinstance(width, int) or width <= 0:
        raise parameter_error(f"{field}.width", "camera width must be a positive integer")
    if isinstance(height, bool) or not isinstance(height, int) or height <= 0:
        raise parameter_error(f"{field}.height", "camera height must be a positive integer")
    fov = finite_float(_require(camera, "fov_y_deg", field), f"{field}.fov_y_deg")
    if not 0.0 < fov < 180.0:
        raise parameter_error(f"{field}.fov_y_deg", "vertical field of view must be in (0, 180)")
    near_far = _require_list(_require(camera, "near_far", field), f"{field}.near_far", 2)
    near = finite_float(near_far[0], f"{field}.near_far[0]")
    far = finite_float(near_far[1], f"{field}.near_far[1]")
    if near <= 0.0 or far <= near:
        raise parameter_error(f"{field}.near_far", "near/far planes must satisfy 0 < near < far")
    trajectory = _require_list(_require(camera, "trajectory", field), f"{field}.trajectory", 2)
    times: list[float] = []
    for index, sample in enumerate(trajectory):
        sample_field = f"{field}.trajectory[{index}]"
        sample = _require_object(sample, sample_field)
        time = finite_float(_require(sample, "time", sample_field), f"{sample_field}.time")
        times.append(time)
        eye = vec3(_require(sample, "eye", sample_field), f"{sample_field}.eye")
        target = vec3(_require(sample, "target", sample_field), f"{sample_field}.target")
        up = vec3(_require(sample, "up", sample_field), f"{sample_field}.up")
        if float(np.linalg.norm(up)) <= 0.0:
            raise parameter_error(f"{sample_field}.up", "camera up vector must be non-zero")
        forward = target - eye
        forward_length = float(np.linalg.norm(forward))
        if forward_length <= 0.0:
            raise parameter_error(f"{sample_field}.target", "camera target must differ from eye")
        if float(np.linalg.norm(np.cross(forward, up))) <= 1.0e-12:
            raise parameter_error(f"{sample_field}.up", "camera up vector must not be parallel to view direction")
    if any(times[index] <= times[index - 1] for index in range(1, len(times))):
        raise parameter_error(f"{field}.trajectory", "camera sample times must be strictly increasing")
    if "default_sample_index" in camera:
        default_index = camera["default_sample_index"]
        if isinstance(default_index, bool) or not isinstance(default_index, int):
            raise parameter_error(f"{field}.default_sample_index", "default sample index must be an integer")
        if not 0 <= default_index < len(trajectory):
            raise parameter_error(f"{field}.default_sample_index", "default sample index is out of range")


def validate_scene_dict(data: Any) -> dict[str, Any]:
    if not isinstance(data, dict):
        raise schema_error("scene", "scene must be an object")
    for key in ("schema_version", "seed", "scene_id", "wavelengths_nm", "media", "lights", "camera"):
        _require(data, key, "scene")
    if data["schema_version"] != SCENE_SCHEMA_VERSION:
        raise schema_error("scene.schema_version", f"expected {SCENE_SCHEMA_VERSION!r}")
    if isinstance(data["seed"], bool) or not isinstance(data["seed"], int):
        raise schema_error("scene.seed", "seed must be an integer")
    if not isinstance(data["scene_id"], str) or not data["scene_id"]:
        raise schema_error("scene.scene_id", "scene_id must be a non-empty string")

    wavelengths = _finite_array(_require(data, "wavelengths_nm", "scene"), "scene.wavelengths_nm")
    if np.any(wavelengths <= 0.0):
        raise parameter_error("scene.wavelengths_nm", "wavelengths must be positive")
    band_count = len(wavelengths)

    media = _require_list(data["media"], "scene.media", 1)
    media_ids: set[str] = set()
    for media_index, medium in enumerate(media):
        field = f"scene.media[{media_index}]"
        medium = _require_object(medium, field)
        for key in ("id", "kind", "boundary", "ior", "absorption", "scattering"):
            _require(medium, key, field)
        media_id = medium["id"]
        if not isinstance(media_id, str) or not media_id:
            raise schema_error(f"{field}.id", "medium id must be a non-empty string")
        if media_id in media_ids:
            raise schema_error(f"{field}.id", f"duplicate medium id {media_id!r}")
        media_ids.add(media_id)
        if not isinstance(medium["kind"], str) or not medium["kind"]:
            raise schema_error(f"{field}.kind", "medium kind must be a non-empty string")
        boundary = _require_object(medium["boundary"], f"{field}.boundary")
        _validate_boundary(boundary, f"{field}.boundary")
        ior = _finite_array(medium["ior"], f"{field}.ior")
        if len(ior) != band_count:
            raise parameter_error(f"{field}.ior", "refractive index length must match wavelength band count")
        if np.any(ior <= 1.0):
            raise parameter_error(f"{field}.ior", "refractive index must be greater than 1.0")
        absorption = _finite_array(medium["absorption"], f"{field}.absorption", nonnegative=True)
        scattering = _finite_array(medium["scattering"], f"{field}.scattering", nonnegative=True)
        if len(absorption) != band_count:
            raise parameter_error(f"{field}.absorption", "absorption length must match wavelength band count")
        if len(scattering) != band_count:
            raise parameter_error(f"{field}.scattering", "scattering length must match wavelength band count")

    lights = _require_list(data["lights"], "scene.lights", 1)
    light_ids: set[str] = set()
    for light_index, light in enumerate(lights):
        field = f"scene.lights[{light_index}]"
        light = _require_object(light, field)
        for key in ("id", "type", "direction", "radiance", "anisotropy"):
            _require(light, key, field)
        light_id = light["id"]
        if not isinstance(light_id, str) or not light_id:
            raise schema_error(f"{field}.id", "light id must be a non-empty string")
        if light_id in light_ids:
            raise schema_error(f"{field}.id", f"duplicate light id {light_id!r}")
        light_ids.add(light_id)
        if light["type"] != "directional_volume":
            raise schema_error(f"{field}.type", "only directional_volume lights are supported")
        direction = vec3(light["direction"], f"{field}.direction")
        if float(np.linalg.norm(direction)) <= 0.0:
            raise parameter_error(f"{field}.direction", "light direction must be non-zero")
        radiance = _finite_array(light["radiance"], f"{field}.radiance", nonnegative=True)
        anisotropy = _finite_array(light["anisotropy"], f"{field}.anisotropy")
        if len(radiance) != band_count:
            raise parameter_error(f"{field}.radiance", "radiance length must match wavelength band count")
        if len(anisotropy) != band_count:
            raise parameter_error(f"{field}.anisotropy", "anisotropy length must match wavelength band count")
        if np.any(anisotropy < -1.0) or np.any(anisotropy > 1.0):
            raise parameter_error(f"{field}.anisotropy", "anisotropy values must lie in [-1, 1]")

    camera = _require_object(data["camera"], "scene.camera")
    _validate_camera(camera, "scene.camera")
    return data


@dataclass
class Scene:
    """Mutable scene wrapper with a stable content hash."""

    data: dict[str, Any]

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Scene":
        validate_scene_dict(data)
        return cls(copy.deepcopy(data))

    @classmethod
    def load(cls, path: str | Path) -> "Scene":
        return cls.from_dict(load_scene(path))

    @property
    def wavelengths(self) -> np.ndarray:
        return np.asarray(self.data["wavelengths_nm"], dtype=np.float64)

    @property
    def band_count(self) -> int:
        return len(self.data["wavelengths_nm"])

    @property
    def media(self) -> list[dict[str, Any]]:
        return self.data["media"]

    @property
    def lights(self) -> list[dict[str, Any]]:
        return self.data["lights"]

    @property
    def camera(self) -> dict[str, Any]:
        return self.data["camera"]

    @property
    def media_by_id(self) -> dict[str, dict[str, Any]]:
        return {medium["id"]: medium for medium in self.media}

    @property
    def light_by_id(self) -> dict[str, dict[str, Any]]:
        return {light["id"]: light for light in self.lights}

    @property
    def content_hash(self) -> str:
        return sha256_json(self.data)

    def copy(self) -> "Scene":
        return Scene(copy.deepcopy(self.data))

    def to_dict(self) -> dict[str, Any]:
        return copy.deepcopy(self.data)

    def replace_media(self, medium: dict[str, Any]) -> None:
        candidate = copy.deepcopy(self.data)
        for index, existing in enumerate(candidate["media"]):
            if existing["id"] == medium["id"]:
                candidate["media"][index] = copy.deepcopy(medium)
                validate_scene_dict(candidate)
                self.data = candidate
                return
        raise schema_error("scene.media.id", f"unknown medium {medium['id']!r}")

    def replace_light(self, light: dict[str, Any]) -> None:
        candidate = copy.deepcopy(self.data)
        for index, existing in enumerate(candidate["lights"]):
            if existing["id"] == light["id"]:
                candidate["lights"][index] = copy.deepcopy(light)
                validate_scene_dict(candidate)
                self.data = candidate
                return
        raise schema_error("scene.lights.id", f"unknown light {light['id']!r}")


def load_scene(path: str | Path) -> dict[str, Any]:
    return validate_scene_dict(read_json(path, "scene"))


def validate_truth_dict(truth: Any, scene: Scene | dict[str, Any] | None = None) -> dict[str, Any]:
    if not isinstance(truth, dict):
        raise schema_error("truth", "truth must be an object")
    for key in ("schema_version", "scene_hash", "wavelengths_nm", "boundary_exit", "ray_transmittance", "ray_scattering"):
        _require(truth, key, "truth")
    if truth["schema_version"] != TRUTH_SCHEMA_VERSION:
        raise schema_error("truth.schema_version", f"expected {TRUTH_SCHEMA_VERSION!r}")
    if scene is not None:
        expected_hash = scene.content_hash if isinstance(scene, Scene) else sha256_json(scene)
        if truth["scene_hash"] != expected_hash:
            raise parameter_error("truth.scene_hash", "truth file does not belong to the loaded scene")
    wavelengths = _finite_array(truth["wavelengths_nm"], "truth.wavelengths_nm")
    scene_media: set[str] | None = None
    scene_lights: set[str] | None = None
    if scene is not None:
        scene_data = scene.data if isinstance(scene, Scene) else scene
        scene_media = {medium["id"] for medium in scene_data["media"]}
        scene_lights = {light["id"] for light in scene_data["lights"]}
    for collection in ("boundary_exit", "ray_transmittance", "ray_scattering"):
        _require_list(truth[collection], f"truth.{collection}")
    for index, case in enumerate(truth["boundary_exit"]):
        field = f"truth.boundary_exit[{index}]"
        case = _require_object(case, field)
        for key in ("id", "medium_id", "wavelength_index", "origin", "direction", "expected_exit", "tolerance"):
            _require(case, key, field)
        vec3(case["origin"], f"{field}.origin")
        vec3(case["direction"], f"{field}.direction")
        vec3(case["expected_exit"], f"{field}.expected_exit")
        if not 0 <= int(case["wavelength_index"]) < len(wavelengths):
            raise parameter_error(f"{field}.wavelength_index", "wavelength index is out of range")
        if scene_media is not None and case["medium_id"] not in scene_media:
            raise schema_error(f"{field}.medium_id", "truth references an unknown medium")
        finite_float(case["tolerance"], f"{field}.tolerance")
    for index, case in enumerate(truth["ray_transmittance"]):
        field = f"truth.ray_transmittance[{index}]"
        case = _require_object(case, field)
        for key in ("id", "medium_id", "wavelength_index", "origin", "direction", "expected_path_length", "expected_transmittance", "tolerance"):
            _require(case, key, field)
        vec3(case["origin"], f"{field}.origin")
        vec3(case["direction"], f"{field}.direction")
        finite_float(case["expected_path_length"], f"{field}.expected_path_length")
        finite_float(case["expected_transmittance"], f"{field}.expected_transmittance")
        if not 0 <= int(case["wavelength_index"]) < len(wavelengths):
            raise parameter_error(f"{field}.wavelength_index", "wavelength index is out of range")
        if scene_media is not None and case["medium_id"] not in scene_media:
            raise schema_error(f"{field}.medium_id", "truth references an unknown medium")
        finite_float(case["tolerance"], f"{field}.tolerance")
    for index, case in enumerate(truth["ray_scattering"]):
        field = f"truth.ray_scattering[{index}]"
        case = _require_object(case, field)
        for key in ("id", "medium_id", "light_id", "wavelength_index", "origin", "direction", "expected_integral", "tolerance"):
            _require(case, key, field)
        vec3(case["origin"], f"{field}.origin")
        vec3(case["direction"], f"{field}.direction")
        finite_float(case["expected_integral"], f"{field}.expected_integral")
        if not 0 <= int(case["wavelength_index"]) < len(wavelengths):
            raise parameter_error(f"{field}.wavelength_index", "wavelength index is out of range")
        if scene_media is not None and case["medium_id"] not in scene_media:
            raise schema_error(f"{field}.medium_id", "truth references an unknown medium")
        if scene_lights is not None and case["light_id"] not in scene_lights:
            raise schema_error(f"{field}.light_id", "truth references an unknown light")
        finite_float(case["tolerance"], f"{field}.tolerance")
    return truth


def load_truth(path: str | Path, scene: Scene | None = None) -> dict[str, Any]:
    return validate_truth_dict(read_json(path, "truth"), scene)


def iter_media(scene: Scene) -> Iterable[dict[str, Any]]:
    return iter(scene.media)


__all__ = [
    "SCENE_SCHEMA_VERSION",
    "TRUTH_SCHEMA_VERSION",
    "Scene",
    "load_scene",
    "load_truth",
    "validate_scene_dict",
    "validate_truth_dict",
]
