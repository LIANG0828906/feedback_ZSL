from __future__ import annotations

from collections import Counter
from dataclasses import dataclass, field
import math
from typing import Any

import numpy as np

from .scene import canonical_scene, scene_digest
from .util import finite_float, is_int, vec3


@dataclass
class ValidationIssue:
    code: str
    message: str
    layer_index: int | None = None
    fold_id: int | None = None
    light_id: str | None = None
    tassel_id: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "layer_index": self.layer_index,
            "fold_id": self.fold_id,
            "light_id": self.light_id,
            "tassel_id": self.tassel_id,
            "message": self.message,
        }


@dataclass(frozen=True)
class CameraData:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_degrees: float
    near: float
    far: float


@dataclass(frozen=True)
class WallData:
    origin: np.ndarray
    normal: np.ndarray
    u_axis: np.ndarray
    v_axis: np.ndarray
    half_u: float
    half_v: float
    base_color: np.ndarray


@dataclass(frozen=True)
class TriangleMesh:
    layer_index: int
    fold_id: int
    vertices: np.ndarray
    triangles: np.ndarray
    uvs: np.ndarray


@dataclass(frozen=True)
class LayerData:
    layer_index: int
    thickness: float
    transmittance: float
    scatter_color: np.ndarray
    pattern_seed: int
    folds: tuple[TriangleMesh, ...]


@dataclass(frozen=True)
class TasselSegment:
    tassel_id: str
    segment_index: int
    a: np.ndarray
    b: np.ndarray
    radius: float


@dataclass(frozen=True)
class TasselData:
    tassel_id: str
    segments: tuple[TasselSegment, ...]


@dataclass(frozen=True)
class LightData:
    light_id: str
    position: np.ndarray
    intensity: float
    radius: float
    color: np.ndarray
    cast_shadow: bool


@dataclass
class SceneData:
    raw: dict[str, Any]
    canonical: dict[str, Any]
    digest: str
    width: int
    height: int
    tile_width: int
    tile_height: int
    background_rgba: np.ndarray
    tone_map_exposure: float
    tone_map_white_cap: float
    owner_alpha_threshold: float
    camera: CameraData
    wall: WallData
    layers: tuple[LayerData, ...]
    tassels: tuple[TasselData, ...]
    lights: tuple[LightData, ...]
    shadows: dict[str, Any]
    shading: dict[str, Any]
    validation_epsilon: float
    lamp_surface_clearance: float


@dataclass
class ValidationResult:
    issues: list[ValidationIssue] = field(default_factory=list)
    data: SceneData | None = None

    @property
    def ok(self) -> bool:
        return not self.issues and self.data is not None

    def report(self) -> dict[str, Any]:
        return {
            "schema_version": "1.0",
            "status": "rejected" if self.issues else "valid",
            "business_code": "INVALID_SCENE" if self.issues else "OK",
            "issue_count": len(self.issues),
            "issues": [issue.as_dict() for issue in self.issues],
        }


def _add(
    issues: list[ValidationIssue],
    code: str,
    message: str,
    *,
    layer_index: int | None = None,
    fold_id: int | None = None,
    light_id: str | None = None,
    tassel_id: str | None = None,
) -> None:
    issues.append(
        ValidationIssue(
            code=code,
            message=message,
            layer_index=layer_index,
            fold_id=fold_id,
            light_id=light_id,
            tassel_id=tassel_id,
        )
    )


def _read_vec3(
    value: Any,
    issues: list[ValidationIssue],
    code: str,
    label: str,
    **identifiers: Any,
) -> np.ndarray | None:
    result = vec3(value)
    if result is None:
        _add(issues, code, f"{label} must be a finite three-component vector", **identifiers)
    return result


def _triangle_area(a: np.ndarray, b: np.ndarray, c: np.ndarray) -> float:
    return float(np.linalg.norm(np.cross(b - a, c - a)) * 0.5)


