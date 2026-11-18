"""Scene schema, deterministic sample generation, and strict validation."""

from __future__ import annotations

import copy
import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import SceneValidationError, ValidationIssue

ENGINE_VERSION = "shadowbox-1.0.0"
SCENE_FORMAT = "shadowbox-scene-v1"
REQUIRED_LAYER_INDICES = (0, 1, 2, 3, 4)


@dataclass(frozen=True)
class Mesh:
    vertices: np.ndarray
    uvs: np.ndarray
    triangles: np.ndarray

    @property
    def bounds(self) -> tuple[float, float, float, float]:
        return (
            float(self.vertices[:, 0].min()),
            float(self.vertices[:, 0].max()),
            float(self.vertices[:, 1].min()),
            float(self.vertices[:, 1].max()),
        )


@dataclass(frozen=True)
class Layer:
    layer_index: int
    name: str
    air_gap_to_prev_mm: float
    transmittance: float
    scatter_color: tuple[float, float, float]
    texture_seed: int
    shadow_samples_per_axis: int
    projection_gain: float
    world_z_mm: float
    mesh: Mesh


@dataclass(frozen=True)
class Camera:
    position_mm: np.ndarray
    target_mm: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    width: int
    height: int
    samples_per_pixel: int
    block_width: int
    block_height: int


@dataclass(frozen=True)
class PointLight:
    position_mm: np.ndarray
    intensity: float
    color: tuple[float, float, float]
    reference_distance_mm: float


@dataclass(frozen=True)
class TextureConfig:
    width: int
    height: int
    projection_width: int
    projection_height: int
    fiber_contrast: float
    hole_coverage: float
    penumbra_base_mm: float
    penumbra_per_gap_mm: float


@dataclass(frozen=True)
class Scene:
    source: dict[str, Any]
    digest: str
    base_z_mm: float
    layers: tuple[Layer, ...]
    camera: Camera
    light: PointLight
    texture: TextureConfig

    @property
    def layers_inner_to_outer(self) -> tuple[Layer, ...]:
        return tuple(sorted(self.layers, key=lambda layer: layer.layer_index))

    def layer(self, layer_index: int) -> Layer:
        for layer in self.layers:
            if layer.layer_index == layer_index:
                return layer
        raise KeyError(layer_index)


def canonical_digest(value: Any) -> str:
    payload = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _is_number(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
    )


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _vector(value: Any, length: int) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != length:
        return None
    if not all(_is_number(item) for item in value):
        return None
    return np.asarray(value, dtype=np.float64)


def _issue(
    issues: list[ValidationIssue],
    code: str,
    layer_index: int | None,
    message: str,
    **details: Any,
) -> None:
    issues.append(
        ValidationIssue(
            code=code,
            layer_index=layer_index,
            message=message,
            details=details,
        )
    )


def _parse_texture(source: dict[str, Any], issues: list[ValidationIssue]) -> TextureConfig:
    tex = source.get("texture")
    if not isinstance(tex, dict):
        _issue(issues, "INVALID_TEXTURE_CONFIG", None, "texture must be an object")
        tex = {}
    width = tex.get("width", 256)
    height = tex.get("height", 256)
    projection_width = tex.get("projection_width", 192)
    projection_height = tex.get("projection_height", 192)
    for label, value, upper in (
        ("width", width, 2048),
        ("height", height, 2048),
        ("projection_width", projection_width, 1024),
        ("projection_height", projection_height, 1024),
    ):
        if not _is_int(value) or not 16 <= value <= upper:
            _issue(
                issues,
                "INVALID_TEXTURE_CONFIG",
                None,
                f"texture.{label} must be an integer in [16, {upper}]",
            )
    contrast = tex.get("fiber_contrast", 0.32)
    coverage = tex.get("hole_coverage", 0.13)
    base_penumbra = tex.get("penumbra_base_mm", 0.45)
    per_gap = tex.get("penumbra_per_gap_mm", 0.16)
    for label, value, lower, upper in (
        ("fiber_contrast", contrast, 0.0, 1.0),
        ("hole_coverage", coverage, 0.0, 0.8),
        ("penumbra_base_mm", base_penumbra, 0.0, 10.0),
        ("penumbra_per_gap_mm", per_gap, 0.0, 5.0),
    ):
        if not _is_number(value) or not lower <= float(value) <= upper:
            _issue(
                issues,
                "INVALID_TEXTURE_CONFIG",
                None,
                f"texture.{label} must be in [{lower}, {upper}]",
            )
    return TextureConfig(
        width=int(width) if _is_int(width) else 256,
        height=int(height) if _is_int(height) else 256,
        projection_width=int(projection_width) if _is_int(projection_width) else 192,
        projection_height=int(projection_height) if _is_int(projection_height) else 192,
        fiber_contrast=float(contrast) if _is_number(contrast) else 0.32,
        hole_coverage=float(coverage) if _is_number(coverage) else 0.13,
        penumbra_base_mm=float(base_penumbra) if _is_number(base_penumbra) else 0.45,
        penumbra_per_gap_mm=float(per_gap) if _is_number(per_gap) else 0.16,
    )


