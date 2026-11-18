"""Scene loading, schema completion, and deterministic validation.

The validator collects all independent issues instead of failing on the first
problem.  Every geometry/material issue carries the business layer number and
layer id so a pipeline can route the error back to the source layer.
"""

from __future__ import annotations

import copy
import hashlib
import json
import math
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import SceneValidationError, ValidationIssue

SCENE_SCHEMA = "veil-scene/1"


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def scene_digest(scene: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(scene).encode("utf-8")).hexdigest()


def load_scene(path: str | Path) -> tuple[dict[str, Any], str]:
    scene_path = Path(path)
    try:
        with scene_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SceneValidationError(
            [
                ValidationIssue(
                    code="E_SCENE_JSON",
                    message=f"cannot read scene JSON: {exc}",
                    path=str(scene_path),
                )
            ]
        ) from exc
    scene = validate_scene(raw)
    return scene, scene_digest(scene)


class _Collector:
    def __init__(self) -> None:
        self.issues: list[ValidationIssue] = []

    def add(
        self,
        code: str,
        message: str,
        *,
        layer: int | None = None,
        layer_id: str | None = None,
        path: str | None = None,
    ) -> None:
        self.issues.append(
            ValidationIssue(
                code=code,
                message=message,
                layer=layer,
                layer_id=layer_id,
                path=path,
            )
        )


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _as_vec(value: Any, size: int) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != size or not all(_is_number(v) for v in value):
        return None
    return np.asarray(value, dtype=np.float64)


def _normalized_id(value: Any) -> str | None:
    if isinstance(value, str):
        stripped = value.strip()
        return stripped or None
    if _is_int(value) and value >= 0:
        return str(value)
    return None


def _read_layer_number(layer: dict[str, Any], display_id: str) -> int | None:
    value = layer.get("layer")
    if not _is_int(value) or value < 1:
        return None
    return value