def _point_triangle_distance(
    point: np.ndarray,
    a: np.ndarray,
    b: np.ndarray,
    c: np.ndarray,
) -> float:
    ab = b - a
    ac = c - a
    ap = point - a
    d1 = float(np.dot(ab, ap))
    d2 = float(np.dot(ac, ap))
    if d1 <= 0.0 and d2 <= 0.0:
        return float(np.linalg.norm(ap))

    bp = point - b
    d3 = float(np.dot(ab, bp))
    d4 = float(np.dot(ac, bp))
    if d3 >= 0.0 and d4 <= d3:
        return float(np.linalg.norm(bp))

    vc = d1 * d4 - d3 * d2
    if vc <= 0.0 and d1 >= 0.0 and d3 <= 0.0:
        t = d1 / (d1 - d3)
        return float(np.linalg.norm(point - (a + t * ab)))

    cp = point - c
    d5 = float(np.dot(ab, cp))
    d6 = float(np.dot(ac, cp))
    if d6 >= 0.0 and d5 <= d6:
        return float(np.linalg.norm(cp))

    vb = d5 * d2 - d1 * d6
    if vb <= 0.0 and d2 >= 0.0 and d6 <= 0.0:
        t = d2 / (d2 - d6)
        return float(np.linalg.norm(point - (a + t * ac)))

    va = d3 * d6 - d5 * d4
    if va <= 0.0 and (d4 - d3) >= 0.0 and (d5 - d6) >= 0.0:
        t = (d4 - d3) / ((d4 - d3) + (d5 - d6))
        return float(np.linalg.norm(point - (b + t * (c - b))))

    denominator = 1.0 / (va + vb + vc)
    v = vb * denominator
    w = vc * denominator
    closest = a + ab * v + ac * w
    return float(np.linalg.norm(point - closest))


def _layer_is_closed(layer: LayerData) -> bool:
    edge_counts: Counter[tuple[int, int]] = Counter()
    offset = 0
    for fold in layer.folds:
        for triangle in fold.triangles:
            a, b, c = (int(index) + offset for index in triangle)
            for left, right in ((a, b), (b, c), (c, a)):
                edge_counts[(min(left, right), max(left, right))] += 1
        offset += len(fold.vertices)
    return bool(edge_counts) and all(count == 2 for count in edge_counts.values())


def _point_winding_number(layer: LayerData, point: np.ndarray) -> float:
    total = 0.0
    for fold in layer.folds:
        vertices = fold.vertices
        for triangle in fold.triangles:
            a = vertices[int(triangle[0])] - point
            b = vertices[int(triangle[1])] - point
            c = vertices[int(triangle[2])] - point
            la = float(np.linalg.norm(a))
            lb = float(np.linalg.norm(b))
            lc = float(np.linalg.norm(c))
            if la < 1e-14 or lb < 1e-14 or lc < 1e-14:
                continue
            determinant = float(np.dot(a, np.cross(b, c)))
            denominator = (
                la * lb * lc
                + float(np.dot(a, b)) * lc
                + float(np.dot(b, c)) * la
                + float(np.dot(c, a)) * lb
            )
            total += 2.0 * math.atan2(determinant, denominator)
    return total / (4.0 * math.pi)