def _parse_mesh_and_material(
    raw_layer: Any,
    layer_index: int | None,
    issues: list[ValidationIssue],
) -> tuple[Mesh | None, dict[str, Any]]:
    material: dict[str, Any] = {}
    if not isinstance(raw_layer, dict):
        _issue(
            issues,
            "INVALID_LAYER",
            layer_index,
            "layer must be an object",
        )
        return None, material

    for key in ("name", "transmittance", "scatter_color", "texture_seed"):
        if key not in raw_layer:
            _issue(
                issues,
                "MISSING_FIELD",
                layer_index,
                f"layer.{key} is required",
                field=key,
            )

    name = raw_layer.get("name")
    if not isinstance(name, str) or not name:
        _issue(issues, "INVALID_LAYER_NAME", layer_index, "name must be a non-empty string")
    else:
        material["name"] = name

    transmittance = raw_layer.get("transmittance")
    if not _is_number(transmittance) or not 0.0 < float(transmittance) <= 1.0:
        _issue(
            issues,
            "TRANSMITTANCE_OUT_OF_RANGE",
            layer_index,
            "transmittance must be finite and in (0, 1]",
            value=transmittance,
        )
    else:
        material["transmittance"] = float(transmittance)

    scatter = _vector(raw_layer.get("scatter_color"), 3)
    if scatter is None or np.any(scatter < 0.0) or np.any(scatter > 1.0):
        _issue(
            issues,
            "SCATTER_COLOR_OUT_OF_RANGE",
            layer_index,
            "scatter_color must contain three finite values in [0, 1]",
        )
    else:
        material["scatter_color"] = tuple(float(v) for v in scatter)

    seed = raw_layer.get("texture_seed")
    if not _is_int(seed) or seed < 0 or seed > 2**63 - 1:
        _issue(
            issues,
            "INVALID_TEXTURE_SEED",
            layer_index,
            "texture_seed must be a non-negative 63-bit integer",
        )
    else:
        material["texture_seed"] = int(seed)

    density = raw_layer.get("shadow_samples_per_axis", 5)
    if not _is_int(density) or not 1 <= density <= 31:
        _issue(
            issues,
            "INVALID_SHADOW_DENSITY",
            layer_index,
            "shadow_samples_per_axis must be an integer in [1, 31]",
        )
        density = 5
    material["shadow_samples_per_axis"] = int(density)

    gain = raw_layer.get("projection_gain", 1.0)
    if not _is_number(gain) or not 0.0 <= float(gain) <= 8.0:
        _issue(
            issues,
            "INVALID_PROJECTION_GAIN",
            layer_index,
            "projection_gain must be in [0, 8]",
        )
        gain = 1.0
    material["projection_gain"] = float(gain)

    raw_mesh = raw_layer.get("mesh")
    if not isinstance(raw_mesh, dict):
        _issue(issues, "INVALID_MESH", layer_index, "mesh must be an object")
        return None, material

    raw_vertices = raw_mesh.get("vertices")
    if not isinstance(raw_vertices, list) or len(raw_vertices) < 3:
        _issue(
            issues,
            "INVALID_MESH",
            layer_index,
            "mesh.vertices must contain at least three vertices",
        )
        return None, material

    vertices: list[list[float]] = []
    for vertex_index, vertex in enumerate(raw_vertices):
        parsed = _vector(vertex, 3)
        if parsed is None:
            _issue(
                issues,
                "INVALID_VERTEX",
                layer_index,
                f"vertex {vertex_index} must contain three finite numbers",
                vertex_index=vertex_index,
            )
            vertices.append([0.0, 0.0, 0.0])
        else:
            vertices.append([float(v) for v in parsed])
    vertex_array = np.asarray(vertices, dtype=np.float64)

    raw_uvs = raw_mesh.get("uvs")
    if raw_uvs is not None:
        if not isinstance(raw_uvs, list) or len(raw_uvs) != len(raw_vertices):
            _issue(
                issues,
                "INVALID_UVS",
                layer_index,
                "mesh.uvs must have one two-component value per vertex",
            )
            uv_array = np.zeros((len(raw_vertices), 2), dtype=np.float64)
        else:
            parsed_uvs: list[list[float]] = []
            for uv_index, uv in enumerate(raw_uvs):
                parsed_uv = _vector(uv, 2)
                if parsed_uv is None:
                    _issue(
                        issues,
                        "INVALID_UVS",
                        layer_index,
                        f"uv {uv_index} must contain two finite numbers",
                        vertex_index=uv_index,
                    )
                    parsed_uvs.append([0.0, 0.0])
                else:
                    parsed_uvs.append([float(parsed_uv[0]), float(parsed_uv[1])])
            uv_array = np.asarray(parsed_uvs, dtype=np.float64)
    else:
        xmin, xmax = float(vertex_array[:, 0].min()), float(vertex_array[:, 0].max())
        ymin, ymax = float(vertex_array[:, 1].min()), float(vertex_array[:, 1].max())
        uv_array = np.column_stack(
            (
                (vertex_array[:, 0] - xmin) / max(xmax - xmin, 1e-12),
                (vertex_array[:, 1] - ymin) / max(ymax - ymin, 1e-12),
            )
        )

    if not np.allclose(vertex_array[:, 2], 0.0, atol=1e-7, rtol=0.0):
        _issue(
            issues,
            "NON_PLANAR_LAYER",
            layer_index,
            "layer-local mesh vertices must all have z=0; layer height comes from air gaps",
        )

    raw_triangles = raw_mesh.get("triangles")
    if not isinstance(raw_triangles, list) or len(raw_triangles) < 1:
        _issue(
            issues,
            "INVALID_MESH",
            layer_index,
            "mesh.triangles must contain at least one triangle",
        )
        return None, material

    triangles: list[list[int]] = []
    for triangle_index, triangle in enumerate(raw_triangles):
        if (
            not isinstance(triangle, list)
            or len(triangle) != 3
            or not all(_is_int(index) for index in triangle)
        ):
            _issue(
                issues,
                "INVALID_TRIANGLE_INDEX",
                layer_index,
                f"triangle {triangle_index} must contain three integer indices",
                triangle_index=triangle_index,
            )
            continue
        indices = [int(index) for index in triangle]
        out_of_range = [
            index for index in indices if index < 0 or index >= len(vertex_array)
        ]
        if out_of_range:
            _issue(
                issues,
                "MESH_INDEX_OUT_OF_RANGE",
                layer_index,
                f"triangle {triangle_index} references out-of-range vertices",
                triangle_index=triangle_index,
                vertex_indices=indices,
            )
            continue
        if len(set(indices)) != 3:
            _issue(
                issues,
                "DEGENERATE_TRIANGLE",
                layer_index,
                f"triangle {triangle_index} repeats a vertex index",
                triangle_index=triangle_index,
                vertex_indices=indices,
            )
            continue
        triangle_points = vertex_array[indices]
        normal = np.cross(
            triangle_points[1] - triangle_points[0],
            triangle_points[2] - triangle_points[0],
        )
        area2 = float(np.linalg.norm(normal))
        if area2 <= 1e-10:
            _issue(
                issues,
                "DEGENERATE_TRIANGLE",
                layer_index,
                f"triangle {triangle_index} has zero area",
                triangle_index=triangle_index,
                area2=area2,
            )
            continue
        triangles.append(indices)

    if not triangles:
        return None, material
    return (
        Mesh(
            vertices=vertex_array,
            uvs=uv_array,
            triangles=np.asarray(triangles, dtype=np.int64),
        ),
        material,
    )


