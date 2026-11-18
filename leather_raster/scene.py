from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SceneValidationError, ValidationIssue
from .mesh import (
    PlaneFrame,
    face_normals,
    fit_plane_frame,
    point_inside_triangular_mesh,
    triangle_areas,
)
from .util import sha256_json


class _Issues:
    def __init__(self) -> None:
        self.items: list[ValidationIssue] = []

    def add(
        self,
        code: str,
        path: str,
        message: str,
        *,
        layer_index: int | None = None,
        cutout_id: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        self.items.append(
            ValidationIssue(
                code=code,
                path=path,
                message=message,
                layer_index=layer_index,
                cutout_id=cutout_id,
                details=details or {},
            )
        )

    def raise_if_any(self) -> None:
        if self.items:
            raise SceneValidationError(self.items)


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value))


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _number(
    mapping: dict[str, Any],
    key: str,
    path: str,
    issues: _Issues,
    *,
    layer_index: int | None = None,
    minimum: float | None = None,
    maximum: float | None = None,
    default: float | None = None,
    required: bool = True,
) -> float:
    field_path = f"{path}.{key}"
    if key not in mapping:
        if required:
            issues.add("MISSING_FIELD", field_path, f"missing required field '{key}'", layer_index=layer_index)
        return 0.0 if default is None else default
    value = mapping[key]
    if not _is_number(value):
        issues.add("INVALID_NUMBER", field_path, "value must be a finite number", layer_index=layer_index)
        return 0.0 if default is None else default
    number = float(value)
    if minimum is not None and number < minimum:
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"value must be >= {minimum}",
            layer_index=layer_index,
            details={"value": number, "minimum": minimum},
        )
    if maximum is not None and number > maximum:
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"value must be <= {maximum}",
            layer_index=layer_index,
            details={"value": number, "maximum": maximum},
        )
    return number


def _integer(
    mapping: dict[str, Any],
    key: str,
    path: str,
    issues: _Issues,
    *,
    layer_index: int | None = None,
    minimum: int | None = None,
    maximum: int | None = None,
    default: int | None = None,
    required: bool = True,
) -> int:
    field_path = f"{path}.{key}"
    if key not in mapping:
        if required:
            issues.add("MISSING_FIELD", field_path, f"missing required field '{key}'", layer_index=layer_index)
        return 0 if default is None else default
    value = mapping[key]
    if not _is_int(value):
        issues.add("INVALID_INTEGER", field_path, "value must be an integer", layer_index=layer_index)
        return 0 if default is None else default
    number = int(value)
    if minimum is not None and number < minimum:
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"value must be >= {minimum}",
            layer_index=layer_index,
            details={"value": number, "minimum": minimum},
        )
    if maximum is not None and number > maximum:
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"value must be <= {maximum}",
            layer_index=layer_index,
            details={"value": number, "maximum": maximum},
        )
    return number


def _vector(
    mapping: dict[str, Any],
    key: str,
    path: str,
    size: int,
    issues: _Issues,
    *,
    layer_index: int | None = None,
    minimum: float | None = None,
    maximum: float | None = None,
    default: list[float] | None = None,
    required: bool = True,
) -> np.ndarray:
    field_path = f"{path}.{key}"
    if key not in mapping:
        if required:
            issues.add("MISSING_FIELD", field_path, f"missing required field '{key}'", layer_index=layer_index)
        return np.asarray(default if default is not None else [0.0] * size, dtype=np.float64)
    value = mapping[key]
    if not isinstance(value, list) or len(value) != size:
        issues.add(
            "INVALID_VECTOR",
            field_path,
            f"value must be an array of {size} finite numbers",
            layer_index=layer_index,
        )
        return np.asarray(default if default is not None else [0.0] * size, dtype=np.float64)
    if not all(_is_number(component) for component in value):
        issues.add(
            "INVALID_VECTOR",
            field_path,
            f"value must be an array of {size} finite numbers",
            layer_index=layer_index,
        )
        return np.asarray(default if default is not None else [0.0] * size, dtype=np.float64)
    result = np.asarray(value, dtype=np.float64)
    if minimum is not None and bool(np.any(result < minimum)):
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"all components must be >= {minimum}",
            layer_index=layer_index,
            details={"value": result.tolist(), "minimum": minimum},
        )
    if maximum is not None and bool(np.any(result > maximum)):
        issues.add(
            "VALUE_OUT_OF_RANGE",
            field_path,
            f"all components must be <= {maximum}",
            layer_index=layer_index,
            details={"value": result.tolist(), "maximum": maximum},
        )
    return result