def _triangle_area2(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> float:
    return float(np.linalg.norm(np.cross(b - a, c - a)))


def _point_in_triangle_3d(
    point: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
    c: np.ndarray,
    *,
    plane_tolerance: float,
    bary_tolerance: float,
) -> bool:
    normal = np.cross(b - a, c - a)
    normal_len = float(np.linalg.norm(normal))
    if normal_len <= 1e-14:
        return False
    plane_distance = abs(float(np.dot(point - a, normal))) / normal_len
    if plane_distance > plane_tolerance:
        return False

    ab = b - a
    ac = c - a
    ap = point - a
    d00 = float(np.dot(ab, ab))
    d01 = float(np.dot(ab, ac))
    d11 = float(np.dot(ac, ac))
    d20 = float(np.dot(ap, ab))
    d21 = float(np.dot(ap, ac))
    denominator = d00 * d11 - d01 * d01
    if abs(denominator) <= 1e-18:
        return False
    v = (d11 * d20 - d01 * d21) / denominator
    w = (d00 * d21 - d01 * d20) / denominator
    u = 1.0 - v - w
    return bool(u >= -bary_tolerance and v >= -bary_tolerance and w >= -bary_tolerance)


def point_inside_layer(
    point: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
    *,
    plane_tolerance: float = 1e-7,
    bary_tolerance: float = 1e-9,
) -> bool:
    for index in triangles:
        if _point_in_triangle_3d(
            point,
            vertices[int(index[0])],
            vertices[int(index[1])],
            vertices[int(index[2])],
            plane_tolerance=plane_tolerance,
            bary_tolerance=bary_tolerance,
        ):
            return True
    return False


def _complete_camera(camera: dict[str, Any]) -> None:
    camera.setdefault("samples", 1)


def _complete_global(settings: dict[str, Any]) -> None:
    settings.setdefault("ambient", 0.035)
    settings.setdefault("light_falloff", 0.08)
    settings.setdefault("scatter_strength", 0.62)
    settings.setdefault("shadow_strength", 0.86)


def _complete_projection(projection: dict[str, Any]) -> None:
    projection.setdefault("samples_per_axis", 3)
    projection.setdefault("landing_samples_per_axis", 3)
    projection.setdefault("penumbra_base", 0.012)
    projection.setdefault("penumbra_distance_gain", 0.32)
    projection.setdefault("penumbra_source_gain", 0.09)
    projection.setdefault("max_penumbra", 0.16)


def _complete_layer(layer: dict[str, Any]) -> None:
    layer.setdefault("thickness", 0.006)
    if layer.get("kind") == "gauze":
        layer.setdefault("resolution", [64, 64])
        layer.setdefault("texture_repeat", 7)
    elif layer.get("kind") == "lattice":
        pattern = layer.get("pattern")
        if not isinstance(pattern, dict):
            pattern = {}
            layer["pattern"] = pattern
        pattern.setdefault("style", "grid")
        raw_width = layer.get("width")
        default_pitch = (
            max(float(raw_width) * 8.0, 0.06) if _is_number(raw_width) else 0.16
        )
        pattern.setdefault("pitch", default_pitch)
        pattern.setdefault("edge_softness", 0.0015)
        layer.setdefault("metal_color", [0.72, 0.64, 0.48])
        layer.setdefault("pattern_seed", 0)


def validate_scene(raw: Any) -> dict[str, Any]:
    """Return a completed deep copy, or raise all validation issues at once."""

    collector = _Collector()
    if not isinstance(raw, dict):
        raise SceneValidationError(
            [ValidationIssue(code="E_SCENE_ROOT", message="scene root must be a JSON object", path="$")]
        )

    scene = copy.deepcopy(raw)
    if scene.get("schema_version") != SCENE_SCHEMA:
        collector.add(
            "E_SCENE_SCHEMA",
            f"schema_version must be {SCENE_SCHEMA!r}",
            path="schema_version",
        )

    image = scene.get("image")
    if not isinstance(image, dict):
        collector.add("E_IMAGE_INVALID", "image must be an object", path="image")
        image = {}
    for key in ("width", "height", "tile_width", "tile_height"):
        value = image.get(key)
        if not _is_int(value) or value <= 0:
            collector.add("E_IMAGE_INVALID", f"image.{key} must be a positive integer", path=f"image.{key}")
    background = _as_vec(image.get("background_rgba"), 4)
    if background is None or np.any(background < 0.0) or np.any(background > 1.0):
        collector.add(
            "E_IMAGE_INVALID",
            "image.background_rgba must contain four finite values in [0,1]",
            path="image.background_rgba",
        )

    camera = scene.get("camera")
    if not isinstance(camera, dict):
        collector.add("E_CAMERA_INVALID", "camera must be an object", path="camera")
        camera = {}
    else:
        _complete_camera(camera)
    position = _as_vec(camera.get("position"), 3)
    target = _as_vec(camera.get("target"), 3)
    up = _as_vec(camera.get("up"), 3)
    fov = camera.get("fov_y_deg")
    near = camera.get("near")
    far = camera.get("far")
    samples = camera.get("samples")
    if position is None:
        collector.add("E_CAMERA_INVALID", "camera.position must be three finite numbers", path="camera.position")
    if target is None:
        collector.add("E_CAMERA_INVALID", "camera.target must be three finite numbers", path="camera.target")
    if up is None or (up is not None and float(np.linalg.norm(up)) <= 1e-12):
        collector.add("E_CAMERA_INVALID", "camera.up must be a nonzero finite vector", path="camera.up")
    if not _is_number(fov) or not 0.0 < float(fov) < 179.0:
        collector.add("E_CAMERA_INVALID", "camera.fov_y_deg must be in (0,179)", path="camera.fov_y_deg")
    if not _is_number(near) or float(near) <= 0.0:
        collector.add("E_CAMERA_INVALID", "camera.near must be positive", path="camera.near")
    if not _is_number(far) or not _is_number(near) or float(far) <= float(near):
        collector.add("E_CAMERA_INVALID", "camera.far must be greater than camera.near", path="camera.far")
    if not _is_int(samples) or not 1 <= int(samples) <= 16:
        collector.add("E_CAMERA_INVALID", "camera.samples must be an integer in [1,16]", path="camera.samples")
    if position is not None and target is not None and up is not None:
        forward = target - position
        if float(np.linalg.norm(forward)) <= 1e-9:
            collector.add("E_CAMERA_INVALID", "camera position and target must differ", path="camera")
        else:
            normalized_forward = forward / np.linalg.norm(forward)
            normalized_up = up / np.linalg.norm(up)
            if abs(float(np.dot(normalized_forward, normalized_up))) >= 1.0 - 1e-7:
                collector.add("E_CAMERA_INVALID", "camera.up must not be parallel to the view direction", path="camera.up")

    ground = scene.get("ground")
    if not isinstance(ground, dict):
        collector.add("E_GROUND_INVALID", "ground must be an object", path="ground")
        ground = {}
    ground_y = ground.get("y")
    if not _is_number(ground_y):
        collector.add("E_GROUND_INVALID", "ground.y must be finite", path="ground.y")
    for key in ("x", "z"):
        bounds = ground.get(key)
        if (
            not isinstance(bounds, list)
            or len(bounds) != 2
            or not all(_is_number(v) for v in bounds)
            or float(bounds[0]) >= float(bounds[1])
        ):
            collector.add("E_GROUND_INVALID", f"ground.{key} must be [min,max]", path=f"ground.{key}")
    ground_color = _as_vec(ground.get("color"), 3)
    if ground_color is None or np.any(ground_color < 0.0) or np.any(ground_color > 1.0):
        collector.add("E_GROUND_INVALID", "ground.color must contain three values in [0,1]", path="ground.color")
    if not _is_number(ground.get("reflectance")) or not 0.0 <= float(ground.get("reflectance", -1)) <= 1.0:
        collector.add("E_GROUND_INVALID", "ground.reflectance must be in [0,1]", path="ground.reflectance")

    settings = scene.get("global")
    if settings is None:
        settings = {}
        scene["global"] = settings
    elif not isinstance(settings, dict):
        collector.add("E_GLOBAL_INVALID", "global must be an object", path="global")
        settings = {}
        scene["global"] = settings
    _complete_global(settings)
    for key in ("ambient", "light_falloff", "scatter_strength", "shadow_strength"):
        value = settings.get(key)
        if not _is_number(value) or float(value) < 0.0:
            collector.add("E_GLOBAL_INVALID", f"global.{key} must be a nonnegative finite number", path=f"global.{key}")

    projection = scene.get("projection")
    if projection is None:
        projection = {}
        scene["projection"] = projection
    elif not isinstance(projection, dict):
        collector.add("E_PROJECTION_INVALID", "projection must be an object", path="projection")
        projection = {}
        scene["projection"] = projection
    _complete_projection(projection)
    for key in ("samples_per_axis", "landing_samples_per_axis"):
        value = projection.get(key)
        if not _is_int(value) or not 1 <= int(value) <= 64:
            collector.add("E_PROJECTION_INVALID", f"projection.{key} must be an integer in [1,64]", path=f"projection.{key}")
    for key in ("penumbra_base", "penumbra_distance_gain", "penumbra_source_gain", "max_penumbra"):
        value = projection.get(key)
        if not _is_number(value) or float(value) < 0.0:
            collector.add(
                "E_PROJECTION_INVALID",
                f"projection.{key} must be a nonnegative finite number",
                path=f"projection.{key}",
            )
    if _is_number(projection.get("max_penumbra")) and float(projection["max_penumbra"]) <= 0.0:
        collector.add("E_PROJECTION_INVALID", "projection.max_penumbra must be positive", path="projection.max_penumbra")

    lights = scene.get("lights")
    if not isinstance(lights, list) or not lights:
        collector.add("E_LIGHT_INVALID", "lights must be a nonempty list", path="lights")
        lights = []
    light_ids: set[str] = set()
    for light_index, light in enumerate(lights):
        path = f"lights[{light_index}]"
        if not isinstance(light, dict):
            collector.add("E_LIGHT_INVALID", "light must be an object", path=path)
            continue
        light_id = _normalized_id(light.get("id"))
        if light_id is None:
            collector.add("E_LIGHT_INVALID", "light.id must be a nonempty string or nonnegative integer", path=f"{path}.id")
        elif light_id in light_ids:
            collector.add("E_LIGHT_DUPLICATE_ID", f"duplicate light id {light_id!r}", path=f"{path}.id")
        else:
            light_ids.add(light_id)
            light["id"] = light_id
        if _as_vec(light.get("position"), 3) is None:
            collector.add("E_LIGHT_INVALID", "light.position must be three finite numbers", path=f"{path}.position")
        if not _is_number(light.get("intensity")) or float(light.get("intensity", -1)) < 0.0:
            collector.add("E_LIGHT_INVALID", "light.intensity must be nonnegative", path=f"{path}.intensity")
        color = _as_vec(light.get("color"), 3)
        if color is None or np.any(color < 0.0) or np.any(color > 1.0):
            collector.add("E_LIGHT_INVALID", "light.color must contain three values in [0,1]", path=f"{path}.color")

    layers = scene.get("layers")
    if not isinstance(layers, list) or not layers:
        collector.add("E_LAYER_INVALID", "layers must be a nonempty list", path="layers")
        layers = []

    seen_numbers: dict[int, int] = {}
    seen_ids: dict[str, int] = {}
    parsed_meshes: list[tuple[int | None, str, np.ndarray, np.ndarray]] = []
    for layer_index, layer in enumerate(layers):
        path = f"layers[{layer_index}]"
        if not isinstance(layer, dict):
            collector.add("E_LAYER_INVALID", "layer must be an object", path=path)
            continue
        _complete_layer(layer)
        raw_layer_id = layer.get("id")
        display_id = _normalized_id(raw_layer_id) or str(raw_layer_id or "")
        number = _read_layer_number(layer, display_id)
        if number is None:
            collector.add(
                "E_LAYER_ORDER_INVALID",
                "layer.layer must be a positive integer",
                layer_id=display_id or None,
                path=f"{path}.layer",
            )
        elif number in seen_numbers:
            collector.add(
                "E_LAYER_ORDER_DUPLICATE",
                f"duplicate layer number {number}; first seen at layers[{seen_numbers[number]}]",
                layer=number,
                layer_id=display_id or None,
                path=f"{path}.layer",
            )
        else:
            seen_numbers[number] = layer_index

        layer_id = _normalized_id(layer.get("id"))
        if layer_id is None:
            collector.add(
                "E_LAYER_ID_INVALID",
                "layer.id must be a nonempty string or nonnegative integer",
                layer=number,
                layer_id=display_id or None,
                path=f"{path}.id",
            )
        elif layer_id in seen_ids:
            collector.add(
                "E_LAYER_ID_DUPLICATE",
                f"duplicate layer id {layer_id!r}; first seen at layers[{seen_ids[layer_id]}]",
                layer=number,
                layer_id=layer_id,
                path=f"{path}.id",
            )
        else:
            seen_ids[layer_id] = layer_index
            layer["id"] = layer_id

        kind = layer.get("kind")
        if kind not in ("gauze", "lattice"):
            collector.add(
                "E_LAYER_KIND",
                "layer.kind must be 'gauze' or 'lattice'",
                layer=number,
                layer_id=display_id or None,
                path=f"{path}.kind",
            )

        mesh = layer.get("mesh")
        vertices: np.ndarray | None = None
        triangles: np.ndarray | None = None
        if not isinstance(mesh, dict):
            collector.add(
                "E_MESH_INVALID",
                "layer.mesh must be an object",
                layer=number,
                layer_id=display_id or None,
                path=f"{path}.mesh",
            )
        else:
            raw_vertices = mesh.get("vertices")
            raw_triangles = mesh.get("triangles")
            if not isinstance(raw_vertices, list) or len(raw_vertices) < 3:
                collector.add(
                    "E_MESH_INVALID",
                    "mesh.vertices must contain at least three vertices",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.mesh.vertices",
                )
            else:
                strict_vertices = all(
                    isinstance(vertex, list)
                    and len(vertex) == 3
                    and all(_is_number(value) for value in vertex)
                    for vertex in raw_vertices
                )
                if strict_vertices:
                    vertices = np.asarray(raw_vertices, dtype=np.float64)
                if not strict_vertices:
                    collector.add(
                        "E_MESH_INVALID",
                        "mesh.vertices must be an Nx3 array of finite numbers",
                        layer=number,
                        layer_id=display_id or None,
                        path=f"{path}.mesh.vertices",
                    )
                    vertices = None

            if not isinstance(raw_triangles, list) or not raw_triangles:
                collector.add(
                    "E_MESH_INVALID",
                    "mesh.triangles must be a nonempty list",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.mesh.triangles",
                )
            else:
                valid_indices = True
                for tri_index, triangle in enumerate(raw_triangles):
                    if (
                        not isinstance(triangle, list)
                        or len(triangle) != 3
                        or not all(_is_int(v) for v in triangle)
                    ):
                        collector.add(
                            "E_MESH_INVALID",
                            "each triangle must contain exactly three integer indices",
                            layer=number,
                            layer_id=display_id or None,
                            path=f"{path}.mesh.triangles[{tri_index}]",
                        )
                        valid_indices = False
                if valid_indices:
                    triangles = np.asarray(raw_triangles, dtype=np.int64)
                    vertex_count = int(vertices.shape[0]) if vertices is not None else len(raw_vertices)
                    if np.any(triangles < 0) or np.any(triangles >= vertex_count):
                        collector.add(
                            "E_MESH_INDEX_RANGE",
                            f"triangle index outside vertex range [0,{vertex_count - 1}]",
                            layer=number,
                            layer_id=display_id or None,
                            path=f"{path}.mesh.triangles",
                        )
                    if vertices is not None:
                        for tri_index, triangle in enumerate(triangles):
                            if np.any(triangle < 0) or np.any(triangle >= vertices.shape[0]):
                                continue
                            area2 = _triangle_area2(
                                vertices[int(triangle[0])],
                                vertices[int(triangle[1])],
                                vertices[int(triangle[2])],
                            )
                            if not math.isfinite(area2) or area2 <= 1e-10:
                                collector.add(
                                    "E_TRIANGLE_DEGENERATE",
                                    "triangle area is zero or below tolerance",
                                    layer=number,
                                    layer_id=display_id or None,
                                    path=f"{path}.mesh.triangles[{tri_index}]",
                                )
        if (
            vertices is not None
            and triangles is not None
            and triangles.size > 0
            and np.all(triangles >= 0)
            and np.all(triangles < vertices.shape[0])
        ):
            parsed_meshes.append((number, display_id, vertices, triangles))

        thickness = layer.get("thickness")
        if not _is_number(thickness) or float(thickness) <= 0.0:
            collector.add(
                "E_LAYER_THICKNESS",
                "layer.thickness must be positive",
                layer=number,
                layer_id=display_id or None,
                path=f"{path}.thickness",
            )

        if kind == "gauze":
            transmittance = layer.get("transmittance")
            if not _is_number(transmittance) or not 0.0 <= float(transmittance) <= 1.0:
                collector.add(
                    "E_TRANSMITTANCE_RANGE",
                    "gauze transmittance must be in [0,1]",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.transmittance",
                )
            tint = _as_vec(layer.get("tint"), 3)
            if tint is None or np.any(tint < 0.0) or np.any(tint > 1.0):
                collector.add(
                    "E_GAUZE_TINT",
                    "gauze tint must contain three values in [0,1]",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.tint",
                )
            seed = layer.get("weave_seed")
            if not _is_int(seed) or seed < 0:
                collector.add(
                    "E_WEAVE_SEED",
                    "gauze weave_seed must be a nonnegative integer",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.weave_seed",
                )
            resolution = layer.get("resolution")
            if (
                not isinstance(resolution, list)
                or len(resolution) != 2
                or not all(_is_int(v) and 1 <= v <= 4096 for v in resolution)
            ):
                collector.add(
                    "E_TEXTURE_RESOLUTION",
                    "gauze resolution must be [width,height] integers in [1,4096]",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.resolution",
                )
            repeat = layer.get("texture_repeat")
            if not _is_number(repeat) or float(repeat) <= 0.0:
                collector.add(
                    "E_TEXTURE_REPEAT",
                    "gauze texture_repeat must be positive",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.texture_repeat",
                )
        elif kind == "lattice":
            reflectance = layer.get("reflectance")
            if not _is_number(reflectance) or not 0.0 <= float(reflectance) <= 1.0:
                collector.add(
                    "E_REFLECTANCE_RANGE",
                    "lattice reflectance must be in [0,1]",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.reflectance",
                )
            width = layer.get("width")
            if not _is_number(width) or float(width) <= 0.0:
                collector.add(
                    "E_LATTICE_WIDTH",
                    "lattice width must be positive",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.width",
                )
            pattern = layer.get("pattern")
            if not isinstance(pattern, dict) or pattern.get("style") not in ("grid", "diamond"):
                collector.add(
                    "E_LATTICE_PATTERN",
                    "lattice pattern.style must be 'grid' or 'diamond'",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.pattern.style",
                )
            elif (
                not _is_number(pattern.get("pitch"))
                or float(pattern.get("pitch", -1)) <= 0.0
                or (_is_number(width) and float(pattern["pitch"]) <= float(width))
            ):
                collector.add(
                    "E_LATTICE_PATTERN",
                    "lattice pattern.pitch must be positive and greater than width",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.pattern.pitch",
                )
            if not _is_number(pattern.get("edge_softness")) or float(pattern.get("edge_softness", -1)) < 0.0:
                collector.add(
                    "E_LATTICE_PATTERN",
                    "lattice pattern.edge_softness must be nonnegative",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.pattern.edge_softness",
                )
            metal_color = _as_vec(layer.get("metal_color"), 3)
            if metal_color is None or np.any(metal_color < 0.0) or np.any(metal_color > 1.0):
                collector.add(
                    "E_LATTICE_METAL_COLOR",
                    "lattice metal_color must contain three values in [0,1]",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.metal_color",
                )
            if not _is_int(layer.get("pattern_seed")) or int(layer.get("pattern_seed", -1)) < 0:
                collector.add(
                    "E_LATTICE_PATTERN",
                    "lattice pattern_seed must be a nonnegative integer",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"{path}.pattern_seed",
                )

    for light_index, light in enumerate(lights):
        if not isinstance(light, dict):
            continue
        position = _as_vec(light.get("position"), 3)
        if position is None:
            continue
        light_id = str(light.get("id", ""))
        for number, display_id, vertices, triangles in parsed_meshes:
            if point_inside_layer(position, vertices, triangles):
                collector.add(
                    "E_LIGHT_INSIDE_LAYER",
                    f"light {light_id!r} lies on layer geometry",
                    layer=number,
                    layer_id=display_id or None,
                    path=f"lights[{light_index}].position",
                )

    if collector.issues:
        raise SceneValidationError(collector.issues)

    layers.sort(key=lambda layer: int(layer["layer"]))
    return scene


def iter_layers(scene: dict[str, Any]) -> Iterable[dict[str, Any]]:
    return iter(scene["layers"])