def _parse_camera(raw: Any, issues: list[ValidationIssue]) -> Camera:
    if not isinstance(raw, dict):
        _issue(issues, "INVALID_CAMERA", None, "camera must be an object")
        raw = {}
    position = _vector(raw.get("position_mm"), 3)
    target = _vector(raw.get("target_mm"), 3)
    up = _vector(raw.get("up"), 3)
    if position is None:
        _issue(issues, "INVALID_CAMERA", None, "camera.position_mm must be a finite 3-vector")
        position = np.zeros(3, dtype=np.float64)
    if target is None:
        _issue(issues, "INVALID_CAMERA", None, "camera.target_mm must be a finite 3-vector")
        target = np.asarray([0.0, 0.0, 1.0], dtype=np.float64)
    if up is None or np.linalg.norm(up) <= 1e-12:
        _issue(issues, "INVALID_CAMERA", None, "camera.up must be a nonzero finite 3-vector")
        up = np.asarray([0.0, 1.0, 0.0], dtype=np.float64)
    direction = target - position
    if np.linalg.norm(direction) <= 1e-12:
        _issue(
            issues,
            "INVALID_CAMERA",
            None,
            "camera position and target must differ",
        )
    elif np.linalg.norm(np.cross(direction, up)) <= 1e-12:
        _issue(
            issues,
            "INVALID_CAMERA",
            None,
            "camera up vector must not be parallel to the view direction",
        )
    fov = raw.get("fov_y_deg")
    if not _is_number(fov) or not 1.0 <= float(fov) < 179.0:
        _issue(issues, "INVALID_CAMERA", None, "camera.fov_y_deg must be in (0, 179)")
        fov = 50.0
    resolution = raw.get("resolution")
    if (
        not isinstance(resolution, list)
        or len(resolution) != 2
        or not all(_is_int(value) for value in resolution)
        or not all(16 <= value <= 4096 for value in resolution)
    ):
        _issue(
            issues,
            "INVALID_RESOLUTION",
            None,
            "camera.resolution must contain two integers in [16, 4096]",
        )
        resolution = [320, 256]
    block = raw.get("block_size", [64, 64])
    if (
        not isinstance(block, list)
        or len(block) != 2
        or not all(_is_int(value) for value in block)
        or not all(8 <= value <= 2048 for value in block)
    ):
        _issue(
            issues,
            "INVALID_BLOCK_SIZE",
            None,
            "camera.block_size must contain two integers in [8, 2048]",
        )
        block = [64, 64]
    spp = raw.get("samples_per_pixel", 1)
    if not _is_int(spp) or spp not in (1, 2, 3, 4, 5, 6, 7, 8):
        _issue(
            issues,
            "INVALID_SAMPLES_PER_PIXEL",
            None,
            "camera.samples_per_pixel must be one of 1..8",
        )
        spp = 1
    return Camera(
        position_mm=np.asarray(position, dtype=np.float64),
        target_mm=np.asarray(target, dtype=np.float64),
        up=np.asarray(up, dtype=np.float64),
        fov_y_deg=float(fov),
        width=int(resolution[0]),
        height=int(resolution[1]),
        samples_per_pixel=int(spp),
        block_width=int(block[0]),
        block_height=int(block[1]),
    )


