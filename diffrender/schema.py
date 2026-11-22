"""Strict loading and validation for deterministic inverse-rendering datasets."""

from __future__ import annotations

import json
import math
import os
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from typing import Any

from .errors import AppError, fail


FORMAT_VERSION = 1


@dataclass(frozen=True)
class SphereSpec:
    id: str
    center: tuple[float, float, float]
    radius: float
    albedo: tuple[float, float, float]


@dataclass(frozen=True)
class PlaneSpec:
    id: str
    point: tuple[float, float, float]
    normal: tuple[float, float, float]
    albedo: tuple[float, float, float]


@dataclass(frozen=True)
class RectLightSpec:
    id: str
    center: tuple[float, float, float]
    normal: tuple[float, float, float]
    half_size: tuple[float, float]
    intensity: float


@dataclass(frozen=True)
class CameraSpec:
    id: str
    intrinsics: tuple[tuple[float, ...], ...]
    world_to_camera: tuple[tuple[float, ...], ...]


@dataclass(frozen=True)
class TargetImageSpec:
    id: str
    camera_id: str
    path: str
    width: int
    height: int


@dataclass(frozen=True)
class SceneSpec:
    scene_id: str
    background: tuple[float, float, float]
    ambient: float
    soft_shadow_samples: int
    ray_epsilon: float
    target_seed: int
    geometries: tuple[SphereSpec | PlaneSpec, ...]
    lights: tuple[RectLightSpec, ...]
    cameras: tuple[CameraSpec, ...]

    @property
    def spheres(self) -> tuple[SphereSpec, ...]:
        return tuple(g for g in self.geometries if isinstance(g, SphereSpec))

    @property
    def planes(self) -> tuple[PlaneSpec, ...]:
        return tuple(g for g in self.geometries if isinstance(g, PlaneSpec))


@dataclass(frozen=True)
class TruthSpec:
    """Physical analytic truth for optimized parameters."""

    values: dict[str, float]


@dataclass(frozen=True)
class Task:
    dataset_root: Path
    group_id: str
    scene: SceneSpec
    targets: tuple[TargetImageSpec, ...]
    truth: TruthSpec
    source_fingerprint: str


@dataclass(frozen=True)
class Dataset:
    root: Path
    manifest: dict[str, Any]
    groups: dict[str, dict[str, Any]]

    def task(self, group_id: str) -> Task:
        if group_id not in self.groups:
            fail(
                "E_SCHEMA",
                f"group not found in manifest: {group_id}",
                "groups",
                available=sorted(self.groups),
            )
        return load_task(self.root, self.groups[group_id])