@dataclass(frozen=True)
class Layer:
    layer_index: int
    vertices: np.ndarray
    triangles: np.ndarray
    triangle_cutout_ids: np.ndarray
    cutouts: dict[int, tuple[int, ...]]
    transmittance: float
    tint: np.ndarray
    fiber_seed: int
    thickness: float
    face_normals: np.ndarray
    plane: PlaneFrame
    uv_axis_a: int
    uv_axis_b: int
    uv_bounds_a: tuple[float, float]
    uv_bounds_b: tuple[float, float]

    def solid_triangles(self) -> np.ndarray:
        return self.triangles[self.triangle_cutout_ids == 0]

    def surface_uv(self, points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        pts = np.asarray(points, dtype=np.float64).reshape(-1, 3)
        a = pts[:, self.uv_axis_a]
        b = pts[:, self.uv_axis_b]
        span_a = max(self.uv_bounds_a[1] - self.uv_bounds_a[0], 1e-12)
        span_b = max(self.uv_bounds_b[1] - self.uv_bounds_b[0], 1e-12)
        return (
            (a - self.uv_bounds_a[0]) / span_a,
            (b - self.uv_bounds_b[0]) / span_b,
        )


@dataclass(frozen=True)
class Light:
    light_id: str
    position: np.ndarray
    intensity: float
    color: np.ndarray
    radius: float
    softness_gain: float


@dataclass(frozen=True)
class Screen:
    origin: np.ndarray
    axis_u: np.ndarray
    axis_v: np.ndarray
    width: float
    height: float
    normal: np.ndarray
    tint: np.ndarray
    fiber_seed: int


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    camera_up: np.ndarray
    fov_degrees: float
    near: float
    far: float
    tan_half_fov: float


@dataclass(frozen=True)
class RenderConfig:
    width: int
    height: int
    block_width: int
    block_height: int
    supersampling: int
    projection_samples: int
    exposure: float
    ambient: float
    background: np.ndarray


@dataclass(frozen=True)
class Scene:
    raw: dict[str, Any]
    digest: str
    layers: tuple[Layer, ...]
    lights: tuple[Light, ...]
    screen: Screen
    camera: Camera
    render: RenderConfig

    @property
    def width(self) -> int:
        return self.render.width

    @property
    def height(self) -> int:
        return self.render.height

    def layer(self, layer_index: int) -> Layer:
        return self.layers[layer_index]


def _load_json(path: str | Path) -> dict[str, Any]:
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise SceneValidationError(
            [ValidationIssue("SCENE_READ_ERROR", "$", f"cannot read scene file: {exc}")]
        ) from exc
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        issue = ValidationIssue(
            "INVALID_JSON",
            "$",
            f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
        )
        raise SceneValidationError([issue]) from exc
    if not isinstance(value, dict):
        raise SceneValidationError(
            [ValidationIssue("INVALID_ROOT", "$", "scene root must be a JSON object")]
        )
    return value


def _uv_axes(layer_index: int, vertices: np.ndarray, normals: np.ndarray) -> tuple[int, int, tuple[float, float], tuple[float, float]]:
    weighted = np.sum(np.abs(normals), axis=0)
    axis = int(np.argmax(weighted))
    if axis == 0:
        axis_a, axis_b = 1, 2
    elif axis == 1:
        axis_a, axis_b = 0, 2
    else:
        axis_a, axis_b = 0, 1
    del layer_index
    return (
        axis_a,
        axis_b,
        (float(np.min(vertices[:, axis_a])), float(np.max(vertices[:, axis_a]))),
        (float(np.min(vertices[:, axis_b])), float(np.max(vertices[:, axis_b]))),
    )


def _parse_layers(
    raw: dict[str, Any],
    issues: _Issues,
) -> tuple[list[Layer], bool]:
    if "layers" not in raw:
        issues.add("MISSING_FIELD", "$.layers", "missing required field 'layers'")
        return [], False
    layer_items = raw["layers"]
    if not isinstance(layer_items, list) or not layer_items:
        issues.add("INVALID_LAYERS", "$.layers", "layers must be a non-empty array")
        return [], False
    if len(layer_items) > 64:
        issues.add("TOO_MANY_LAYERS", "$.layers", "at most 64 layers are supported")

    parsed_indices: list[int] = []
    layers: list[Layer] = []
    all_geometry_safe = True
    for position, item in enumerate(layer_items):
        base = f"$.layers[{position}]"
        if not isinstance(item, dict):
            issues.add("INVALID_LAYER", base, "layer must be a JSON object")
            all_geometry_safe = False
            continue
        layer_index = _integer(item, "layer_index", base, issues, minimum=0, maximum=63)
        parsed_indices.append(layer_index)
        transmittance = _number(item, "transmittance", base, issues, layer_index=layer_index, minimum=0.0, maximum=1.0)
        tint = _vector(item, "tint", base, 3, issues, layer_index=layer_index, minimum=0.0, maximum=1.0)
        fiber_seed = _integer(item, "fiber_seed", base, issues, layer_index=layer_index, minimum=0)
        thickness = _number(item, "thickness", base, issues, layer_index=layer_index, minimum=1e-6)

        mesh = item.get("mesh")
        mesh_path = f"{base}.mesh"
        vertices = np.zeros((0, 3), dtype=np.float64)
        triangles = np.zeros((0, 3), dtype=np.int32)
        triangle_cutouts = np.zeros((0,), dtype=np.int32)
        cutout_map: dict[int, tuple[int, ...]] = {}
        layer_geometry_safe = True
        if not isinstance(mesh, dict):
            issues.add("INVALID_MESH", mesh_path, "mesh must be a JSON object", layer_index=layer_index)
            all_geometry_safe = False
            layer_geometry_safe = False
        else:
            vertices_raw = mesh.get("vertices")
            triangles_raw = mesh.get("triangles")
            if not isinstance(vertices_raw, list) or len(vertices_raw) < 3:
                issues.add(
                    "INVALID_VERTICES",
                    f"{mesh_path}.vertices",
                    "vertices must be an array with at least three [x,y,z] entries",
                    layer_index=layer_index,
                )
                layer_geometry_safe = False
            else:
                vertex_rows: list[list[float]] = []
                for vertex_position, vertex in enumerate(vertices_raw):
                    vertex_path = f"{mesh_path}.vertices[{vertex_position}]"
                    if (
                        not isinstance(vertex, list)
                        or len(vertex) != 3
                        or not all(_is_number(component) for component in vertex)
                    ):
                        issues.add(
                            "INVALID_VERTEX",
                            vertex_path,
                            "vertex must contain three finite numbers",
                            layer_index=layer_index,
                        )
                        layer_geometry_safe = False
                        vertex_rows.append([0.0, 0.0, 0.0])
                    else:
                        vertex_rows.append([float(component) for component in vertex])
                vertices = np.asarray(vertex_rows, dtype=np.float64)

            if not isinstance(triangles_raw, list) or not triangles_raw:
                issues.add(
                    "INVALID_TRIANGLES",
                    f"{mesh_path}.triangles",
                    "triangles must be a non-empty array",
                    layer_index=layer_index,
                )
                layer_geometry_safe = False
            else:
                triangle_rows: list[list[int]] = []
                for triangle_position, triangle in enumerate(triangles_raw):
                    triangle_path = f"{mesh_path}.triangles[{triangle_position}]"
                    if not isinstance(triangle, list) or len(triangle) != 3:
                        issues.add(
                            "INVALID_TRIANGLE",
                            triangle_path,
                            "triangle must contain three vertex indices",
                            layer_index=layer_index,
                        )
                        layer_geometry_safe = False
                        triangle_rows.append([0, 0, 0])
                        continue
                    if not all(_is_int(index) for index in triangle):
                        issues.add(
                            "INVALID_TRIANGLE",
                            triangle_path,
                            "triangle vertex indices must be integers",
                            layer_index=layer_index,
                        )
                        layer_geometry_safe = False
                        triangle_rows.append([0, 0, 0])
                        continue
                    for component, index in enumerate(triangle):
                        if index < 0 or index >= len(vertices):
                            issues.add(
                                "MESH_INDEX_OUT_OF_RANGE",
                                f"{triangle_path}[{component}]",
                                f"vertex index {index} is outside [0, {len(vertices) - 1}]",
                                layer_index=layer_index,
                                details={"index": index, "vertex_count": len(vertices)},
                            )
                            layer_geometry_safe = False
                    triangle_rows.append([int(index) for index in triangle])
                triangles = np.asarray(triangle_rows, dtype=np.int32)

            if layer_geometry_safe and len(triangles):
                areas = triangle_areas(vertices, triangles)
                for triangle_position, area in enumerate(areas):
                    if float(area) <= 1e-12:
                        issues.add(
                            "DEGENERATE_TRIANGLE",
                            f"{mesh_path}.triangles[{triangle_position}]",
                            "triangle area is zero or below tolerance",
                            layer_index=layer_index,
                            details={"area": float(area)},
                        )
                        layer_geometry_safe = False

        cutouts_raw = item.get("cutouts")
        if not isinstance(cutouts_raw, list):
            issues.add(
                "INVALID_CUTOUTS",
                f"{base}.cutouts",
                "cutouts must be an array (it may be empty)",
                layer_index=layer_index,
            )
            layer_geometry_safe = False
            cutouts_raw = []

        triangle_cutouts = np.zeros((len(triangles),), dtype=np.int32)
        assigned_triangles: set[int] = set()
        seen_cutout_ids: set[int] = set()
        max_cutout_id = 0
        for cutout_position, cutout in enumerate(cutouts_raw):
            cutout_path = f"{base}.cutouts[{cutout_position}]"
            if not isinstance(cutout, dict):
                issues.add("INVALID_CUTOUT", cutout_path, "cutout must be a JSON object", layer_index=layer_index)
                layer_geometry_safe = False
                continue
            cutout_id = _integer(cutout, "cutout_id", cutout_path, issues, layer_index=layer_index, minimum=1)
            max_cutout_id = max(max_cutout_id, cutout_id)
            if cutout_id in seen_cutout_ids:
                issues.add(
                    "DUPLICATE_CUTOUT_ID",
                    f"{cutout_path}.cutout_id",
                    f"cutout_id {cutout_id} is declared more than once",
                    layer_index=layer_index,
                    cutout_id=cutout_id,
                )
                layer_geometry_safe = False
            seen_cutout_ids.add(cutout_id)

            indices_raw = cutout.get("triangle_indices")
            if not isinstance(indices_raw, list) or not indices_raw:
                issues.add(
                    "INVALID_CUTOUT_TRIANGLES",
                    f"{cutout_path}.triangle_indices",
                    "triangle_indices must be a non-empty array",
                    layer_index=layer_index,
                    cutout_id=cutout_id,
                )
                layer_geometry_safe = False
                continue
            valid_indices: list[int] = []
            for list_position, triangle_index in enumerate(indices_raw):
                index_path = f"{cutout_path}.triangle_indices[{list_position}]"
                if not _is_int(triangle_index):
                    issues.add(
                        "INVALID_CUTOUT_TRIANGLES",
                        index_path,
                        "triangle index must be an integer",
                        layer_index=layer_index,
                        cutout_id=cutout_id,
                    )
                    layer_geometry_safe = False
                    continue
                if triangle_index < 0 or triangle_index >= len(triangles):
                    issues.add(
                        "MESH_INDEX_OUT_OF_RANGE",
                        index_path,
                        f"triangle index {triangle_index} is outside [0, {len(triangles) - 1}]",
                        layer_index=layer_index,
                        cutout_id=cutout_id,
                        details={"index": triangle_index, "triangle_count": len(triangles)},
                    )
                    layer_geometry_safe = False
                    continue
                if triangle_index in assigned_triangles:
                    issues.add(
                        "DUPLICATE_CUTOUT_TRIANGLE",
                        index_path,
                        f"triangle {triangle_index} is assigned to more than one cutout",
                        layer_index=layer_index,
                        cutout_id=cutout_id,
                    )
                    layer_geometry_safe = False
                    continue
                assigned_triangles.add(int(triangle_index))
                valid_indices.append(int(triangle_index))
                triangle_cutouts[int(triangle_index)] = cutout_id
            if valid_indices:
                cutout_map[cutout_id] = tuple(valid_indices)

        if max_cutout_id > 0:
            for expected_id in range(1, max_cutout_id + 1):
                if expected_id not in seen_cutout_ids:
                    issues.add(
                        "MISSING_CUTOUT_ID",
                        f"{base}.cutouts",
                        f"cutout_id {expected_id} is missing from the contiguous sequence",
                        layer_index=layer_index,
                        cutout_id=expected_id,
                    )
                    layer_geometry_safe = False

        if layer_geometry_safe:
            normals = face_normals(vertices, triangles)
            plane = fit_plane_frame(vertices, fallback_normal=np.array([0.0, 0.0, 1.0]))
            axis_a, axis_b, bounds_a, bounds_b = _uv_axes(layer_index, vertices, normals)
            layers.append(
                Layer(
                    layer_index=layer_index,
                    vertices=vertices,
                    triangles=triangles,
                    triangle_cutout_ids=triangle_cutouts,
                    cutouts=cutout_map,
                    transmittance=transmittance,
                    tint=tint,
                    fiber_seed=fiber_seed,
                    thickness=thickness,
                    face_normals=normals,
                    plane=plane,
                    uv_axis_a=axis_a,
                    uv_axis_b=axis_b,
                    uv_bounds_a=bounds_a,
                    uv_bounds_b=bounds_b,
                )
            )
        else:
            all_geometry_safe = False

    if parsed_indices:
        counts: dict[int, int] = {}
        for index in parsed_indices:
            counts[index] = counts.get(index, 0) + 1
        for index, count in sorted(counts.items()):
            if count > 1:
                issues.add(
                    "DUPLICATE_LAYER_INDEX",
                    "$.layers",
                    f"layer_index {index} occurs {count} times",
                    layer_index=index,
                )
        for expected in range(max(parsed_indices) + 1):
            if expected not in counts:
                issues.add(
                    "MISSING_LAYER_INDEX",
                    "$.layers",
                    f"layer_index {expected} is missing from the contiguous sequence",
                    layer_index=expected,
                )

    layers.sort(key=lambda layer: layer.layer_index)
    return layers, all_geometry_safe and len(layers) == len(layer_items)


def _parse_lights(raw: dict[str, Any], layers: list[Layer], issues: _Issues) -> list[Light]:
    if "lights" not in raw:
        issues.add("MISSING_FIELD", "$.lights", "missing required field 'lights'")
        return []
    items = raw["lights"]
    if not isinstance(items, list) or len(items) != 3:
        issues.add("INVALID_LIGHTS", "$.lights", "exactly three oil lamps are required")
        return []
    lights: list[Light] = []
    seen_ids: set[str] = set()
    for position, item in enumerate(items):
        base = f"$.lights[{position}]"
        if not isinstance(item, dict):
            issues.add("INVALID_LIGHT", base, "light must be a JSON object")
            continue
        light_id = item.get("id")
        if not isinstance(light_id, str) or not light_id:
            issues.add("INVALID_LIGHT_ID", f"{base}.id", "light id must be a non-empty string")
            light_id = f"light_{position}"
        if light_id in seen_ids:
            issues.add("DUPLICATE_LIGHT_ID", f"{base}.id", f"light id '{light_id}' is duplicated")
        seen_ids.add(light_id)
        light_position = _vector(item, "position", base, 3, issues)
        intensity = _number(item, "intensity", base, issues, minimum=0.0)
        color = _vector(
            item,
            "color",
            base,
            3,
            issues,
            minimum=0.0,
            maximum=1.0,
            default=[1.0, 0.78, 0.46],
            required=False,
        )
        radius = _number(
            item,
            "radius",
            base,
            issues,
            minimum=1e-6,
            default=0.1,
            required=False,
        )
        softness_gain = _number(
            item,
            "softness_gain",
            base,
            issues,
            minimum=0.0,
            default=1.0,
            required=False,
        )

        for layer in layers:
            solid = layer.solid_triangles()
            if len(solid) == 0:
                continue
            extent = float(np.linalg.norm(np.ptp(layer.vertices, axis=0)))
            tolerance = max(1e-8, extent * 1e-8)
            if point_inside_triangular_mesh(light_position, layer.vertices, solid, tolerance):
                issues.add(
                    "LIGHT_INSIDE_GEOMETRY",
                    f"{base}.position",
                    f"light '{light_id}' lies on or inside layer {layer.layer_index} geometry",
                    layer_index=layer.layer_index,
                    details={"light_id": light_id, "position": light_position.tolist()},
                )
        lights.append(
            Light(
                light_id=light_id,
                position=light_position,
                intensity=intensity,
                color=color,
                radius=radius,
                softness_gain=softness_gain,
            )
        )
    return lights


def _parse_camera(raw: dict[str, Any], issues: _Issues) -> Camera:
    base = "$.camera"
    if "camera" not in raw or not isinstance(raw["camera"], dict):
        issues.add("INVALID_CAMERA", base, "camera must be a JSON object")
        camera_raw: dict[str, Any] = {}
    else:
        camera_raw = raw["camera"]
    position = _vector(camera_raw, "position", base, 3, issues)
    target = _vector(camera_raw, "target", base, 3, issues)
    up = _vector(camera_raw, "up", base, 3, issues)
    fov = _number(camera_raw, "fov_degrees", base, issues, minimum=1.0, maximum=179.0)
    near = _number(camera_raw, "near", base, issues, minimum=1e-9)
    far = _number(camera_raw, "far", base, issues, minimum=1e-9)
    if far <= near:
        issues.add("INVALID_CAMERA_CLIP", f"{base}.far", "far plane must be greater than near plane")

    forward_vec = target - position
    forward_len = float(np.linalg.norm(forward_vec))
    up_len = float(np.linalg.norm(up))
    forward = np.array([0.0, 0.0, 1.0])
    right = np.array([1.0, 0.0, 0.0])
    camera_up = np.array([0.0, 1.0, 0.0])
    if forward_len <= 1e-12:
        issues.add("INVALID_CAMERA", f"{base}.target", "camera target must differ from position")
    else:
        forward = forward_vec / forward_len
    if up_len <= 1e-12:
        issues.add("INVALID_CAMERA", f"{base}.up", "camera up vector must be non-zero")
    else:
        right_candidate = np.cross(up, forward)
        right_len = float(np.linalg.norm(right_candidate))
        if right_len <= 1e-10:
            issues.add(
                "INVALID_CAMERA_BASIS",
                f"{base}.up",
                "camera up vector must not be parallel to the view direction",
            )
        else:
            right = right_candidate / right_len
            camera_up = np.cross(forward, right)
            camera_up = camera_up / max(float(np.linalg.norm(camera_up)), 1e-30)
    return Camera(
        position=position,
        target=target,
        up=up,
        forward=forward,
        right=right,
        camera_up=camera_up,
        fov_degrees=fov,
        near=near,
        far=far,
        tan_half_fov=math.tan(math.radians(fov) * 0.5),
    )


def _parse_screen(raw: dict[str, Any], issues: _Issues) -> Screen:
    base = "$.screen"
    if "screen" not in raw or not isinstance(raw["screen"], dict):
        issues.add("INVALID_SCREEN", base, "screen must be a JSON object")
        screen_raw: dict[str, Any] = {}
    else:
        screen_raw = raw["screen"]
    origin = _vector(screen_raw, "origin", base, 3, issues)
    axis_u_raw = _vector(screen_raw, "u_axis", base, 3, issues)
    axis_v_raw = _vector(screen_raw, "v_axis", base, 3, issues)
    width = _number(screen_raw, "width", base, issues, minimum=1e-9)
    height = _number(screen_raw, "height", base, issues, minimum=1e-9)
    tint = _vector(
        screen_raw,
        "tint",
        base,
        3,
        issues,
        minimum=0.0,
        maximum=1.0,
        default=[0.62, 0.48, 0.28],
        required=False,
    )
    fiber_seed = _integer(
        screen_raw,
        "fiber_seed",
        base,
        issues,
        minimum=0,
        default=1,
        required=False,
    )
    length_u = float(np.linalg.norm(axis_u_raw))
    length_v = float(np.linalg.norm(axis_v_raw))
    if length_u <= 1e-12:
        issues.add("INVALID_SCREEN_BASIS", f"{base}.u_axis", "u_axis must be non-zero")
        axis_u = np.array([1.0, 0.0, 0.0])
    else:
        axis_u = axis_u_raw / length_u
    if length_v <= 1e-12:
        issues.add("INVALID_SCREEN_BASIS", f"{base}.v_axis", "v_axis must be non-zero")
        axis_v = np.array([0.0, 1.0, 0.0])
    else:
        axis_v = axis_v_raw / length_v
    if abs(float(np.dot(axis_u, axis_v))) > 1e-6:
        issues.add(
            "INVALID_SCREEN_BASIS",
            f"{base}.v_axis",
            "screen axes must be orthogonal",
            details={"dot": float(np.dot(axis_u, axis_v))},
        )
    normal = np.cross(axis_u, axis_v)
    normal = normal / max(float(np.linalg.norm(normal)), 1e-30)
    return Screen(
        origin=origin,
        axis_u=axis_u,
        axis_v=axis_v,
        width=width,
        height=height,
        normal=normal,
        tint=tint,
        fiber_seed=fiber_seed,
    )


def _parse_render(raw: dict[str, Any], issues: _Issues) -> RenderConfig:
    base = "$.render"
    if "render" not in raw or not isinstance(raw["render"], dict):
        issues.add("INVALID_RENDER", base, "render must be a JSON object")
        render_raw: dict[str, Any] = {}
    else:
        render_raw = raw["render"]
    width = _integer(render_raw, "width", base, issues, minimum=1, maximum=4096)
    height = _integer(render_raw, "height", base, issues, minimum=1, maximum=4096)
    block_width = _integer(render_raw, "block_width", base, issues, minimum=1, maximum=max(width, 1))
    block_height = _integer(render_raw, "block_height", base, issues, minimum=1, maximum=max(height, 1))
    supersampling = _integer(
        render_raw,
        "supersampling",
        base,
        issues,
        minimum=1,
        maximum=4,
        default=2,
        required=False,
    )
    projection_samples = _integer(
        render_raw,
        "projection_samples",
        base,
        issues,
        minimum=1,
        maximum=8,
        default=4,
        required=False,
    )
    exposure = _number(
        render_raw,
        "exposure",
        base,
        issues,
        minimum=1e-6,
        default=0.85,
        required=False,
    )
    ambient = _number(
        render_raw,
        "ambient",
        base,
        issues,
        minimum=0.0,
        default=0.12,
        required=False,
    )
    background = _vector(
        render_raw,
        "background",
        base,
        4,
        issues,
        minimum=0.0,
        maximum=1.0,
        default=[0.01, 0.008, 0.006, 1.0],
        required=False,
    )
    return RenderConfig(
        width=width,
        height=height,
        block_width=min(block_width, max(width, 1)),
        block_height=min(block_height, max(height, 1)),
        supersampling=supersampling,
        projection_samples=projection_samples,
        exposure=exposure,
        ambient=ambient,
        background=background,
    )


def validate_scene_object(raw: dict[str, Any]) -> Scene:
    issues = _Issues()
    if not isinstance(raw, dict):
        issues.add("INVALID_ROOT", "$", "scene root must be a JSON object")
        issues.raise_if_any()
    schema_version = _integer(raw, "schema_version", "$", issues, minimum=1)
    if schema_version != 1:
        issues.add(
            "UNSUPPORTED_SCHEMA_VERSION",
            "$.schema_version",
            "only schema_version 1 is supported",
        )

    layers, geometry_safe = _parse_layers(raw, issues)
    lights = _parse_lights(raw, layers if geometry_safe else [], issues)
    camera = _parse_camera(raw, issues)
    screen = _parse_screen(raw, issues)
    render = _parse_render(raw, issues)

    camera_depth = float(np.dot(screen.origin - camera.position, camera.forward))
    if camera_depth <= camera.near:
        issues.add(
            "SCREEN_BEHIND_CAMERA",
            "$.screen.origin",
            "screen origin must be in front of the camera near plane",
            details={"camera_depth": camera_depth, "near": camera.near},
        )
    camera_screen_dot = abs(float(np.dot(camera.position - screen.origin, screen.normal)))
    if camera_screen_dot <= 1e-9:
        issues.add(
            "CAMERA_ON_SCREEN_PLANE",
            "$.camera.position",
            "camera must not lie on the screen plane",
        )
    if geometry_safe:
        for layer in layers:
            layer_depth = float(np.dot(layer.plane.origin - camera.position, camera.forward))
            if layer_depth < camera.near or layer_depth > camera.far:
                issues.add(
                    "LAYER_OUTSIDE_CAMERA_CLIP",
                    f"$.layers[{layer.layer_index}].mesh.vertices",
                    "layer plane must lie between the camera near and far planes",
                    layer_index=layer.layer_index,
                    details={"camera_depth": layer_depth, "near": camera.near, "far": camera.far},
                )
            if layer_depth >= camera_depth - 1e-8:
                issues.add(
                    "SCREEN_NOT_BEHIND_LAYER",
                    "$.screen.origin",
                    "screen origin must be behind every puppet layer",
                    layer_index=layer.layer_index,
                    details={"layer_depth": layer_depth, "screen_depth": camera_depth},
                )

    issues.raise_if_any()
    return Scene(
        raw=raw,
        digest=sha256_json(raw),
        layers=tuple(layers),
        lights=tuple(lights),
        screen=screen,
        camera=camera,
        render=render,
    )


def load_scene(path: str | Path) -> Scene:
    return validate_scene_object(_load_json(path))