def _parse_light(raw: Any, issues: list[ValidationIssue]) -> PointLight:
    if not isinstance(raw, dict):
        _issue(issues, "INVALID_LIGHT", None, "light must be an object")
        raw = {}
    position = _vector(raw.get("position_mm"), 3)
    if position is None:
        _issue(issues, "INVALID_LIGHT", None, "light.position_mm must be a finite 3-vector")
        position = np.zeros(3, dtype=np.float64)
    intensity = raw.get("intensity")
    if not _is_number(intensity) or float(intensity) <= 0.0:
        _issue(
            issues,
            "INVALID_LIGHT",
            None,
            "light.intensity must be finite and positive",
        )
        intensity = 1.0
    color = _vector(raw.get("color", [1.0, 0.82, 0.58]), 3)
    if color is None or np.any(color < 0.0) or np.any(color > 1.0):
        _issue(
            issues,
            "INVALID_LIGHT",
            None,
            "light.color must contain three values in [0, 1]",
        )
        color = np.asarray([1.0, 0.82, 0.58], dtype=np.float64)
    reference = raw.get("reference_distance_mm", 100.0)
    if not _is_number(reference) or not 1.0 <= float(reference) <= 10000.0:
        _issue(
            issues,
            "INVALID_LIGHT",
            None,
            "light.reference_distance_mm must be in [1, 10000]",
        )
        reference = 100.0
    return PointLight(
        position_mm=np.asarray(position, dtype=np.float64),
        intensity=float(intensity),
        color=tuple(float(v) for v in color),
        reference_distance_mm=float(reference),
    )