def _require_mapping(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail("E_SCHEMA", "expected a JSON object", path, actual_type=type(value).__name__)
    return value


def _require_key(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        fail("E_SCHEMA", f"missing required field: {key}", f"{path}.{key}")
    return mapping[key]


def _require_string(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value:
        fail("E_SCHEMA", "expected a non-empty string", path)
    return value


def _finite_float(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        fail("E_SCHEMA", "expected a finite number", path)
    result = float(value)
    if not math.isfinite(result):
        fail("E_SCHEMA", "expected a finite number", path, value=repr(value))
    return result


def _positive_int(value: Any, path: str, *, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        fail("E_SCHEMA", "expected an integer", path)
    if value <= 0:
        fail("E_SCHEMA", "expected a positive integer", path, value=value)
    if maximum is not None and value > maximum:
        fail("E_SCHEMA", f"value exceeds maximum {maximum}", path, value=value)
    return value


def _vec3(value: Any, path: str, *, albedo: bool = False) -> tuple[float, float, float]:
    if not isinstance(value, list) or len(value) != 3:
        fail("E_SCHEMA", "expected an array of exactly 3 numbers", path)
    out = tuple(_finite_float(v, f"{path}[{i}]") for i, v in enumerate(value))
    if albedo:
        for i, v in enumerate(out):
            if v < 0.0 or v > 1.0:
                fail("E_SCHEMA", "albedo must be in [0, 1]", f"{path}[{i}]", value=v)
    return out  # type: ignore[return-value]


def _vec2(value: Any, path: str) -> tuple[float, float]:
    if not isinstance(value, list) or len(value) != 2:
        fail("E_SCHEMA", "expected an array of exactly 2 numbers", path)
    return (
        _finite_float(value[0], f"{path}[0]"),
        _finite_float(value[1], f"{path}[1]"),
    )


def _matrix(
    value: Any,
    path: str,
    rows: int,
    cols: int,
) -> tuple[tuple[float, ...], ...]:
    if not isinstance(value, list) or len(value) != rows:
        fail("E_SCHEMA", f"expected a {rows}x{cols} matrix", path)
    result: list[tuple[float, ...]] = []
    for r, row in enumerate(value):
        if not isinstance(row, list) or len(row) != cols:
            fail("E_SCHEMA", f"expected a {rows}x{cols} matrix", f"{path}[{r}]")
        result.append(
            tuple(
                _finite_float(cell, f"{path}[{r}][{c}]")
                for c, cell in enumerate(row)
            )
        )
    return tuple(result)


def _det3(matrix: tuple[tuple[float, ...], ...]) -> float:
    a, b, c = matrix[0][:3]
    d, e, f = matrix[1][:3]
    g, h, i = matrix[2][:3]
    return a * (e * i - f * h) - b * (d * i - f * g) + c * (d * h - e * g)


def _safe_relative_path(value: Any, path: str) -> str:
    text = _require_string(value, path)
    pure = PurePosixPath(text.replace("\\", "/"))
    if pure.is_absolute() or ".." in pure.parts:
        fail("E_SCHEMA", "path must be relative and must not traverse parents", path, value=text)
    return pure.as_posix()


def _normal_norm(vector: tuple[float, ...]) -> float:
    return math.sqrt(sum(v * v for v in vector))


def _parse_geometries(raw: Any, path: str) -> tuple[SphereSpec | PlaneSpec, ...]:
    if not isinstance(raw, list) or not raw:
        fail("E_SCHEMA", "geometries must be a non-empty array", path)
    geometries: list[SphereSpec | PlaneSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw):
        item_path = f"{path}[{index}]"
        node = _require_mapping(item, item_path)
        geom_id = _require_string(_require_key(node, "id", item_path), f"{item_path}.id")
        if geom_id in seen:
            fail("E_SCHEMA", "duplicate geometry id", f"{item_path}.id", id=geom_id)
        seen.add(geom_id)
        geom_type = _require_string(
            _require_key(node, "type", item_path), f"{item_path}.type"
        )
        albedo = _vec3(
            _require_key(node, "albedo", item_path), f"{item_path}.albedo", albedo=True
        )
        if geom_type == "sphere":
            center = _vec3(
                _require_key(node, "center", item_path), f"{item_path}.center"
            )
            radius = _finite_float(
                _require_key(node, "radius", item_path), f"{item_path}.radius"
            )
            if radius <= 0.0 or radius > 1.0e4:
                fail(
                    "E_SCHEMA",
                    "sphere radius must be in (0, 10000]",
                    f"{item_path}.radius",
                    value=radius,
                )
            if any(abs(v) > 1.0e4 for v in center):
                fail(
                    "E_SCHEMA",
                    "sphere center component exceeds magnitude 10000",
                    f"{item_path}.center",
                )
            geometries.append(SphereSpec(geom_id, center, radius, albedo))
        elif geom_type == "plane":
            point = _vec3(
                _require_key(node, "point", item_path), f"{item_path}.point"
            )
            normal = _vec3(
                _require_key(node, "normal", item_path), f"{item_path}.normal"
            )
            norm = _normal_norm(normal)
            if norm < 1.0e-8:
                fail(
                    "E_SCHEMA",
                    "plane normal must be non-zero",
                    f"{item_path}.normal",
                    norm=norm,
                )
            if not (0.99 <= norm <= 1.01):
                fail(
                    "E_SCHEMA",
                    "plane normal must be approximately unit length",
                    f"{item_path}.normal",
                    norm=norm,
                )
            geometries.append(
                PlaneSpec(geom_id, point, tuple(v / norm for v in normal), albedo)
            )
        else:
            fail(
                "E_SCHEMA",
                "unsupported geometry type; expected sphere or plane",
                f"{item_path}.type",
                value=geom_type,
            )
    return tuple(geometries)


def _parse_lights(raw: Any, path: str) -> tuple[RectLightSpec, ...]:
    if not isinstance(raw, list) or not raw:
        fail("E_SCHEMA", "lights must be a non-empty array", path)
    lights: list[RectLightSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw):
        item_path = f"{path}[{index}]"
        node = _require_mapping(item, item_path)
        light_id = _require_string(_require_key(node, "id", item_path), f"{item_path}.id")
        if light_id in seen:
            fail("E_SCHEMA", "duplicate light id", f"{item_path}.id", id=light_id)
        seen.add(light_id)
        light_type = _require_string(
            _require_key(node, "type", item_path), f"{item_path}.type"
        )
        if light_type != "rect":
            fail(
                "E_SCHEMA",
                "unsupported light type; expected rect",
                f"{item_path}.type",
                value=light_type,
            )
        center = _vec3(_require_key(node, "center", item_path), f"{item_path}.center")
        normal = _vec3(_require_key(node, "normal", item_path), f"{item_path}.normal")
        norm = _normal_norm(normal)
        if norm < 1.0e-8:
            fail(
                "E_SCHEMA",
                "light normal must be non-zero",
                f"{item_path}.normal",
                norm=norm,
            )
        half_size = _vec2(
            _require_key(node, "half_size", item_path), f"{item_path}.half_size"
        )
        for i, value in enumerate(half_size):
            if value <= 0.0 or value > 1.0e4:
                fail(
                    "E_SCHEMA",
                    "light half_size must be in (0, 10000]",
                    f"{item_path}.half_size[{i}]",
                    value=value,
                )
        intensity = _finite_float(
            _require_key(node, "intensity", item_path), f"{item_path}.intensity"
        )
        if intensity < 0.0 or intensity > 1.0e6:
            fail(
                "E_SCHEMA",
                "light intensity must be in [0, 1e6]",
                f"{item_path}.intensity",
                value=intensity,
            )
        lights.append(
            RectLightSpec(
                light_id,
                center,
                tuple(v / norm for v in normal),
                half_size,
                intensity,
            )
        )
    return tuple(lights)


def _parse_cameras(raw: Any, path: str) -> tuple[CameraSpec, ...]:
    if not isinstance(raw, list) or not raw:
        fail("E_SCHEMA", "cameras must be a non-empty array", path)
    cameras: list[CameraSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw):
        item_path = f"{path}[{index}]"
        node = _require_mapping(item, item_path)
        camera_id = _require_string(
            _require_key(node, "id", item_path), f"{item_path}.id"
        )
        if camera_id in seen:
            fail("E_SCHEMA", "duplicate camera id", f"{item_path}.id", id=camera_id)
        seen.add(camera_id)
        intrinsics = _matrix(
            _require_key(node, "intrinsics", item_path), f"{item_path}.intrinsics", 3, 3
        )
        det = _det3(intrinsics)
        if abs(det) <= 1.0e-8:
            fail(
                "E_SCHEMA",
                "camera intrinsics must be invertible",
                f"{item_path}.intrinsics",
                determinant=det,
            )
        extrinsics = _matrix(
            _require_key(node, "world_to_camera", item_path),
            f"{item_path}.world_to_camera",
            4,
            4,
        )
        bottom = extrinsics[3]
        if any(abs(bottom[i] - expected) > 1.0e-8 for i, expected in enumerate((0, 0, 0, 1))):
            fail(
                "E_SCHEMA",
                "world_to_camera bottom row must be [0, 0, 0, 1]",
                f"{item_path}.world_to_camera[3]",
            )
        det_ext = _det3(extrinsics)
        if abs(det_ext) <= 1.0e-8:
            fail(
                "E_SCHEMA",
                "camera world_to_camera rotation must be invertible",
                f"{item_path}.world_to_camera",
                determinant=det_ext,
            )
        cameras.append(CameraSpec(camera_id, intrinsics, extrinsics))
    return tuple(cameras)


def _parse_targets(
    raw: Any,
    path: str,
    cameras: tuple[CameraSpec, ...],
) -> tuple[TargetImageSpec, ...]:
    if not isinstance(raw, list) or not raw:
        fail("E_SCHEMA", "targets must be a non-empty array", path)
    camera_ids = {camera.id for camera in cameras}
    targets: list[TargetImageSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw):
        item_path = f"{path}[{index}]"
        node = _require_mapping(item, item_path)
        target_id = _require_string(
            _require_key(node, "id", item_path), f"{item_path}.id"
        )
        if target_id in seen:
            fail("E_SCHEMA", "duplicate target id", f"{item_path}.id", id=target_id)
        seen.add(target_id)
        camera_id = _require_string(
            _require_key(node, "camera_id", item_path), f"{item_path}.camera_id"
        )
        if camera_id not in camera_ids:
            fail(
                "E_SCHEMA",
                "target references an unknown camera",
                f"{item_path}.camera_id",
                value=camera_id,
            )
        width = _positive_int(
            _require_key(node, "width", item_path), f"{item_path}.width", maximum=8192
        )
        height = _positive_int(
            _require_key(node, "height", item_path), f"{item_path}.height", maximum=8192
        )
        target_path = _safe_relative_path(
            _require_key(node, "path", item_path), f"{item_path}.path"
        )
        targets.append(
            TargetImageSpec(target_id, camera_id, target_path, width, height)
        )
    return tuple(targets)


def _parse_truth(raw: Any, path: str) -> TruthSpec:
    node = _require_mapping(raw, path)
    geometries = _require_mapping(_require_key(node, "geometries", path), f"{path}.geometries")
    lights = _require_mapping(_require_key(node, "lights", path), f"{path}.lights")
    flat: dict[str, float] = {}
    for geom_id, values in geometries.items():
        geom_path = f"{path}.geometries.{geom_id}"
        values_map = _require_mapping(values, geom_path)
        center = _vec3(_require_key(values_map, "center", geom_path), f"{geom_path}.center")
        for axis, value in zip("xyz", center):
            flat[f"{geom_id}.center.{axis}"] = value
        if "radius" in values_map:
            radius = _finite_float(values_map["radius"], f"{geom_path}.radius")
            if radius <= 0:
                fail("E_SCHEMA", "truth radius must be positive", f"{geom_path}.radius")
            flat[f"{geom_id}.radius"] = radius
        albedo = _vec3(
            _require_key(values_map, "albedo", geom_path),
            f"{geom_path}.albedo",
            albedo=True,
        )
        for channel, value in zip(("r", "g", "b"), albedo):
            flat[f"{geom_id}.albedo.{channel}"] = value
    for light_id, values in lights.items():
        light_path = f"{path}.lights.{light_id}"
        values_map = _require_mapping(values, light_path)
        intensity = _finite_float(
            _require_key(values_map, "intensity", light_path),
            f"{light_path}.intensity",
        )
        if intensity < 0:
            fail(
                "E_SCHEMA",
                "truth light intensity must be non-negative",
                f"{light_path}.intensity",
            )
        flat[f"{light_id}.intensity"] = intensity
    return TruthSpec(flat)


def load_dataset(root: str | os.PathLike[str]) -> Dataset:
    dataset_root = Path(root).resolve()
    manifest_path = dataset_root / "manifest.json"
    if not manifest_path.is_file():
        fail(
            "E_IO",
            "dataset manifest not found; run the deterministic generator first",
            "manifest.json",
            path=str(manifest_path),
        )
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail("E_SCHEMA", f"cannot parse dataset manifest: {exc}", "manifest.json")
    manifest = _require_mapping(manifest, "$")
    format_version = _require_key(manifest, "format_version", "$")
    if format_version != FORMAT_VERSION:
        fail(
            "E_SCHEMA",
            f"unsupported manifest format_version; expected {FORMAT_VERSION}",
            "format_version",
            value=format_version,
        )
    groups_raw = _require_key(manifest, "groups", "$")
    if not isinstance(groups_raw, list) or not groups_raw:
        fail("E_SCHEMA", "groups must be a non-empty array", "groups")
    groups: dict[str, dict[str, Any]] = {}
    for index, item in enumerate(groups_raw):
        path = f"groups[{index}]"
        node = _require_mapping(item, path)
        group_id = _require_string(_require_key(node, "group_id", path), f"{path}.group_id")
        if group_id in groups:
            fail("E_SCHEMA", "duplicate group id", f"{path}.group_id", id=group_id)
        scene_path = _safe_relative_path(
            _require_key(node, "scene_path", path), f"{path}.scene_path"
        )
        target_manifest_path = _safe_relative_path(
            _require_key(node, "target_manifest_path", path),
            f"{path}.target_manifest_path",
        )
        if not (dataset_root / scene_path).is_file():
            fail(
                "E_SCHEMA",
                "scene file referenced by manifest is missing",
                f"{path}.scene_path",
                value=scene_path,
            )
        if not (dataset_root / target_manifest_path).is_file():
            fail(
                "E_SCHEMA",
                "target manifest referenced by manifest is missing",
                f"{path}.target_manifest_path",
                value=target_manifest_path,
            )
        groups[group_id] = {
            **node,
            "scene_path": scene_path,
            "target_manifest_path": target_manifest_path,
        }
    return Dataset(root=dataset_root, manifest=manifest, groups=groups)


def load_task(root: Path, entry: dict[str, Any]) -> Task:
    group_id = str(entry["group_id"])
    scene_path = root / str(entry["scene_path"])
    target_path = root / str(entry["target_manifest_path"])
    try:
        scene_raw = json.loads(scene_path.read_text(encoding="utf-8"))
        targets_raw = json.loads(target_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail("E_SCHEMA", f"cannot parse task JSON: {exc}", "task")
    scene_node = _require_mapping(scene_raw, "$")
    targets_node = _require_mapping(targets_raw, "$")
    scene_id = _require_string(
        _require_key(scene_node, "scene_id", "$"), "scene_id"
    )
    background = _vec3(
        _require_key(scene_node, "background", "$"), "background"
    )
    ambient = _finite_float(_require_key(scene_node, "ambient", "$"), "ambient")
    if ambient < 0.0 or ambient > 1.0:
        fail("E_SCHEMA", "ambient must be in [0, 1]", "ambient", value=ambient)
    render_node = _require_mapping(_require_key(scene_node, "render", "$"), "render")
    soft_shadow_samples = _positive_int(
        _require_key(render_node, "soft_shadow_samples", "render"),
        "render.soft_shadow_samples",
        maximum=64,
    )
    ray_epsilon = _finite_float(
        _require_key(render_node, "ray_epsilon", "render"), "render.ray_epsilon"
    )
    if not (0.0 < ray_epsilon <= 0.1):
        fail(
            "E_SCHEMA",
            "ray_epsilon must be in (0, 0.1]",
            "render.ray_epsilon",
            value=ray_epsilon,
        )
    target_seed = _require_key(render_node, "target_seed", "render")
    if isinstance(target_seed, bool) or not isinstance(target_seed, int):
        fail(
            "E_SCHEMA",
            "target_seed must be an integer",
            "render.target_seed",
        )
    geometries = _parse_geometries(
        _require_key(scene_node, "geometries", "$"), "geometries"
    )
    lights = _parse_lights(_require_key(scene_node, "lights", "$"), "lights")
    cameras = _parse_cameras(_require_key(scene_node, "cameras", "$"), "cameras")
    if not any(isinstance(g, SphereSpec) for g in geometries):
        fail("E_SCHEMA", "at least one sphere geometry is required", "geometries")
    if not any(isinstance(g, PlaneSpec) for g in geometries):
        fail("E_SCHEMA", "at least one plane geometry is required", "geometries")
    link_format = _require_key(targets_node, "format_version", "$")
    if link_format != FORMAT_VERSION:
        fail(
            "E_SCHEMA",
            f"unsupported target format_version; expected {FORMAT_VERSION}",
            "format_version",
            value=link_format,
        )
    targets = _parse_targets(
        _require_key(targets_node, "targets", "$"), "targets", cameras
    )
    truth = _parse_truth(_require_key(targets_node, "truth", "$"), "truth")
    expected_labels = {
        label
        for geom in geometries
        for label in (
            [f"{geom.id}.center.{axis}" for axis in "xyz"]
            + ([f"{geom.id}.radius"] if isinstance(geom, SphereSpec) else [])
            + [f"{geom.id}.albedo.{channel}" for channel in "rgb"]
        )
    } | {f"{light.id}.intensity" for light in lights}
    missing = sorted(expected_labels - set(truth.values))
    extra = sorted(set(truth.values) - expected_labels)
    if missing or extra:
        fail(
            "E_SCHEMA",
            "analytic truth labels do not exactly match scene parameters",
            "truth",
            missing=missing,
            extra=extra,
        )
    source_fingerprint = _task_fingerprint(
        root, scene_path, target_path, targets
    )
    return Task(
        dataset_root=root,
        group_id=group_id,
        scene=SceneSpec(
            scene_id=scene_id,
            background=background,
            ambient=ambient,
            soft_shadow_samples=soft_shadow_samples,
            ray_epsilon=ray_epsilon,
            target_seed=target_seed,
            geometries=geometries,
            lights=lights,
            cameras=cameras,
        ),
        targets=targets,
        truth=truth,
        source_fingerprint=source_fingerprint,
    )


def _task_fingerprint(
    root: Path,
    scene_path: Path,
    target_path: Path,
    targets: tuple[TargetImageSpec, ...],
) -> str:
    import hashlib

    digest = hashlib.sha256()
    for path in (scene_path, target_path):
        digest.update(path.relative_to(root).as_posix().encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    for target in targets:
        path = root / target.path
        digest.update(target.path.encode("utf-8"))
        digest.update(b"\0")
        if path.is_file():
            digest.update(path.read_bytes())
        else:
            digest.update(b"<missing>")
        digest.update(b"\0")
    return digest.hexdigest()


def decode_target(path: Path, target: TargetImageSpec) -> "Any":
    """Decode and validate a local float target image."""

    import numpy as np

    if not path.is_file():
        fail(
            "E_DECODE",
            "target image file is missing",
            f"targets.{target.id}.path",
            path=str(path),
        )
    try:
        image = np.load(path, allow_pickle=False)
    except (OSError, ValueError, EOFError) as exc:
        fail(
            "E_DECODE",
            f"target image cannot be decoded: {exc}",
            f"targets.{target.id}.path",
            path=str(path),
        )
    if image.ndim != 3 or image.shape[2] != 3:
        fail(
            "E_DECODE",
            "target image must have shape [height, width, 3]",
            f"targets.{target.id}.path",
            shape=list(image.shape),
        )
    if image.shape[0] != target.height or image.shape[1] != target.width:
        fail(
            "E_DECODE",
            "target image dimensions do not match the manifest",
            f"targets.{target.id}.path",
            expected=[target.height, target.width],
            actual=list(image.shape[:2]),
        )
    if not np.issubdtype(image.dtype, np.floating):
        fail(
            "E_DECODE",
            "target image dtype must be floating point",
            f"targets.{target.id}.path",
            dtype=str(image.dtype),
        )
    if not np.isfinite(image).all():
        fail(
            "E_DECODE",
            "target image contains non-finite values",
            f"targets.{target.id}.path",
        )
    return image.astype(np.float64, copy=False)