def validate_scene(scene: Any) -> ValidationResult:
    issues: list[ValidationIssue] = []
    if not isinstance(scene, dict):
        _add(issues, "SCENE_NOT_OBJECT", "scene root must be a JSON object")
        return ValidationResult(issues=issues)

    if scene.get("schema_version") != "1.0":
        _add(
            issues,
            "UNSUPPORTED_SCHEMA",
            "schema_version must be '1.0'",
        )

    render = scene.get("render")
    width = height = tile_width = tile_height = 0
    background = None
    exposure = 0.92
    white_cap = 0.965
    owner_threshold = 0.025
    if not isinstance(render, dict):
        _add(issues, "RENDER_CONFIG_INVALID", "render must be an object")
    else:
        dimensions: dict[str, int] = {}
        for key in ("width", "height", "tile_width", "tile_height"):
            value = render.get(key)
            if not is_int(value) or value <= 0:
                _add(
                    issues,
                    "RENDER_DIMENSION_INVALID",
                    f"render.{key} must be a positive integer",
                )
                dimensions[key] = 0
            else:
                dimensions[key] = int(value)
        width = dimensions["width"]
        height = dimensions["height"]
        tile_width = dimensions["tile_width"]
        tile_height = dimensions["tile_height"]
        if width > 4096 or height > 4096:
            _add(
                issues,
                "RENDER_DIMENSION_UNSUPPORTED",
                "render dimensions may not exceed 4096",
            )
        bg = render.get("background_rgba")
        if (
            not isinstance(bg, list)
            or len(bg) != 4
            or not all(finite_float(v) and 0 <= float(v) <= 255 for v in bg)
        ):
            _add(
                issues,
                "BACKGROUND_INVALID",
                "render.background_rgba must contain four finite values in [0,255]",
            )
        else:
            background = np.asarray(bg, dtype=np.float64) / 255.0
        if finite_float(render.get("tone_map_exposure")) and float(
            render["tone_map_exposure"]
        ) > 0:
            exposure = float(render["tone_map_exposure"])
        else:
            _add(
                issues,
                "TONE_MAP_INVALID",
                "render.tone_map_exposure must be positive",
            )
        if finite_float(render.get("tone_map_white_cap")):
            white_cap = float(render["tone_map_white_cap"])
            if not 0.1 <= white_cap <= 1.0:
                _add(
                    issues,
                    "TONE_MAP_INVALID",
                    "render.tone_map_white_cap must be in [0.1, 1.0]",
                )
        else:
            _add(
                issues,
                "TONE_MAP_INVALID",
                "render.tone_map_white_cap must be finite",
            )
        if finite_float(render.get("owner_alpha_threshold")):
            owner_threshold = float(render["owner_alpha_threshold"])
            if not 0.0 <= owner_threshold <= 1.0:
                _add(
                    issues,
                    "OWNER_THRESHOLD_INVALID",
                    "render.owner_alpha_threshold must be in [0,1]",
                )
        else:
            _add(
                issues,
                "OWNER_THRESHOLD_INVALID",
                "render.owner_alpha_threshold must be finite",
            )

    camera_raw = scene.get("camera")
    camera: CameraData | None = None
    if not isinstance(camera_raw, dict):
        _add(issues, "CAMERA_INVALID", "camera must be an object")
    else:
        position = _read_vec3(
            camera_raw.get("position"), issues, "CAMERA_INVALID", "camera.position"
        )
        target = _read_vec3(
            camera_raw.get("target"), issues, "CAMERA_INVALID", "camera.target"
        )
        up = _read_vec3(camera_raw.get("up"), issues, "CAMERA_INVALID", "camera.up")
        fov = camera_raw.get("fov_y_degrees")
        near = camera_raw.get("near")
        far = camera_raw.get("far")
        if not finite_float(fov) or not 0.0 < float(fov) < 180.0:
            _add(
                issues,
                "CAMERA_INVALID",
                "camera.fov_y_degrees must be in (0,180)",
            )
        if not finite_float(near) or float(near) <= 0.0:
            _add(issues, "CAMERA_INVALID", "camera.near must be positive")
        if not finite_float(far) or not finite_float(near) or float(far) <= float(near):
            _add(issues, "CAMERA_INVALID", "camera.far must be greater than near")
        if position is not None and target is not None and up is not None:
            forward = target - position
            if float(np.linalg.norm(forward)) < 1e-8:
                _add(
                    issues,
                    "CAMERA_INVALID",
                    "camera position and target must differ",
                )
            elif float(np.linalg.norm(np.cross(forward, up))) < 1e-8:
                _add(
                    issues,
                    "CAMERA_INVALID",
                    "camera up must not be parallel to the view direction",
                )
            elif all(
                finite_float(v)
                for v in (fov, near, far)
            ):
                camera = CameraData(
                    position=position,
                    target=target,
                    up=up,
                    fov_y_degrees=float(fov),
                    near=float(near),
                    far=float(far),
                )

    wall_raw = scene.get("wall")
    wall: WallData | None = None
    if not isinstance(wall_raw, dict):
        _add(issues, "WALL_INVALID", "wall must be an object")
    else:
        origin = _read_vec3(
            wall_raw.get("origin"), issues, "WALL_INVALID", "wall.origin"
        )
        normal = _read_vec3(
            wall_raw.get("normal"), issues, "WALL_INVALID", "wall.normal"
        )
        u_axis = _read_vec3(
            wall_raw.get("u_axis"), issues, "WALL_INVALID", "wall.u_axis"
        )
        v_axis = _read_vec3(
            wall_raw.get("v_axis"), issues, "WALL_INVALID", "wall.v_axis"
        )
        half_u = wall_raw.get("half_u")
        half_v = wall_raw.get("half_v")
        base_color = _read_vec3(
            wall_raw.get("base_color"), issues, "WALL_INVALID", "wall.base_color"
        )
        if not finite_float(half_u) or float(half_u) <= 0.0:
            _add(issues, "WALL_INVALID", "wall.half_u must be positive")
        if not finite_float(half_v) or float(half_v) <= 0.0:
            _add(issues, "WALL_INVALID", "wall.half_v must be positive")
        if (
            base_color is not None
            and np.any(base_color < 0.0)
            or base_color is not None
            and np.any(base_color > 1.0)
        ):
            _add(issues, "WALL_INVALID", "wall.base_color components must be in [0,1]")
        vectors_ok = all(
            v is not None for v in (origin, normal, u_axis, v_axis, base_color)
        )
        if vectors_ok:
            n_len = float(np.linalg.norm(normal))
            u_len = float(np.linalg.norm(u_axis))
            v_len = float(np.linalg.norm(v_axis))
            if n_len < 1e-9 or u_len < 1e-9 or v_len < 1e-9:
                _add(issues, "WALL_INVALID", "wall basis vectors must be non-zero")
            else:
                normal = normal / n_len
                u_axis = u_axis / u_len
                v_axis = v_axis / v_len
                if (
                    abs(float(np.dot(normal, u_axis))) > 1e-5
                    or abs(float(np.dot(normal, v_axis))) > 1e-5
                    or abs(float(np.dot(u_axis, v_axis))) > 1e-5
                ):
                    _add(
                        issues,
                        "WALL_INVALID",
                        "wall basis vectors must be mutually orthogonal",
                    )
                elif finite_float(half_u) and finite_float(half_v):
                    wall = WallData(
                        origin=origin,
                        normal=normal,
                        u_axis=u_axis,
                        v_axis=v_axis,
                        half_u=float(half_u),
                        half_v=float(half_v),
                        base_color=base_color,
                    )

    shading_raw = scene.get("fabric_shading")
    shading: dict[str, Any] = {
        "fold_normal_strength": 0.34,
        "min_cosine": 0.22,
        "alpha_floor": 0.34,
        "alpha_transmission_weight": 0.75,
        "ambient_strength": 0.16,
        "transmission_reference_thickness": 0.03,
    }
    if not isinstance(shading_raw, dict):
        _add(issues, "FABRIC_SHADING_INVALID", "fabric_shading must be an object")
    else:
        for key, low, high in (
            ("fold_normal_strength", 0.0, 2.0),
            ("min_cosine", 0.02, 1.0),
            ("alpha_floor", 0.01, 0.99),
            ("alpha_transmission_weight", 0.0, 1.0),
            ("ambient_strength", 0.0, 2.0),
            ("transmission_reference_thickness", 1e-5, 10.0),
        ):
            value = shading_raw.get(key)
            if not finite_float(value) or not low <= float(value) <= high:
                _add(
                    issues,
                    "FABRIC_SHADING_INVALID",
                    f"fabric_shading.{key} must be in [{low},{high}]",
                )
            else:
                shading[key] = float(value)

    layers_raw = scene.get("layers")
    layers: list[LayerData] = []
    seen_layer_indices: set[int] = set()
    seen_fold_ids: set[int] = set()
    if not isinstance(layers_raw, list) or not layers_raw:
        _add(issues, "LAYERS_INVALID", "layers must be a non-empty array")
        layers_raw = []

    for layer_raw in layers_raw:
        if not isinstance(layer_raw, dict):
            _add(issues, "LAYER_INVALID", "each layer must be an object")
            continue
        layer_index_raw = layer_raw.get("layer_index")
        layer_index: int | None = None
        if not is_int(layer_index_raw):
            _add(
                issues,
                "LAYER_INDEX_INVALID",
                "layer_index must be an integer",
            )
        else:
            layer_index = int(layer_index_raw)
            if layer_index in seen_layer_indices:
                _add(
                    issues,
                    "DUPLICATE_LAYER_INDEX",
                    f"layer_index {layer_index} is duplicated",
                    layer_index=layer_index,
                )
            if not 1 <= layer_index <= 65535:
                _add(
                    issues,
                    "LAYER_INDEX_INVALID",
                    "layer_index must be in [1,65535] for uint16 ownership",
                    layer_index=layer_index,
                )
            seen_layer_indices.add(layer_index)

        thickness = layer_raw.get("thickness")
        if not finite_float(thickness) or float(thickness) <= 0.0:
            _add(
                issues,
                "THICKNESS_INVALID",
                "thickness must be positive",
                layer_index=layer_index,
            )
        transmittance = layer_raw.get("transmittance")
        if (
            not finite_float(transmittance)
            or not 0.0 <= float(transmittance) <= 1.0
        ):
            _add(
                issues,
                "TRANSMITTANCE_OUT_OF_RANGE",
                "transmittance must be in [0,1]",
                layer_index=layer_index,
            )
        scatter = _read_vec3(
            layer_raw.get("scatter_color"),
            issues,
            "SCATTER_COLOR_INVALID",
            "scatter_color",
            layer_index=layer_index,
        )
        if scatter is not None and (np.any(scatter < 0.0) or np.any(scatter > 1.0)):
            _add(
                issues,
                "SCATTER_COLOR_INVALID",
                "scatter_color components must be in [0,1]",
                layer_index=layer_index,
            )
            scatter = None
        seed = layer_raw.get("pattern_seed")
        if not is_int(seed) or seed < 0:
            _add(
                issues,
                "PATTERN_SEED_INVALID",
                "pattern_seed must be a non-negative integer",
                layer_index=layer_index,
            )

        folds_raw = layer_raw.get("folds")
        folds: list[TriangleMesh] = []
        if not isinstance(folds_raw, list) or not folds_raw:
            _add(
                issues,
                "FOLDS_INVALID",
                "folds must be a non-empty array",
                layer_index=layer_index,
            )
            folds_raw = []
        for fold_raw in folds_raw:
            fold_layer_index = layer_index
            fold_id: int | None = None
            if not isinstance(fold_raw, dict):
                _add(
                    issues,
                    "FOLD_INVALID",
                    "each fold must be an object",
                    layer_index=fold_layer_index,
                )
                continue
            fold_id_raw = fold_raw.get("fold_id")
            if not is_int(fold_id_raw):
                _add(
                    issues,
                    "FOLD_ID_INVALID",
                    "fold_id must be an integer",
                    layer_index=fold_layer_index,
                )
            else:
                fold_id = int(fold_id_raw)
                if fold_id in seen_fold_ids:
                    _add(
                        issues,
                        "DUPLICATE_FOLD_ID",
                        f"fold_id {fold_id} is duplicated",
                        layer_index=fold_layer_index,
                        fold_id=fold_id,
                    )
                seen_fold_ids.add(fold_id)

            vertices_raw = fold_raw.get("vertices")
            vertices: np.ndarray | None = None
            if not isinstance(vertices_raw, list) or len(vertices_raw) < 3:
                _add(
                    issues,
                    "VERTICES_INVALID",
                    "vertices must contain at least three points",
                    layer_index=fold_layer_index,
                    fold_id=fold_id,
                )
            else:
                parsed_vertices = [
                    vec3(vertex) for vertex in vertices_raw
                ]
                if any(vertex is None for vertex in parsed_vertices):
                    _add(
                        issues,
                        "VERTICES_INVALID",
                        "every vertex must be a finite three-component vector",
                        layer_index=fold_layer_index,
                        fold_id=fold_id,
                    )
                else:
                    vertices = np.asarray(parsed_vertices, dtype=np.float64)

            uvs_raw = fold_raw.get("uvs")
            uvs: np.ndarray | None = None
            if not isinstance(uvs_raw, list):
                _add(
                    issues,
                    "UVS_INVALID",
                    "uvs must be an array",
                    layer_index=fold_layer_index,
                    fold_id=fold_id,
                )
            elif vertices is not None and len(uvs_raw) != len(vertices):
                _add(
                    issues,
                    "UVS_INVALID",
                    "uvs length must match vertices length",
                    layer_index=fold_layer_index,
                    fold_id=fold_id,
                )
            else:
                parsed_uvs: list[list[float]] = []
                valid_uvs = True
                for uv in uvs_raw:
                    if (
                        not isinstance(uv, list)
                        or len(uv) != 2
                        or not all(finite_float(v) for v in uv)
                    ):
                        valid_uvs = False
                        break
                    parsed_uvs.append([float(uv[0]), float(uv[1])])
                if not valid_uvs:
                    _add(
                        issues,
                        "UVS_INVALID",
                        "every uv must be a finite two-component vector",
                        layer_index=fold_layer_index,
                        fold_id=fold_id,
                    )
                else:
                    uvs = np.asarray(parsed_uvs, dtype=np.float64)

            triangles_raw = fold_raw.get("triangles")
            triangles: np.ndarray | None = None
            if not isinstance(triangles_raw, list) or not triangles_raw:
                _add(
                    issues,
                    "TRIANGLES_INVALID",
                    "triangles must be a non-empty array",
                    layer_index=fold_layer_index,
                    fold_id=fold_id,
                )
            else:
                parsed_triangles: list[list[int]] = []
                triangles_valid = True
                for triangle in triangles_raw:
                    if (
                        not isinstance(triangle, list)
                        or len(triangle) != 3
                        or not all(is_int(index) for index in triangle)
                    ):
                        _add(
                            issues,
                            "TRIANGLE_INVALID",
                            "every triangle must contain three integer indices",
                            layer_index=fold_layer_index,
                            fold_id=fold_id,
                        )
                        triangles_valid = False
                        break
                    parsed_triangles.append([int(i) for i in triangle])
                if triangles_valid and vertices is not None:
                    for tri_index, triangle in enumerate(parsed_triangles):
                        if any(
                            index < 0 or index >= len(vertices)
                            for index in triangle
                        ):
                            _add(
                                issues,
                                "MESH_INDEX_OUT_OF_RANGE",
                                f"triangle {tri_index} has an out-of-range vertex index",
                                layer_index=fold_layer_index,
                                fold_id=fold_id,
                            )
                            triangles_valid = False
                            break
                    if triangles_valid:
                        for tri_index, triangle in enumerate(parsed_triangles):
                            a, b, c = (
                                vertices[triangle[0]],
                                vertices[triangle[1]],
                                vertices[triangle[2]],
                            )
                            if _triangle_area(a, b, c) <= 1e-12:
                                _add(
                                    issues,
                                    "DEGENERATE_TRIANGLE",
                                    f"triangle {tri_index} is degenerate",
                                    layer_index=fold_layer_index,
                                    fold_id=fold_id,
                                )
                                triangles_valid = False
                                break
                if triangles_valid:
                    triangles = np.asarray(parsed_triangles, dtype=np.int64)

            if (
                layer_index is not None
                and fold_id is not None
                and vertices is not None
                and triangles is not None
                and uvs is not None
            ):
                order = np.lexsort(
                    (
                        triangles[:, 2],
                        triangles[:, 1],
                        triangles[:, 0],
                    )
                )
                folds.append(
                    TriangleMesh(
                        layer_index=layer_index,
                        fold_id=fold_id,
                        vertices=vertices,
                        triangles=triangles[order],
                        uvs=uvs,
                    )
                )

        if (
            layer_index is not None
            and finite_float(thickness)
            and finite_float(transmittance)
            and 0.0 <= float(transmittance) <= 1.0
            and scatter is not None
            and is_int(seed)
            and seed >= 0
            and folds
        ):
            layers.append(
                LayerData(
                    layer_index=layer_index,
                    thickness=float(thickness),
                    transmittance=float(transmittance),
                    scatter_color=scatter,
                    pattern_seed=int(seed),
                    folds=tuple(sorted(folds, key=lambda fold: fold.fold_id)),
                )
            )

    tassels_raw = scene.get("tassels")
    tassels: list[TasselData] = []
    seen_tassel_ids: set[str] = set()
    if not isinstance(tassels_raw, list):
        _add(issues, "TASSELS_INVALID", "tassels must be an array")
        tassels_raw = []
    for tassel_raw in tassels_raw:
        if not isinstance(tassel_raw, dict):
            _add(issues, "TASSEL_INVALID", "each tassel must be an object")
            continue
        tassel_id_raw = tassel_raw.get("tassel_id")
        tassel_id: str | None = None
        if not isinstance(tassel_id_raw, str) or not tassel_id_raw:
            _add(issues, "TASSEL_ID_INVALID", "tassel_id must be a non-empty string")
        else:
            tassel_id = tassel_id_raw
            if tassel_id in seen_tassel_ids:
                _add(
                    issues,
                    "DUPLICATE_TASSEL_ID",
                    f"tassel_id {tassel_id} is duplicated",
                    tassel_id=tassel_id,
                )
            seen_tassel_ids.add(tassel_id)
        segments_raw = tassel_raw.get("segments")
        segments: list[TasselSegment] = []
        if not isinstance(segments_raw, list) or not segments_raw:
            _add(
                issues,
                "TASSEL_GEOMETRY_INVALID",
                "segments must be a non-empty array",
                tassel_id=tassel_id,
            )
            segments_raw = []
        for segment_index, segment_raw in enumerate(segments_raw):
            if not isinstance(segment_raw, dict):
                _add(
                    issues,
                    "TASSEL_GEOMETRY_INVALID",
                    "each segment must be an object",
                    tassel_id=tassel_id,
                )
                continue
            a = _read_vec3(
                segment_raw.get("a"),
                issues,
                "TASSEL_GEOMETRY_INVALID",
                "segment.a",
                tassel_id=tassel_id,
            )
            b = _read_vec3(
                segment_raw.get("b"),
                issues,
                "TASSEL_GEOMETRY_INVALID",
                "segment.b",
                tassel_id=tassel_id,
            )
            radius = segment_raw.get("radius")
            segment_ok = (
                a is not None
                and b is not None
                and finite_float(radius)
                and float(radius) > 0.0
            )
            if a is not None and b is not None and float(np.linalg.norm(b - a)) < 1e-9:
                _add(
                    issues,
                    "TASSEL_GEOMETRY_INVALID",
                    "segment endpoints must differ",
                    tassel_id=tassel_id,
                )
                segment_ok = False
            if not finite_float(radius) or float(radius) <= 0.0:
                _add(
                    issues,
                    "TASSEL_GEOMETRY_INVALID",
                    "segment.radius must be positive",
                    tassel_id=tassel_id,
                )
            if segment_ok and tassel_id is not None:
                segments.append(
                    TasselSegment(
                        tassel_id=tassel_id,
                        segment_index=segment_index,
                        a=a,
                        b=b,
                        radius=float(radius),
                    )
                )
        if tassel_id is not None and segments:
            tassels.append(TasselData(tassel_id=tassel_id, segments=tuple(segments)))

    lights_raw = scene.get("lights")
    lights: list[LightData] = []
    seen_light_ids: set[str] = set()
    if not isinstance(lights_raw, list) or len(lights_raw) != 3:
        _add(issues, "LIGHT_COUNT_INVALID", "exactly three lights are required")
        lights_raw = lights_raw if isinstance(lights_raw, list) else []
    for light_raw in lights_raw:
        if not isinstance(light_raw, dict):
            _add(issues, "LIGHT_INVALID", "each light must be an object")
            continue
        light_id_raw = light_raw.get("light_id")
        light_id: str | None = None
        if not isinstance(light_id_raw, str) or not light_id_raw:
            _add(issues, "LIGHT_ID_INVALID", "light_id must be a non-empty string")
        else:
            light_id = light_id_raw
            if light_id in seen_light_ids:
                _add(
                    issues,
                    "DUPLICATE_LIGHT_ID",
                    f"light_id {light_id} is duplicated",
                    light_id=light_id,
                )
            seen_light_ids.add(light_id)
        position = _read_vec3(
            light_raw.get("position"),
            issues,
            "LIGHT_INVALID",
            "light.position",
            light_id=light_id,
        )
        color = _read_vec3(
            light_raw.get("color"),
            issues,
            "LIGHT_INVALID",
            "light.color",
            light_id=light_id,
        )
        intensity = light_raw.get("intensity")
        radius = light_raw.get("radius")
        cast_shadow = light_raw.get("cast_shadow")
        if not finite_float(intensity) or float(intensity) <= 0.0:
            _add(
                issues,
                "LIGHT_INVALID",
                "light.intensity must be positive",
                light_id=light_id,
            )
        if not finite_float(radius) or float(radius) <= 0.0:
            _add(
                issues,
                "LIGHT_INVALID",
                "light.radius must be positive",
                light_id=light_id,
            )
        if not isinstance(cast_shadow, bool):
            _add(
                issues,
                "LIGHT_INVALID",
                "light.cast_shadow must be a boolean",
                light_id=light_id,
            )
        if color is not None and (np.any(color < 0.0) or np.any(color > 1.0)):
            _add(
                issues,
                "LIGHT_INVALID",
                "light.color components must be in [0,1]",
                light_id=light_id,
            )
            color = None
        if (
            light_id is not None
            and position is not None
            and color is not None
            and finite_float(intensity)
            and float(intensity) > 0.0
            and finite_float(radius)
            and float(radius) > 0.0
            and isinstance(cast_shadow, bool)
        ):
            lights.append(
                LightData(
                    light_id=light_id,
                    position=position,
                    intensity=float(intensity),
                    radius=float(radius),
                    color=color,
                    cast_shadow=cast_shadow,
                )
            )

    shadows_raw = scene.get("shadows")
    shadows: dict[str, Any] = {}
    if not isinstance(shadows_raw, dict):
        _add(issues, "SHADOWS_INVALID", "shadows must be an object")
    else:
        samples = shadows_raw.get("samples_per_light")
        if not is_int(samples) or not 1 <= samples <= 4096:
            _add(
                issues,
                "SHADOW_SAMPLES_INVALID",
                "shadows.samples_per_light must be an integer in [1,4096]",
            )
        spacing = shadows_raw.get("edge_scan_spacing_px")
        if not finite_float(spacing) or not 0.1 <= float(spacing) <= 16.0:
            _add(
                issues,
                "SHADOW_SCAN_INVALID",
                "shadows.edge_scan_spacing_px must be in [0.1,16]",
            )
        max_px = shadows_raw.get("edge_scan_max_px")
        if not finite_float(max_px) or not 4.0 <= float(max_px) <= 4096.0:
            _add(
                issues,
                "SHADOW_SCAN_INVALID",
                "shadows.edge_scan_max_px must be in [4,4096]",
            )
        pattern = shadows_raw.get("sample_pattern")
        if pattern != "vogel_disk_seeded_v1":
            _add(
                issues,
                "SHADOW_SAMPLE_PATTERN_INVALID",
                "shadows.sample_pattern must be 'vogel_disk_seeded_v1'",
            )
        measurements = shadows_raw.get("measurements")
        if not isinstance(measurements, list) or not measurements:
            _add(
                issues,
                "SHADOW_MEASUREMENTS_INVALID",
                "shadows.measurements must be a non-empty array",
            )
        if (
            is_int(samples)
            and finite_float(spacing)
            and finite_float(max_px)
            and pattern == "vogel_disk_seeded_v1"
        ):
            shadows = {
                "samples_per_light": int(samples),
                "edge_scan_spacing_px": float(spacing),
                "edge_scan_max_px": float(max_px),
                "sample_pattern": pattern,
                "measurements": measurements
                if isinstance(measurements, list)
                else [],
            }

    light_ids = {light.light_id for light in lights}
    tassel_ids = {tassel.tassel_id for tassel in tassels}
    if isinstance(shadows.get("measurements"), list):
        for measurement in shadows["measurements"]:
            if not isinstance(measurement, dict):
                _add(
                    issues,
                    "SHADOW_MEASUREMENTS_INVALID",
                    "each shadow measurement must be an object",
                )
                continue
            tassel_id = measurement.get("tassel_id")
            light_id = measurement.get("light_id")
            if tassel_id not in tassel_ids:
                _add(
                    issues,
                    "SHADOW_MEASUREMENTS_INVALID",
                    "measurement references an unknown tassel_id",
                    tassel_id=tassel_id if isinstance(tassel_id, str) else None,
                    light_id=light_id if isinstance(light_id, str) else None,
                )
            if light_id not in light_ids:
                _add(
                    issues,
                    "SHADOW_MEASUREMENTS_INVALID",
                    "measurement references an unknown light_id",
                    tassel_id=tassel_id if isinstance(tassel_id, str) else None,
                    light_id=light_id if isinstance(light_id, str) else None,
                )

    validation_raw = scene.get("validation")
    geometry_epsilon = 1e-5
    lamp_clearance = 1e-5
    if not isinstance(validation_raw, dict):
        _add(issues, "VALIDATION_CONFIG_INVALID", "validation must be an object")
    else:
        eps = validation_raw.get("geometry_epsilon")
        clearance = validation_raw.get("lamp_surface_clearance")
        if not finite_float(eps) or not 0.0 <= float(eps) <= 1e-2:
            _add(
                issues,
                "VALIDATION_CONFIG_INVALID",
                "validation.geometry_epsilon must be in [0,0.01]",
            )
        else:
            geometry_epsilon = float(eps)
        if not finite_float(clearance) or not 0.0 <= float(clearance) <= 1e-2:
            _add(
                issues,
                "VALIDATION_CONFIG_INVALID",
                "validation.lamp_surface_clearance must be in [0,0.01]",
            )
        else:
            lamp_clearance = float(clearance)

    # A lamp may not sit inside the physical thickness of a fabric layer.
    for light in lights:
        nearest_distance = float("inf")
        nearest_layer: int | None = None
        nearest_fold: int | None = None
        nearest_threshold = 0.0
        closed_volume_layer: int | None = None
        closed_volume_fold: int | None = None
        closed_cache: dict[int, bool] = {}
        for layer in layers:
            threshold = layer.thickness * 0.5 + lamp_clearance + geometry_epsilon
            for fold in layer.folds:
                vertices = fold.vertices
                for triangle in fold.triangles:
                    distance = _point_triangle_distance(
                        light.position,
                        vertices[int(triangle[0])],
                        vertices[int(triangle[1])],
                        vertices[int(triangle[2])],
                    )
                    if distance < nearest_distance:
                        nearest_distance = distance
                        nearest_layer = layer.layer_index
                        nearest_fold = fold.fold_id
                        nearest_threshold = threshold
            if layer.layer_index not in closed_cache:
                closed_cache[layer.layer_index] = _layer_is_closed(layer)
            if closed_cache[layer.layer_index]:
                winding = _point_winding_number(layer, light.position)
                if abs(winding) > 0.5 and closed_volume_fold is None:
                    closed_volume_layer = layer.layer_index
                    closed_volume_fold = layer.folds[0].fold_id
        surface_contains_light = (
            nearest_layer is not None and nearest_distance <= nearest_threshold
        )
        volume_contains_light = closed_volume_layer is not None
        if surface_contains_light:
            _add(
                issues,
                "LIGHT_INSIDE_FABRIC",
                (
                    f"light {light.light_id} lies inside the fabric thickness "
                    f"({nearest_distance:.7f} <= {nearest_threshold:.7f})"
                ),
                layer_index=nearest_layer,
                fold_id=nearest_fold,
                light_id=light.light_id,
            )
        elif volume_contains_light:
            _add(
                issues,
                "LIGHT_INSIDE_FABRIC",
                f"light {light.light_id} lies inside a closed fabric mesh",
                layer_index=closed_volume_layer,
                fold_id=closed_volume_fold,
                light_id=light.light_id,
            )

    if issues:
        return ValidationResult(issues=issues)

    canonical = canonical_scene(scene)
    assert camera is not None
    assert wall is not None
    assert background is not None
    data = SceneData(
        raw=scene,
        canonical=canonical,
        digest=scene_digest(scene),
        width=width,
        height=height,
        tile_width=tile_width,
        tile_height=tile_height,
        background_rgba=background,
        tone_map_exposure=exposure,
        tone_map_white_cap=white_cap,
        owner_alpha_threshold=owner_threshold,
        camera=camera,
        wall=wall,
        layers=tuple(sorted(layers, key=lambda layer: layer.layer_index)),
        tassels=tuple(sorted(tassels, key=lambda tassel: tassel.tassel_id)),
        lights=tuple(sorted(lights, key=lambda light: light.light_id)),
        shadows=shadows,
        shading=shading,
        validation_epsilon=geometry_epsilon,
        lamp_surface_clearance=lamp_clearance,
    )
    return ValidationResult(issues=[], data=data)