def _point_in_triangle_2d(
    point: np.ndarray, a: np.ndarray, b: np.ndarray, c: np.ndarray
) -> bool:
    p = point[:2]
    v0 = c[:2] - a[:2]
    v1 = b[:2] - a[:2]
    v2 = p - a[:2]
    den = v0[0] * v1[1] - v1[0] * v0[1]
    if abs(den) <= 1e-14:
        return False
    u = (v2[0] * v1[1] - v1[0] * v2[1]) / den
    v = (v0[0] * v2[1] - v2[0] * v0[1]) / den
    return u >= -1e-9 and v >= -1e-9 and u + v <= 1.0 + 1e-9


def _check_position_rules(
    issues: list[ValidationIssue],
    camera: Camera,
    light: PointLight,
    layers: list[Layer],
) -> None:
    if not layers:
        return
    ordered = sorted(layers, key=lambda layer: layer.layer_index)
    inner, outer = ordered[0], ordered[-1]
    z_tolerance = 1e-6

    if light.position_mm[2] >= inner.world_z_mm - z_tolerance:
        _issue(
            issues,
            "LIGHT_SIDE_INVALID",
            inner.layer_index,
            "point light must be on the inner side of the innermost layer",
            light_z_mm=float(light.position_mm[2]),
            layer_z_mm=float(inner.world_z_mm),
        )
    if camera.position_mm[2] <= outer.world_z_mm + z_tolerance:
        _issue(
            issues,
            "CAMERA_SIDE_INVALID",
            outer.layer_index,
            "camera must be on the outer side of the outermost layer",
            camera_z_mm=float(camera.position_mm[2]),
            layer_z_mm=float(outer.world_z_mm),
        )

    for layer in ordered:
        world = layer.mesh.vertices.copy()
        world[:, 2] += layer.world_z_mm
        if abs(light.position_mm[2] - layer.world_z_mm) <= z_tolerance:
            for triangle in layer.mesh.triangles:
                if _point_in_triangle_2d(
                    light.position_mm,
                    world[triangle[0]],
                    world[triangle[1]],
                    world[triangle[2]],
                ):
                    _issue(
                        issues,
                        "LIGHT_INSIDE_LAYER_GEOMETRY",
                        layer.layer_index,
                        "point light lies on paper geometry",
                    )
                    break
        if abs(camera.position_mm[2] - layer.world_z_mm) <= z_tolerance:
            for triangle in layer.mesh.triangles:
                if _point_in_triangle_2d(
                    camera.position_mm,
                    world[triangle[0]],
                    world[triangle[1]],
                    world[triangle[2]],
                ):
                    _issue(
                        issues,
                        "CAMERA_INSIDE_LAYER_GEOMETRY",
                        layer.layer_index,
                        "camera lies on paper geometry",
                    )
                    break


