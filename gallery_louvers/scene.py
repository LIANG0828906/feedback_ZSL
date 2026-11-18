"""JSON scene loading, business validation and immutable render views."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import ErrorCode, SceneValidationError, ValidationIssue
from .util import object_digest


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _vec3(value: Any) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != 3 or not all(_is_number(v) for v in value):
        return None
    return np.asarray(value, dtype=np.float64)


@dataclass(frozen=True)
class Leaf:
    layer: int
    leaf: int
    seed: int
    thickness: float
    brush_direction: np.ndarray
    vertices: np.ndarray
    triangles: np.ndarray
    uvs: np.ndarray
    albedo: np.ndarray
    roughness_along: float
    roughness_across: float
    specular: float
    z_center: float
    z_front: float
    z_back: float
    rect_x: tuple[float, float]
    rect_y: tuple[float, float]

    @property
    def key(self) -> tuple[int, int]:
        return (self.layer, self.leaf)

    @property
    def width(self) -> float:
        return self.rect_x[1] - self.rect_x[0]

    @property
    def height(self) -> float:
        return self.rect_y[1] - self.rect_y[0]


@dataclass(frozen=True)
class Layer:
    layer: int
    name: str
    z_center: float
    gap_to_outer: float
    leaf_ids: tuple[int, ...]


@dataclass(frozen=True)
class Receiver:
    id: str
    plane_axis: str
    plane_value: float
    normal: np.ndarray
    bounds: dict[str, tuple[float, float]]
    albedo: np.ndarray
    roughness: float


@dataclass(frozen=True)
class Light:
    id: str
    position: np.ndarray
    target: np.ndarray
    intensity: float
    color: np.ndarray
    inner_cone_deg: float
    outer_cone_deg: float
    falloff: float
    radius_m: float

    @property
    def axis(self) -> np.ndarray:
        vector = self.target - self.position
        return vector / np.linalg.norm(vector)

    @property
    def cos_inner(self) -> float:
        return math.cos(math.radians(self.inner_cone_deg))

    @property
    def cos_outer(self) -> float:
        return math.cos(math.radians(self.outer_cone_deg))


@dataclass(frozen=True)
class CameraSettings:
    center: np.ndarray
    radius: float
    elevation_deg: float
    angles_deg: tuple[float, ...]
    fov_deg: float
    near: float
    far: float
    width: int
    height: int


@dataclass(frozen=True)
class RenderSettings:
    tile_width: int
    tile_height: int
    pattern_resolution: tuple[int, int]
    microsurface_resolution: tuple[int, int]
    shadow_sample_density: int
    pixel_supersamples: int
    ambient_strength: float
    highlight_soft_knee: float
    tone_map_white: float
    thickness_bounds: tuple[float, float]
    softness_base: float
    softness_per_leaf_receiver: float
    softness_per_light_leaf: float
    interlayer_gap: float
    seed_domain: str

    def block_counts(self, width: int, height: int) -> tuple[int, int]:
        return (
            (width + self.tile_width - 1) // self.tile_width,
            (height + self.tile_height - 1) // self.tile_height,
        )

    def block_origins(self, angle_width: int, angle_height: int) -> list[tuple[int, int, int]]:
        blocks_x, blocks_y = self.block_counts(angle_width, angle_height)
        result: list[tuple[int, int, int]] = []
        index = 0
        for by in range(blocks_y):
            for bx in range(blocks_x):
                x0 = bx * self.tile_width
                y0 = by * self.tile_height
                result.append((index, x0, y0))
                index += 1
        return result


@dataclass
class Scene:
    raw: dict[str, Any]
    digest: str
    leaves: tuple[Leaf, ...]
    layers: tuple[Layer, ...]
    receivers: dict[str, Receiver]
    lights: tuple[Light, ...]
    camera: CameraSettings
    render: RenderSettings
    issues: tuple[ValidationIssue, ...] = ()

    @property
    def leaves_by_layer(self) -> dict[int, tuple[Leaf, ...]]:
        result: dict[int, tuple[Leaf, ...]] = {}
        for layer in self.layers:
            result[layer.layer] = tuple(
                leaf for leaf in self.leaves if leaf.layer == layer.layer
            )
        return result

    def leaf_lookup(self) -> dict[tuple[int, int], Leaf]:
        return {leaf.key: leaf for leaf in self.leaves}

    @property
    def angles(self) -> tuple[float, ...]:
        return self.camera.angles_deg

    def pattern_seed_manifest(self) -> list[dict[str, Any]]:
        return [
            {
                "layer": leaf.layer,
                "leaf": leaf.leaf,
                "seed": leaf.seed,
                "pattern_resolution": list(self.render.pattern_resolution),
                "microsurface_resolution": list(self.render.microsurface_resolution),
                "seed_domain": self.render.seed_domain,
            }
            for leaf in sorted(self.leaves, key=lambda item: item.key)
        ]


def _json_load(path: str | Path) -> Any:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def load_scene(path: str | Path) -> Scene:
    try:
        raw = _json_load(path)
    except (OSError, json.JSONDecodeError) as exc:
        issue = ValidationIssue(
            code=ErrorCode.SCENE_SCHEMA_INVALID,
            layer=None,
            leaf=None,
            path=str(path),
            message=f"cannot read JSON scene: {exc}",
        )
        raise SceneValidationError([issue]) from exc
    return validate_scene(raw)


def _parse_render_config(
    scene: dict[str, Any], issues: list[ValidationIssue]
) -> RenderSettings | None:
    render = scene.get("render")
    if not isinstance(render, dict):
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "render object is required",
                path="render",
            )
        )
        return None

    def positive_int(name: str, default: int, maximum: int) -> int:
        value = render.get(name, default)
        if not _is_int(value) or value <= 0 or value > maximum:
            issues.append(
                ValidationIssue(
                    ErrorCode.RENDER_CONFIG_INVALID,
                    f"render.{name} must be an integer in [1, {maximum}]",
                    path=f"render.{name}",
                    context={"value": value},
                )
            )
            return default
        return value

    tile_width = positive_int("tile_width", 64, 4096)
    tile_height = positive_int("tile_height", 48, 4096)
    density = render.get("shadow_sample_density", 5)
    if not _is_int(density) or density < 3 or density > 9 or density % 2 == 0:
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "shadow_sample_density must be an odd integer in [3, 9]",
                path="render.shadow_sample_density",
                context={"value": density},
            )
        )
        density = 5
    supersamples = render.get("pixel_supersamples", 1)
    if supersamples != 1:
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "pixel_supersamples currently supports the deterministic value 1",
                path="render.pixel_supersamples",
                context={"value": supersamples},
            )
        )
        supersamples = 1

    pattern_res = render.get("pattern_resolution", [96, 96])
    if (
        not isinstance(pattern_res, list)
        or len(pattern_res) != 2
        or not all(_is_int(v) and 16 <= v <= 512 for v in pattern_res)
    ):
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "pattern_resolution must be [height, width] integers in [16, 512]",
                path="render.pattern_resolution",
            )
        )
        pattern_res = [96, 96]
    micro_res = render.get("microsurface_resolution", [32, 12])
    if (
        not isinstance(micro_res, list)
        or len(micro_res) != 2
        or not all(_is_int(v) and 4 <= v <= 256 for v in micro_res)
    ):
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "microsurface_resolution must be [rows, columns] integers in [4, 256]",
                path="render.microsurface_resolution",
            )
        )
        micro_res = [32, 12]

    thickness_bounds = render.get("thickness_bounds_m", [0.008, 0.080])
    if (
        not isinstance(thickness_bounds, list)
        or len(thickness_bounds) != 2
        or not all(_is_number(v) for v in thickness_bounds)
        or not (0.0 < thickness_bounds[0] < thickness_bounds[1])
    ):
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "thickness_bounds_m must be a positive [min, max] pair",
                path="render.thickness_bounds_m",
            )
        )
        thickness_bounds = [0.008, 0.080]

    softness = render.get("softness", {})
    if not isinstance(softness, dict):
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "render.softness must be an object",
                path="render.softness",
            )
        )
        softness = {}
    soft_values: dict[str, float] = {}
    for key, default in (
        ("base_m", 0.0015),
        ("leaf_to_receiver_m_per_m", 0.014),
        ("light_to_leaf_m_per_m", 0.0042),
    ):
        value = softness.get(key, default)
        if not _is_number(value) or value < 0.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.RENDER_CONFIG_INVALID,
                    f"softness.{key} must be a finite non-negative number",
                    path=f"render.softness.{key}",
                )
            )
            value = default
        soft_values[key] = float(value)

    numeric_defaults = {
        "ambient_strength": 0.17,
        "highlight_soft_knee": 0.42,
        "tone_map_white": 1.85,
        "interlayer_gap_m": 0.16,
    }
    numeric: dict[str, float] = {}
    for key, default in numeric_defaults.items():
        value = render.get(key, default)
        if not _is_number(value) or value < 0.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.RENDER_CONFIG_INVALID,
                    f"render.{key} must be finite and non-negative",
                    path=f"render.{key}",
                )
            )
            value = default
        numeric[key] = float(value)

    seed_domain = render.get("seed_domain", "GLV-SEED-V1")
    if not isinstance(seed_domain, str) or not seed_domain:
        issues.append(
            ValidationIssue(
                ErrorCode.RENDER_CONFIG_INVALID,
                "render.seed_domain must be a non-empty string",
                path="render.seed_domain",
            )
        )
        seed_domain = "GLV-SEED-V1"

    return RenderSettings(
        tile_width=tile_width,
        tile_height=tile_height,
        pattern_resolution=(int(pattern_res[0]), int(pattern_res[1])),
        microsurface_resolution=(int(micro_res[0]), int(micro_res[1])),
        shadow_sample_density=density,
        pixel_supersamples=supersamples,
        ambient_strength=numeric["ambient_strength"],
        highlight_soft_knee=numeric["highlight_soft_knee"],
        tone_map_white=numeric["tone_map_white"],
        thickness_bounds=(float(thickness_bounds[0]), float(thickness_bounds[1])),
        softness_base=soft_values["base_m"],
        softness_per_leaf_receiver=soft_values["leaf_to_receiver_m_per_m"],
        softness_per_light_leaf=soft_values["light_to_leaf_m_per_m"],
        interlayer_gap=numeric["interlayer_gap_m"],
        seed_domain=seed_domain,
    )


def _parse_camera(
    scene: dict[str, Any], issues: list[ValidationIssue]
) -> CameraSettings | None:
    camera = scene.get("camera")
    if not isinstance(camera, dict):
        issues.append(
            ValidationIssue(ErrorCode.CAMERA_INVALID, "camera object is required", path="camera")
        )
        return None
    center = _vec3(camera.get("center"))
    if center is None:
        issues.append(
            ValidationIssue(
                ErrorCode.CAMERA_INVALID,
                "camera.center must be a finite three-vector",
                path="camera.center",
            )
        )
        center = np.zeros(3, dtype=np.float64)
    radius = camera.get("radius")
    elevation = camera.get("elevation_deg")
    fov = camera.get("fov_deg")
    near = camera.get("near")
    far = camera.get("far")
    width = camera.get("width")
    height = camera.get("height")
    angles = camera.get("angles_deg")
    numeric_checks = [
        ("radius", radius, lambda v: v > 0.0),
        ("elevation_deg", elevation, lambda v: -89.0 <= v <= 89.0),
        ("fov_deg", fov, lambda v: 1.0 <= v < 179.0),
        ("near", near, lambda v: v > 0.0),
        ("far", far, lambda v: v > 0.0),
    ]
    for name, value, predicate in numeric_checks:
        if not _is_number(value) or not predicate(value):
            issues.append(
                ValidationIssue(
                    ErrorCode.CAMERA_INVALID,
                    f"camera.{name} is outside its legal range",
                    path=f"camera.{name}",
                    context={"value": value},
                )
            )
    if _is_number(near) and _is_number(far) and far <= near:
        issues.append(
            ValidationIssue(
                ErrorCode.CAMERA_INVALID,
                "camera.far must be greater than camera.near",
                path="camera.far",
            )
        )
    for name, value, maximum in (("width", width, 4096), ("height", height, 4096)):
        if not _is_int(value) or value < 8 or value > maximum:
            issues.append(
                ValidationIssue(
                    ErrorCode.CAMERA_INVALID,
                    f"camera.{name} must be an integer in [8, {maximum}]",
                    path=f"camera.{name}",
                )
            )
    if not isinstance(angles, list) or not angles:
        issues.append(
            ValidationIssue(
                ErrorCode.CAMERA_INVALID,
                "camera.angles_deg must be a non-empty list",
                path="camera.angles_deg",
            )
        )
        angles = [0.0]
    elif not all(_is_number(value) for value in angles):
        issues.append(
            ValidationIssue(
                ErrorCode.CAMERA_INVALID,
                "camera.angles_deg must contain only finite numbers",
                path="camera.angles_deg",
            )
        )
        angles = [float(value) for value in angles if _is_number(value)] or [0.0]
    return CameraSettings(
        center=center,
        radius=float(radius) if _is_number(radius) and radius > 0 else 5.0,
        elevation_deg=float(elevation) if _is_number(elevation) else 8.0,
        angles_deg=tuple(float(value) for value in angles),
        fov_deg=float(fov) if _is_number(fov) else 38.0,
        near=float(near) if _is_number(near) else 0.05,
        far=float(far) if _is_number(far) else 30.0,
        width=int(width) if _is_int(width) and width >= 8 else 320,
        height=int(height) if _is_int(height) and height >= 8 else 240,
    )


def _parse_receivers(
    scene: dict[str, Any], issues: list[ValidationIssue]
) -> dict[str, Receiver]:
    raw = scene.get("receivers")
    if not isinstance(raw, dict):
        issues.append(
            ValidationIssue(
                ErrorCode.RECEIVER_INVALID,
                "receivers object with wall and floor is required",
                path="receivers",
            )
        )
        return {}
    result: dict[str, Receiver] = {}
    for receiver_id in ("wall", "floor"):
        item = raw.get(receiver_id)
        if not isinstance(item, dict):
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    f"receiver {receiver_id} is required",
                    path=f"receivers.{receiver_id}",
                )
            )
            continue
        axis = item.get("plane_axis")
        value = item.get("plane_value")
        normal = _vec3(item.get("normal"))
        bounds = item.get("bounds")
        albedo = _vec3(item.get("albedo"))
        roughness = item.get("roughness")
        valid = True
        if axis not in ("x", "y", "z"):
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "plane_axis must be x, y or z",
                    path=f"receivers.{receiver_id}.plane_axis",
                )
            )
            valid = False
        if not _is_number(value):
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "plane_value must be finite",
                    path=f"receivers.{receiver_id}.plane_value",
                )
            )
            valid = False
        if normal is None or np.linalg.norm(normal) < 1e-12:
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "normal must be a non-zero finite three-vector",
                    path=f"receivers.{receiver_id}.normal",
                )
            )
            valid = False
        if albedo is None or np.any(albedo < 0.0):
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "albedo must be a non-negative three-vector",
                    path=f"receivers.{receiver_id}.albedo",
                )
            )
            valid = False
        if not _is_number(roughness) or not 0.0 <= roughness <= 1.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "roughness must be in [0, 1]",
                    path=f"receivers.{receiver_id}.roughness",
                )
            )
            valid = False
        parsed_bounds: dict[str, tuple[float, float]] = {}
        if not isinstance(bounds, dict):
            issues.append(
                ValidationIssue(
                    ErrorCode.RECEIVER_INVALID,
                    "bounds must be an object keyed by the two in-plane axes",
                    path=f"receivers.{receiver_id}.bounds",
                )
            )
            valid = False
        else:
            expected_axes = [name for name in ("x", "y", "z") if name != axis]
            if sorted(bounds) != sorted(expected_axes):
                issues.append(
                    ValidationIssue(
                        ErrorCode.RECEIVER_INVALID,
                        f"bounds for receiver {receiver_id} must key {expected_axes}",
                        path=f"receivers.{receiver_id}.bounds",
                    )
                )
                valid = False
            for bound_axis in expected_axes:
                pair = bounds.get(bound_axis)
                if (
                    not isinstance(pair, list)
                    or len(pair) != 2
                    or not all(_is_number(v) for v in pair)
                    or pair[0] >= pair[1]
                ):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.RECEIVER_INVALID,
                            f"bounds.{bound_axis} must be a finite ascending pair",
                            path=f"receivers.{receiver_id}.bounds.{bound_axis}",
                        )
                    )
                    valid = False
                else:
                    parsed_bounds[bound_axis] = (float(pair[0]), float(pair[1]))
        if valid:
            result[receiver_id] = Receiver(
                id=receiver_id,
                plane_axis=str(axis),
                plane_value=float(value),
                normal=normal / np.linalg.norm(normal),
                bounds=parsed_bounds,
                albedo=albedo,
                roughness=float(roughness),
            )
    return result


def _parse_lights(
    scene: dict[str, Any], issues: list[ValidationIssue]
) -> tuple[Light, ...]:
    raw = scene.get("lights")
    if not isinstance(raw, list) or len(raw) != 3:
        issues.append(
            ValidationIssue(
                ErrorCode.LIGHT_INVALID,
                "exactly three track spotlights are required",
                path="lights",
                context={"count": len(raw) if isinstance(raw, list) else None},
            )
        )
        if not isinstance(raw, list):
            return ()
    result: list[Light] = []
    seen_ids: set[str] = set()
    for index, item in enumerate(raw):
        path = f"lights[{index}]"
        if not isinstance(item, dict):
            issues.append(ValidationIssue(ErrorCode.LIGHT_INVALID, "light must be an object", path=path))
            continue
        light_id = item.get("id")
        position = _vec3(item.get("position"))
        target = _vec3(item.get("target"))
        color = _vec3(item.get("color"))
        intensity = item.get("intensity")
        inner = item.get("inner_cone_deg")
        outer = item.get("outer_cone_deg")
        falloff = item.get("falloff")
        radius = item.get("radius_m")
        valid = True
        if not isinstance(light_id, str) or not light_id or light_id in seen_ids:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "light id must be a unique non-empty string",
                    path=f"{path}.id",
                )
            )
            valid = False
        else:
            seen_ids.add(light_id)
        if position is None:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "position must be a finite three-vector",
                    path=f"{path}.position",
                )
            )
            valid = False
        if target is None:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "target must be a finite three-vector",
                    path=f"{path}.target",
                )
            )
            valid = False
        if position is not None and target is not None and np.linalg.norm(target - position) < 1e-12:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "position and target must differ",
                    path=f"{path}.target",
                )
            )
            valid = False
        if color is None or np.any(color < 0.0):
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "color must be a non-negative three-vector",
                    path=f"{path}.color",
                )
            )
            valid = False
        if not _is_number(intensity) or intensity < 0.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "intensity must be finite and non-negative",
                    path=f"{path}.intensity",
                )
            )
            valid = False
        if not _is_number(inner) or not _is_number(outer) or not (0.0 < inner < outer < 90.0):
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "cone angles must satisfy 0 < inner < outer < 90",
                    path=f"{path}.inner_cone_deg",
                )
            )
            valid = False
        if not _is_number(falloff) or falloff < 0.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "falloff must be finite and non-negative",
                    path=f"{path}.falloff",
                )
            )
            valid = False
        if not _is_number(radius) or radius < 0.0:
            issues.append(
                ValidationIssue(
                    ErrorCode.LIGHT_INVALID,
                    "radius_m must be finite and non-negative",
                    path=f"{path}.radius_m",
                )
            )
            valid = False
        if valid:
            result.append(
                Light(
                    id=str(light_id),
                    position=position,
                    target=target,
                    intensity=float(intensity),
                    color=color,
                    inner_cone_deg=float(inner),
                    outer_cone_deg=float(outer),
                    falloff=float(falloff),
                    radius_m=float(radius),
                )
            )
    return tuple(result)


def validate_scene(raw: Any) -> Scene:
    issues: list[ValidationIssue] = []
    if not isinstance(raw, dict):
        raise SceneValidationError(
            [
                ValidationIssue(
                    ErrorCode.SCENE_SCHEMA_INVALID,
                    "scene root must be a JSON object",
                    path="$",
                )
            ]
        )
    schema_version = raw.get("schema_version")
    if schema_version != 1:
        issues.append(
            ValidationIssue(
                ErrorCode.SCENE_SCHEMA_INVALID,
                "schema_version must be 1",
                path="schema_version",
                context={"value": schema_version},
            )
        )
    if not isinstance(raw.get("scene_id"), str) or not raw.get("scene_id"):
        issues.append(
            ValidationIssue(
                ErrorCode.SCENE_SCHEMA_INVALID,
                "scene_id must be a non-empty string",
                path="scene_id",
            )
        )

    camera = _parse_camera(raw, issues)
    render = _parse_render_config(raw, issues)
    receivers = _parse_receivers(raw, issues)
    lights = _parse_lights(raw, issues)

    layers_raw = raw.get("layers")
    leaves_raw = raw.get("leaves")
    layer_records: dict[int, Layer] = {}
    if not isinstance(layers_raw, list) or not isinstance(leaves_raw, list):
        issues.append(
            ValidationIssue(
                ErrorCode.SCENE_SCHEMA_INVALID,
                "layers and leaves must be arrays",
                path="layers",
            )
        )
        layers_raw = []
        leaves_raw = []
    if len(layers_raw) != 4:
        issues.append(
            ValidationIssue(
                ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                "exactly four louver layers are required",
                path="layers",
                context={"count": len(layers_raw)},
            )
        )
    seen_layer_ids: set[int] = set()
    for index, item in enumerate(layers_raw):
        path = f"layers[{index}]"
        if not isinstance(item, dict):
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "layer record must be an object",
                    path=path,
                )
            )
            continue
        layer_id = item.get("layer")
        if not _is_int(layer_id):
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "layer index is missing or not an integer",
                    layer=None,
                    path=f"{path}.layer",
                )
            )
            continue
        if layer_id in seen_layer_ids:
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "duplicate layer index",
                    layer=layer_id,
                    path=f"{path}.layer",
                )
            )
            continue
        seen_layer_ids.add(layer_id)
        if layer_id < 0 or layer_id >= 4:
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "layer index must be in [0, 3]",
                    layer=layer_id,
                    path=f"{path}.layer",
                )
            )
            continue
        leaf_ids = item.get("leaf_ids")
        if (
            not isinstance(leaf_ids, list)
            or not leaf_ids
            or not all(_is_int(value) for value in leaf_ids)
            or len(set(leaf_ids)) != len(leaf_ids)
            or sorted(leaf_ids) != list(range(len(leaf_ids)))
        ):
            issues.append(
                ValidationIssue(
                    ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE,
                    "leaf_ids must be a non-empty contiguous unique integer list",
                    layer=layer_id,
                    path=f"{path}.leaf_ids",
                )
            )
            leaf_ids = []
        layer_records[layer_id] = Layer(
            layer=layer_id,
            name=str(item.get("name", f"layer-{layer_id}")),
            z_center=float(item["z_center"]) if _is_number(item.get("z_center")) else 0.0,
            gap_to_outer=float(item["gap_to_outer_m"])
            if _is_number(item.get("gap_to_outer_m"))
            else 0.16,
            leaf_ids=tuple(int(value) for value in leaf_ids),
        )
    for expected_layer in range(4):
        if expected_layer not in seen_layer_ids:
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "layer index is missing",
                    layer=expected_layer,
                    path="layers",
                )
            )

    leaves: list[Leaf] = []
    seen_leaf_keys: set[tuple[int, int]] = set()
    per_layer_ids: dict[int, set[int]] = {layer: set() for layer in seen_layer_ids}
    for index, item in enumerate(leaves_raw):
        path = f"leaves[{index}]"
        if not isinstance(item, dict):
            issues.append(
                ValidationIssue(
                    ErrorCode.SCENE_SCHEMA_INVALID,
                    "leaf record must be an object",
                    path=path,
                )
            )
            continue
        layer_id = item.get("layer")
        leaf_id = item.get("leaf")
        layer_value = layer_id if _is_int(layer_id) else None
        leaf_value = leaf_id if _is_int(leaf_id) else None
        if not _is_int(layer_id):
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "leaf record is missing integer layer index",
                    leaf=leaf_value,
                    path=f"{path}.layer",
                )
            )
            continue
        if not _is_int(leaf_id):
            issues.append(
                ValidationIssue(
                    ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE,
                    "leaf record is missing integer leaf index",
                    layer=layer_value,
                    path=f"{path}.leaf",
                )
            )
            continue
        key = (layer_id, leaf_id)
        if key in seen_leaf_keys:
            issues.append(
                ValidationIssue(
                    ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE,
                    "duplicate (layer, leaf) identity",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=path,
                )
            )
            continue
        seen_leaf_keys.add(key)
        per_layer_ids.setdefault(layer_id, set()).add(leaf_id)
        if layer_id not in seen_layer_ids:
            issues.append(
                ValidationIssue(
                    ErrorCode.LAYER_ID_MISSING_OR_DUPLICATE,
                    "leaf references an undeclared layer",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.layer",
                )
            )

        seed = item.get("seed")
        if not _is_int(seed):
            issues.append(
                ValidationIssue(
                    ErrorCode.SEED_MISSING_OR_INVALID,
                    "leaf seed must be an integer",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.seed",
                )
            )

        thickness = item.get("thickness")
        if not _is_number(thickness):
            issues.append(
                ValidationIssue(
                    ErrorCode.THICKNESS_OUT_OF_RANGE,
                    "leaf thickness must be finite",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.thickness",
                )
            )
        elif render is not None and not (
            render.thickness_bounds[0] <= thickness <= render.thickness_bounds[1]
        ):
            issues.append(
                ValidationIssue(
                    ErrorCode.THICKNESS_OUT_OF_RANGE,
                    "leaf thickness is outside render.thickness_bounds_m",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.thickness",
                    context={
                        "value": thickness,
                        "bounds": list(render.thickness_bounds),
                    },
                )
            )

        brush = _vec3(item.get("brush_direction"))
        if brush is None or np.linalg.norm(brush) <= 1e-9:
            issues.append(
                ValidationIssue(
                    ErrorCode.ZERO_BRUSH_DIRECTION,
                    "brush_direction must be a non-zero finite three-vector",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.brush_direction",
                )
            )
            brush = np.asarray([1.0, 0.0, 0.0])

        vertices_raw = item.get("vertices")
        triangles_raw = item.get("triangles")
        vertices: np.ndarray | None = None
        triangles: np.ndarray | None = None
        if not isinstance(vertices_raw, list) or len(vertices_raw) < 3:
            issues.append(
                ValidationIssue(
                    ErrorCode.SCENE_SCHEMA_INVALID,
                    "vertices must contain at least three points",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.vertices",
                )
            )
        else:
            parsed_vertices = [_vec3(vertex) for vertex in vertices_raw]
            if any(vertex is None for vertex in parsed_vertices):
                issues.append(
                    ValidationIssue(
                        ErrorCode.SCENE_SCHEMA_INVALID,
                        "every vertex must be a finite three-vector",
                        layer=layer_id,
                        leaf=leaf_id,
                        path=f"{path}.vertices",
                    )
                )
            else:
                vertices = np.asarray(parsed_vertices, dtype=np.float64)
        if not isinstance(triangles_raw, list) or not triangles_raw:
            issues.append(
                ValidationIssue(
                    ErrorCode.SCENE_SCHEMA_INVALID,
                    "triangles must be a non-empty array",
                    layer=layer_id,
                    leaf=leaf_id,
                    path=f"{path}.triangles",
                )
            )
        else:
            bad_index = False
            parsed_triangles: list[list[int]] = []
            for triangle_index, triangle in enumerate(triangles_raw):
                if (
                    not isinstance(triangle, list)
                    or len(triangle) != 3
                    or not all(_is_int(value) for value in triangle)
                ):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "triangle must contain three integer indices",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.triangles[{triangle_index}]",
                        )
                    )
                    bad_index = True
                    continue
                if vertices is not None and any(
                    value < 0 or value >= len(vertices) for value in triangle
                ):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.MESH_INDEX_OUT_OF_RANGE,
                            "triangle index is out of range",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.triangles[{triangle_index}]",
                            context={
                                "triangle": triangle,
                                "vertex_count": len(vertices),
                            },
                        )
                    )
                    bad_index = True
                parsed_triangles.append([int(value) for value in triangle])
            if not bad_index:
                triangles = np.asarray(parsed_triangles, dtype=np.int64)

        if vertices is not None and triangles is not None:
            a = vertices[triangles[:, 0]]
            b = vertices[triangles[:, 1]]
            c = vertices[triangles[:, 2]]
            areas = np.linalg.norm(np.cross(b - a, c - a), axis=1) * 0.5
            degenerate = np.flatnonzero(areas <= 1e-12)
            if degenerate.size:
                for triangle_index in degenerate[:16]:
                    issues.append(
                        ValidationIssue(
                            ErrorCode.DEGENERATE_TRIANGLE,
                            "triangle area is zero or below 1e-12",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.triangles[{int(triangle_index)}]",
                            context={"area": float(areas[triangle_index])},
                        )
                    )

        if vertices is not None:
            x_min, y_min, z_min = np.min(vertices, axis=0)
            x_max, y_max, z_max = np.max(vertices, axis=0)
            z_extent = float(z_max - z_min)
            if x_max <= x_min or y_max <= y_min or z_max <= z_min:
                issues.append(
                    ValidationIssue(
                        ErrorCode.GEOMETRY_BOUNDS_INVALID,
                        "leaf mesh must have positive extents on all three axes",
                        layer=layer_id,
                        leaf=leaf_id,
                        path=f"{path}.vertices",
                    )
                )
            elif _is_number(thickness) and abs(z_extent - float(thickness)) > 2e-6:
                issues.append(
                    ValidationIssue(
                        ErrorCode.GEOMETRY_BOUNDS_INVALID,
                        "declared thickness does not match mesh z extent",
                        layer=layer_id,
                        leaf=leaf_id,
                        path=f"{path}.thickness",
                        context={"declared": thickness, "mesh_z_extent": z_extent},
                    )
                )
            if (
                x_max > x_min
                and y_max > y_min
                and z_max > z_min
                and _is_number(thickness)
                and triangles is not None
                and seed is not None
            ):
                material = item.get("material")
                if not isinstance(material, dict):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "leaf material object is required",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.material",
                        )
                    )
                    material = {}
                albedo = _vec3(material.get("albedo", [0.6, 0.6, 0.6]))
                rough_along = material.get("roughness_along", 0.12)
                rough_across = material.get("roughness_across", 0.35)
                specular = material.get("specular", 0.56)
                if albedo is None or np.any(albedo < 0.0):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "material.albedo must be a non-negative three-vector",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.material.albedo",
                        )
                    )
                    albedo = np.asarray([0.6, 0.6, 0.6])
                if (
                    not _is_number(rough_along)
                    or not _is_number(rough_across)
                    or not (0.0 < rough_along <= rough_across <= 1.0)
                ):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "roughness must satisfy 0 < along <= across <= 1",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.material.roughness_along",
                        )
                    )
                    rough_along, rough_across = 0.12, 0.35
                if not _is_number(specular) or specular < 0.0:
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "material.specular must be finite and non-negative",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.material.specular",
                        )
                    )
                    specular = 0.56
                uvs_raw = item.get("uvs", [[0.0, 0.0]] * len(vertices))
                if (
                    not isinstance(uvs_raw, list)
                    or len(uvs_raw) != len(vertices)
                    or any(
                        not isinstance(uv, list)
                        or len(uv) != 2
                        or not all(_is_number(value) for value in uv)
                        for uv in uvs_raw
                    )
                ):
                    issues.append(
                        ValidationIssue(
                            ErrorCode.SCENE_SCHEMA_INVALID,
                            "uvs must contain one finite [u, v] pair per vertex",
                            layer=layer_id,
                            leaf=leaf_id,
                            path=f"{path}.uvs",
                        )
                    )
                    uvs = np.asarray([[0.0, 0.0]] * len(vertices), dtype=np.float64)
                else:
                    uvs = np.asarray(uvs_raw, dtype=np.float64)
                leaves.append(
                    Leaf(
                        layer=layer_id,
                        leaf=leaf_id,
                        seed=int(seed),
                        thickness=float(thickness),
                        brush_direction=brush / np.linalg.norm(brush),
                        vertices=vertices,
                        triangles=triangles,
                        uvs=uvs,
                        albedo=albedo,
                        roughness_along=float(rough_along),
                        roughness_across=float(rough_across),
                        specular=float(specular),
                        z_center=float((z_min + z_max) * 0.5),
                        z_front=float(z_max),
                        z_back=float(z_min),
                        rect_x=(float(x_min), float(x_max)),
                        rect_y=(float(y_min), float(y_max)),
                    )
                )

    for layer_id, layer in layer_records.items():
        expected = set(layer.leaf_ids) if layer.leaf_ids else set()
        actual = per_layer_ids.get(layer_id, set())
        if expected and actual != expected:
            missing = sorted(expected - actual)
            extra = sorted(actual - expected)
            issues.append(
                ValidationIssue(
                    ErrorCode.LEAF_ID_MISSING_OR_DUPLICATE,
                    "declared leaf_ids do not match leaf records",
                    layer=layer_id,
                    path=f"layers[{layer_id}].leaf_ids",
                    context={"missing": missing, "undeclared": extra},
                )
            )

    if issues:
        raise SceneValidationError(issues)
    assert camera is not None and render is not None
    sorted_leaves = tuple(sorted(leaves, key=lambda item: item.key))
    sorted_layers = tuple(layer_records[index] for index in sorted(layer_records))
    return Scene(
        raw=raw,
        digest=object_digest(raw),
        leaves=sorted_leaves,
        layers=sorted_layers,
        receivers=receivers,
        lights=tuple(sorted(lights, key=lambda item: item.id.encode("utf-8"))),
        camera=camera,
        render=render,
    )