def validate_scene(source: Any) -> Scene:
    """Validate a parsed scene object and return an immutable runtime scene."""

    issues: list[ValidationIssue] = []
    if not isinstance(source, dict):
        raise SceneValidationError(
            [ValidationIssue("INVALID_SCENE", None, "scene root must be an object")]
        )
    if source.get("format") != SCENE_FORMAT:
        _issue(
            issues,
            "INVALID_SCENE_FORMAT",
            None,
            f"format must be {SCENE_FORMAT!r}",
        )

    stack = source.get("stack")
    if not isinstance(stack, dict):
        _issue(issues, "INVALID_STACK", None, "stack must be an object")
        stack = {}
    base_z = stack.get("base_z_mm", 24.0)
    if not _is_number(base_z):
        _issue(issues, "INVALID_STACK", None, "stack.base_z_mm must be finite")
        base_z = 24.0
    layer_count = stack.get("layer_count", 5)
    if not _is_int(layer_count) or layer_count != 5:
        _issue(issues, "INVALID_LAYER_COUNT", None, "stack.layer_count must be 5")

    camera = _parse_camera(source.get("camera"), issues)
    light = _parse_light(source.get("light"), issues)
    texture = _parse_texture(source, issues)

    raw_layers = source.get("layers")
    if not isinstance(raw_layers, list):
        _issue(issues, "INVALID_LAYERS", None, "layers must be an array")
        raw_layers = []

    seen: dict[int, int] = {}
    parsed_by_index: dict[int, tuple[Layer | None, dict[str, Any], float]] = {}
    for position, raw_layer in enumerate(raw_layers):
        layer_index: int | None = None
        if isinstance(raw_layer, dict) and _is_int(raw_layer.get("layer_index")):
            layer_index = int(raw_layer["layer_index"])
            if layer_index not in REQUIRED_LAYER_INDICES:
                _issue(
                    issues,
                    "UNKNOWN_LAYER_INDEX",
                    layer_index,
                    f"layer_index must be one of {REQUIRED_LAYER_INDICES}",
                )
            if layer_index in seen:
                _issue(
                    issues,
                    "DUPLICATE_LAYER_INDEX",
                    layer_index,
                    f"layer_index {layer_index} appears more than once",
                    first_position=seen[layer_index],
                    duplicate_position=position,
                )
            else:
                seen[layer_index] = position
        else:
            _issue(
                issues,
                "INVALID_LAYER_INDEX",
                None,
                f"layers[{position}].layer_index must be an integer",
            )

        mesh, material = _parse_mesh_and_material(raw_layer, layer_index, issues)
        if layer_index is None:
            continue

        gap = raw_layer.get("air_gap_to_prev_mm") if isinstance(raw_layer, dict) else None
        if not _is_number(gap) or float(gap) < 0.0:
            _issue(
                issues,
                "INVALID_AIR_GAP",
                layer_index,
                "air_gap_to_prev_mm must be finite and non-negative",
            )
            gap_value = 0.0
        else:
            gap_value = float(gap)
        if layer_index == 0 and gap_value != 0.0:
            _issue(
                issues,
                "INVALID_AIR_GAP",
                0,
                "the innermost layer must declare air_gap_to_prev_mm=0",
            )
        parsed_by_index[layer_index] = (mesh, material, gap_value)

    for expected in REQUIRED_LAYER_INDICES:
        if expected not in seen:
            _issue(
                issues,
                "MISSING_LAYER_INDEX",
                expected,
                f"required layer_index {expected} is missing",
            )

    if issues:
        raise SceneValidationError(issues)

    layers: list[Layer] = []
    cumulative_z = float(base_z)
    for expected in REQUIRED_LAYER_INDICES:
        mesh, material, gap = parsed_by_index[expected]
        if expected > 0:
            cumulative_z += gap
        if mesh is None:
            _issue(
                issues,
                "INVALID_MESH",
                expected,
                "layer mesh could not be constructed",
            )
            continue
        if not material:
            continue
        layers.append(
            Layer(
                layer_index=expected,
                name=material["name"],
                air_gap_to_prev_mm=gap,
                transmittance=material["transmittance"],
                scatter_color=material["scatter_color"],
                texture_seed=material["texture_seed"],
                shadow_samples_per_axis=material["shadow_samples_per_axis"],
                projection_gain=material["projection_gain"],
                world_z_mm=cumulative_z,
                mesh=mesh,
            )
        )

    if len(layers) == 5:
        _check_position_rules(issues, camera, light, layers)
    if issues:
        raise SceneValidationError(issues)

    normalized = copy.deepcopy(source)
    normalized["layers"] = sorted(normalized["layers"], key=lambda item: item["layer_index"])
    return Scene(
        source=normalized,
        digest=canonical_digest(normalized),
        base_z_mm=float(base_z),
        layers=tuple(sorted(layers, key=lambda layer: layer.layer_index)),
        camera=camera,
        light=light,
        texture=texture,
    )


def load_scene(path: str | Path) -> Scene:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            source = json.load(handle)
    except json.JSONDecodeError as exc:
        raise SceneValidationError(
            [
                ValidationIssue(
                    "INVALID_JSON",
                    None,
                    f"scene JSON is invalid at line {exc.lineno}, column {exc.colno}",
                )
            ]
        ) from exc
    except OSError as exc:
        raise SceneValidationError(
            [ValidationIssue("SCENE_READ_ERROR", None, str(exc))]
        ) from exc
    return validate_scene(source)


def _paper_mesh(
    layer_index: int,
    width_mm: float,
    height_mm: float,
    columns: int = 8,
    rows: int = 6,
) -> dict[str, Any]:
    vertices: list[list[float]] = []
    uvs: list[list[float]] = []
    for row in range(rows + 1):
        v = row / rows
        y = -height_mm / 2 + v * height_mm
        for column in range(columns + 1):
            u = column / columns
            x = -width_mm / 2 + u * width_mm
            vertices.append([x, y, 0.0])
            uvs.append([u, v])
    triangles: list[list[int]] = []
    stride = columns + 1
    for row in range(rows):
        for column in range(columns):
            lower_left = row * stride + column
            lower_right = lower_left + 1
            upper_left = lower_left + stride
            upper_right = upper_left + 1
            triangles.append([lower_left, lower_right, upper_right])
            triangles.append([lower_left, upper_right, upper_left])
    return {"vertices": vertices, "uvs": uvs, "triangles": triangles}


def generate_scene(
    *,
    width: int = 320,
    height: int = 256,
    block_size: tuple[int, int] = (64, 64),
    samples_per_pixel: int = 2,
) -> dict[str, Any]:
    """Return the deterministic five-layer sample scene."""

    colors = [
        ("red", [0.82, 0.075, 0.045]),
        ("orange", [0.98, 0.36, 0.045]),
        ("yellow", [0.98, 0.76, 0.10]),
        ("green", [0.16, 0.72, 0.24]),
        ("blue", [0.08, 0.30, 0.88]),
    ]
    gaps = [0.0, 4.0, 6.0, 8.0, 10.0]
    transmittances = [0.38, 0.45, 0.52, 0.60, 0.68]
    densities = [1, 5, 6, 7, 8]
    layers: list[dict[str, Any]] = []
    for index, (name, color) in enumerate(colors):
        layers.append(
            {
                "layer_index": index,
                "name": name,
                "air_gap_to_prev_mm": gaps[index],
                "transmittance": transmittances[index],
                "scatter_color": color,
                "texture_seed": 41017 + index * 7919,
                "shadow_samples_per_axis": densities[index],
                "projection_gain": 1.0 + index * 0.08,
                "mesh": _paper_mesh(index, 140.0, 96.0),
            }
        )
    return {
        "format": SCENE_FORMAT,
        "stack": {"base_z_mm": 24.0, "layer_count": 5},
        "camera": {
            "position_mm": [15.0, -11.0, 190.0],
            "target_mm": [0.0, 0.0, 38.0],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 47.0,
            "resolution": [width, height],
            "block_size": [block_size[0], block_size[1]],
            "samples_per_pixel": samples_per_pixel,
        },
        "light": {
            "position_mm": [-20.0, 14.0, 10.0],
            "intensity": 1.15,
            "color": [1.0, 0.79, 0.53],
            "reference_distance_mm": 100.0,
        },
        "texture": {
            "width": 256,
            "height": 256,
            "projection_width": 192,
            "projection_height": 192,
            "fiber_contrast": 0.34,
            "hole_coverage": 0.13,
            "penumbra_base_mm": 0.45,
            "penumbra_per_gap_mm": 0.16,
        },
        "layers": layers,
    }


def write_scene(path: str | Path, scene: dict[str, Any] | None = None) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(
        scene if scene is not None else generate_scene(),
        ensure_ascii=False,
        indent=2,
        sort_keys=False,
    )
    path.write_text(payload + "\n", encoding="utf-8")


def layer_bounds_world(layer: Layer) -> tuple[float, float, float, float]:
    xmin, xmax, ymin, ymax = layer.mesh.bounds
    return xmin, xmax, ymin, ymax


def project_point(
    light_position: np.ndarray,
    source_point: np.ndarray,
    target_z_mm: float,
) -> np.ndarray | None:
    dz = float(source_point[2] - light_position[2])
    if abs(dz) <= 1e-12:
        return None
    t = (target_z_mm - float(light_position[2])) / dz
    if t <= 0.0:
        return None
    return light_position + t * (source_point - light_position)
