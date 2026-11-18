from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SceneValidationError, ValidationIssue
from .util import canonical_json, sha256_bytes


@dataclass(frozen=True)
class Mesh:
    vertices: np.ndarray
    triangles: np.ndarray
    uvs: np.ndarray
    face_normals: np.ndarray


@dataclass(frozen=True)
class Layer:
    layer_index: int
    transmittance: float
    scatter_color: np.ndarray
    texture_resolution: int
    mesh: Mesh


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    near: float
    far: float

    @property
    def forward(self) -> np.ndarray:
        return _unit(self.target - self.position)

    @property
    def right(self) -> np.ndarray:
        return _unit(np.cross(self.forward, self.up))

    @property
    def true_up(self) -> np.ndarray:
        return _unit(np.cross(self.right, self.forward))


@dataclass(frozen=True)
class Light:
    position: np.ndarray
    color: np.ndarray
    intensity: float


@dataclass(frozen=True)
class Shadow:
    samples: int
    softness: float
    probe_light_distances: tuple[float, ...]


@dataclass(frozen=True)
class ImageSpec:
    width: int
    height: int
    tile_width: int
    tile_height: int
    background: np.ndarray


@dataclass(frozen=True)
class Scene:
    raw: dict
    digest: str
    seed: int
    image: ImageSpec
    camera: Camera
    light: Light
    shadow: Shadow
    layers: tuple[Layer, ...]

    @property
    def tile_columns(self) -> int:
        return (self.image.width + self.image.tile_width - 1) // self.image.tile_width

    @property
    def tile_rows(self) -> int:
        return (self.image.height + self.image.tile_height - 1) // self.image.tile_height

    @property
    def tile_count(self) -> int:
        return self.tile_columns * self.tile_rows


def _unit(vector: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(vector))
    if length <= 1e-15:
        return np.zeros(3, dtype=np.float64)
    return np.asarray(vector, dtype=np.float64) / length


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _finite_vector(value: Any, length: int = 3) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != length:
        return None
    if not all(_is_number(item) and math.isfinite(float(item)) for item in value):
        return None
    return np.asarray(value, dtype=np.float64)


def _default_uvs(vertices: np.ndarray, face_normals: np.ndarray) -> np.ndarray:
    normal = face_normals[0] if len(face_normals) else np.asarray([0.0, 0.0, 1.0])
    drop = int(np.argmax(np.abs(normal)))
    axes = [axis for axis in range(3) if axis != drop]
    planar = vertices[:, axes]
    lo = np.min(planar, axis=0)
    span = np.max(planar, axis=0) - lo
    span[span <= 1e-15] = 1.0
    return (planar - lo) / span


def _validate_mesh(
    raw_mesh: Any, layer_index: int, issues: list[ValidationIssue]
) -> Mesh | None:
    if not isinstance(raw_mesh, dict):
        issues.append(
            ValidationIssue("MESH_INVALID", layer_index, "mesh must be an object")
        )
        return None
    raw_vertices = raw_mesh.get("vertices")
    if not isinstance(raw_vertices, list) or len(raw_vertices) < 3:
        issues.append(
            ValidationIssue(
                "MESH_INVALID", layer_index, "mesh.vertices must contain at least 3 points"
            )
        )
        return None
    vertices_list: list[np.ndarray] = []
    for vertex_number, raw_vertex in enumerate(raw_vertices):
        vertex = _finite_vector(raw_vertex)
        if vertex is None:
            issues.append(
                ValidationIssue(
                    "MESH_VERTEX_INVALID",
                    layer_index,
                    f"vertex {vertex_number} must be a finite 3D vector",
                )
            )
            return None
        vertices_list.append(vertex)
    vertices = np.vstack(vertices_list)

    raw_triangles = raw_mesh.get("triangles")
    if not isinstance(raw_triangles, list) or not raw_triangles:
        issues.append(
            ValidationIssue(
                "MESH_INVALID", layer_index, "mesh.triangles must contain at least one triangle"
            )
        )
        return None
    triangles_list: list[list[int]] = []
    for triangle_number, raw_triangle in enumerate(raw_triangles):
        if not isinstance(raw_triangle, list) or len(raw_triangle) != 3:
            issues.append(
                ValidationIssue(
                    "MESH_INDEX_OUT_OF_RANGE",
                    layer_index,
                    f"triangle {triangle_number} must contain exactly 3 vertex indices",
                )
            )
            return None
        triangle: list[int] = []
        for raw_index in raw_triangle:
            if not _is_int(raw_index):
                issues.append(
                    ValidationIssue(
                        "MESH_INDEX_OUT_OF_RANGE",
                        layer_index,
                        f"triangle {triangle_number} contains a non-integer vertex index",
                    )
                )
                return None
            if raw_index < 0 or raw_index >= len(vertices):
                issues.append(
                    ValidationIssue(
                        "MESH_INDEX_OUT_OF_RANGE",
                        layer_index,
                        f"triangle {triangle_number} index {raw_index} is outside "
                        f"vertex range [0, {len(vertices) - 1}]",
                    )
                )
                return None
            triangle.append(raw_index)
        triangles_list.append(triangle)
    triangles = np.asarray(triangles_list, dtype=np.int64)

    tri_vertices = vertices[triangles]
    cross = np.cross(
        tri_vertices[:, 1] - tri_vertices[:, 0],
        tri_vertices[:, 2] - tri_vertices[:, 0],
    )
    double_area = np.linalg.norm(cross, axis=1)
    edges = np.linalg.norm(
        np.stack(
            (
                tri_vertices[:, 1] - tri_vertices[:, 0],
                tri_vertices[:, 2] - tri_vertices[:, 1],
                tri_vertices[:, 0] - tri_vertices[:, 2],
            ),
            axis=1,
        ),
        axis=2,
    )
    scale = np.max(edges, axis=1)
    tolerance = np.maximum(scale * scale * 1e-12, 1e-15)
    degenerate = np.nonzero(double_area <= tolerance)[0]
    if len(degenerate):
        first = int(degenerate[0])
        issues.append(
            ValidationIssue(
                "TRIANGLE_DEGENERATE",
                layer_index,
                f"triangle {first} has zero or near-zero area",
            )
        )
        return None
    face_normals = cross / double_area[:, None]

    raw_uvs = raw_mesh.get("uvs")
    if raw_uvs is None:
        uvs = _default_uvs(vertices, face_normals)
    else:
        if not isinstance(raw_uvs, list) or len(raw_uvs) != len(vertices):
            issues.append(
                ValidationIssue(
                    "MESH_UV_INVALID",
                    layer_index,
                    "mesh.uvs must contain one 2D coordinate per vertex",
                )
            )
            return None
        uv_list: list[np.ndarray] = []
        for uv_number, raw_uv in enumerate(raw_uvs):
            uv = _finite_vector(raw_uv, length=2)
            if uv is None:
                issues.append(
                    ValidationIssue(
                        "MESH_UV_INVALID",
                        layer_index,
                        f"uv {uv_number} must be a finite 2D coordinate",
                    )
                )
                return None
            uv_list.append(uv)
        uvs = np.vstack(uv_list)
    return Mesh(vertices=vertices, triangles=triangles, uvs=uvs, face_normals=face_normals)


def _light_on_mesh(mesh: Mesh, point: np.ndarray) -> bool:
    """True when the point is on the surface or strictly inside a closed mesh."""
    tri = mesh.vertices[mesh.triangles]
    nearest_distance = math.inf
    for a, b, c in tri:
        ab = b - a
        ac = c - a
        normal = np.cross(ab, ac)
        normal_length = float(np.linalg.norm(normal))
        if normal_length <= 1e-15:
            continue
        plane_distance = abs(float(np.dot(point - a, normal / normal_length)))
        if plane_distance >= nearest_distance:
            continue
        projected = point - (point - a) @ (normal / normal_length) * (normal / normal_length)
        v0 = b - a
        v1 = c - a
        v2 = projected - a
        d00 = float(np.dot(v0, v0))
        d01 = float(np.dot(v0, v1))
        d11 = float(np.dot(v1, v1))
        d20 = float(np.dot(v2, v0))
        d21 = float(np.dot(v2, v1))
        denominator = d00 * d11 - d01 * d01
        if denominator <= 1e-20:
            continue
        bary_v = (d11 * d20 - d01 * d21) / denominator
        bary_w = (d00 * d21 - d01 * d20) / denominator
        bary_u = 1.0 - bary_v - bary_w
        if bary_u >= -1e-10 and bary_v >= -1e-10 and bary_w >= -1e-10:
            nearest_distance = min(nearest_distance, plane_distance)
    if nearest_distance <= 1e-9:
        return True

    edge_counts: dict[tuple[int, int], int] = {}
    for triangle in mesh.triangles:
        for first, second in (
            (int(triangle[0]), int(triangle[1])),
            (int(triangle[1]), int(triangle[2])),
            (int(triangle[2]), int(triangle[0])),
        ):
            edge = (first, second) if first < second else (second, first)
            edge_counts[edge] = edge_counts.get(edge, 0) + 1
    if not edge_counts or any(count != 2 for count in edge_counts.values()):
        return False

    # Solid-angle winding test for a watertight triangle mesh.
    a = tri[:, 0] - point
    b = tri[:, 1] - point
    c = tri[:, 2] - point
    numerator = np.einsum("ij,ij->i", a, np.cross(b, c))
    denominator = (
        np.linalg.norm(a, axis=1) * np.linalg.norm(b, axis=1) * np.linalg.norm(c, axis=1)
        + np.einsum("ij,ij->i", a, b) * np.linalg.norm(c, axis=1)
        + np.einsum("ij,ij->i", b, c) * np.linalg.norm(a, axis=1)
        + np.einsum("ij,ij->i", c, a) * np.linalg.norm(b, axis=1)
    )
    solid_angle = float(np.sum(2.0 * np.arctan2(numerator, denominator)))
    if abs(solid_angle) > 2.0 * math.pi:
        return True

    # Majority ray-parity fallback also handles watertight meshes with mixed
    # face winding, where the solid-angle sum can cancel.
    inside_votes = 0
    ray_directions = (
        _unit(np.asarray([0.219, 0.571, 0.769])),
        _unit(np.asarray([-0.653, 0.337, 0.677])),
        _unit(np.asarray([0.431, -0.743, 0.513])),
    )
    vertices = mesh.vertices[mesh.triangles]
    for direction in ray_directions:
        intersections = 0
        for triangle in vertices:
            v0, v1, v2 = triangle
            edge1 = v1 - v0
            edge2 = v2 - v0
            h = np.cross(direction, edge2)
            determinant = float(np.dot(edge1, h))
            if abs(determinant) <= 1e-14:
                continue
            inverse = 1.0 / determinant
            s = point - v0
            bary_v = inverse * float(np.dot(s, h))
            if bary_v < -1e-10 or bary_v > 1.0 + 1e-10:
                continue
            q = np.cross(s, edge1)
            bary_w = inverse * float(np.dot(direction, q))
            if bary_w < -1e-10 or bary_v + bary_w > 1.0 + 1e-10:
                continue
            distance = inverse * float(np.dot(edge2, q))
            if distance > 1e-9:
                intersections += 1
        if intersections % 2 == 1:
            inside_votes += 1
    return inside_votes >= 2


def _require_object(data: Any, key: str, label: str) -> dict:
    value = data.get(key)
    if not isinstance(value, dict):
        raise SceneValidationError(
            [ValidationIssue("SCENE_SCHEMA_INVALID", None, f"{label} must be an object")]
        )
    return value


def validate_scene_data(data: Any) -> Scene:
    issues: list[ValidationIssue] = []
    if not isinstance(data, dict):
        raise SceneValidationError(
            [ValidationIssue("SCENE_SCHEMA_INVALID", None, "scene root must be an object")]
        )
    if data.get("schema_version") != 1:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "schema_version must be 1"
            )
        )
    seed = data.get("seed")
    if not _is_int(seed) or seed < 0:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "seed must be a non-negative integer"
            )
        )
        seed_value = 0
    else:
        seed_value = int(seed)

    image_data = data.get("image")
    if not isinstance(image_data, dict):
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "image must be an object")
        )
        image_data = {}
    width = image_data.get("width")
    height = image_data.get("height")
    tile_width = image_data.get("tile_width")
    tile_height = image_data.get("tile_height")
    for name, value in (
        ("image.width", width),
        ("image.height", height),
        ("image.tile_width", tile_width),
        ("image.tile_height", tile_height),
    ):
        if not _is_int(value) or value <= 0:
            issues.append(
                ValidationIssue(
                    "SCENE_SCHEMA_INVALID", None, f"{name} must be a positive integer"
                )
            )
    if _is_int(width) and _is_int(height) and width * height > 16_000_000:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "image exceeds the 16 megapixel safety limit"
            )
        )
    background = _finite_vector(image_data.get("background"))
    if background is None or np.any(background < 0.0) or np.any(background > 1.0):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID",
                None,
                "image.background must be an RGB vector in [0, 1]",
            )
        )
        background = np.asarray([0.0, 0.0, 0.0])

    camera_data = data.get("camera")
    if not isinstance(camera_data, dict):
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera must be an object")
        )
        camera_data = {}
    camera_position = _finite_vector(camera_data.get("position"))
    camera_target = _finite_vector(camera_data.get("target"))
    camera_up = _finite_vector(camera_data.get("up"))
    fov = camera_data.get("fov_y_deg")
    near = camera_data.get("near")
    far = camera_data.get("far")
    if camera_position is None:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera.position is invalid")
        )
    if camera_target is None:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera.target is invalid")
        )
    if camera_up is None or (camera_up is not None and np.linalg.norm(camera_up) <= 1e-12):
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera.up is invalid")
        )
    if not _is_number(fov) or not math.isfinite(float(fov)) or not 0.0 < float(fov) < 180.0:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "camera.fov_y_deg must be in (0, 180)"
            )
        )
    if not _is_number(near) or not math.isfinite(float(near)) or float(near) <= 0.0:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera.near must be positive")
        )
    if not _is_number(far) or not math.isfinite(float(far)) or float(far) <= 0.0:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera.far must be positive")
        )
    if (
        _is_number(near)
        and _is_number(far)
        and float(far) <= float(near)
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "camera.far must be greater than camera.near"
            )
        )
    if (
        camera_position is not None
        and camera_target is not None
        and camera_up is not None
        and np.linalg.norm(camera_target - camera_position) <= 1e-12
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "camera position and target must differ"
            )
        )
    if (
        camera_position is not None
        and camera_target is not None
        and camera_up is not None
        and np.linalg.norm(
            np.cross(camera_target - camera_position, camera_up)
        )
        <= 1e-12
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "camera.up must not be parallel to view direction"
            )
        )

    light_data = data.get("light")
    if not isinstance(light_data, dict):
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "light must be an object")
        )
        light_data = {}
    light_position = _finite_vector(light_data.get("position"))
    light_color = _finite_vector(light_data.get("color"))
    light_intensity = light_data.get("intensity")
    if light_position is None:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "light.position is invalid")
        )
    if (
        light_color is None
        or np.any(light_color < 0.0)
        or np.any(light_color > 1.0)
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "light.color must be an RGB vector in [0, 1]"
            )
        )
        light_color = np.asarray([1.0, 1.0, 1.0])
    if (
        not _is_number(light_intensity)
        or not math.isfinite(float(light_intensity))
        or float(light_intensity) < 0.0
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID", None, "light.intensity must be a non-negative number"
            )
        )
        light_intensity = 0.0

    shadow_data = data.get("shadow")
    if not isinstance(shadow_data, dict):
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "shadow must be an object")
        )
        shadow_data = {}
    shadow_samples = shadow_data.get("samples")
    saturation = shadow_data.get("softness")
    probe_distances = shadow_data.get("probe_light_distances", [1.0, 2.0, 3.0, 4.0])
    if not _is_int(shadow_samples) or not 1 <= shadow_samples <= 4096:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID",
                None,
                "shadow.samples must be an integer in [1, 4096]",
            )
        )
        shadow_samples = 1
    if (
        not _is_number(saturation)
        or not math.isfinite(float(saturation))
        or not 0.0 <= float(saturation) < 0.5
    ):
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID",
                None,
                "shadow.softness must be an angular radius in [0, 0.5)",
            )
        )
        saturation = 0.0
    if not isinstance(probe_distances, list) or not probe_distances:
        issues.append(
            ValidationIssue(
                "SCENE_SCHEMA_INVALID",
                None,
                "shadow.probe_light_distances must be a non-empty array",
            )
        )
        probe_distances = [1.0]
    valid_probes: list[float] = []
    for distance in probe_distances:
        if not _is_number(distance) or not math.isfinite(float(distance)) or float(distance) <= 0.0:
            issues.append(
                ValidationIssue(
                    "SCENE_SCHEMA_INVALID",
                    None,
                    "shadow.probe_light_distances entries must be positive",
                )
            )
            break
        valid_probes.append(float(distance))

    layers_data = data.get("layers")
    layers: list[Layer] = []
    seen_layer_indices: set[int] = set()
    if not isinstance(layers_data, list) or not layers_data:
        issues.append(
            ValidationIssue("SCENE_SCHEMA_INVALID", None, "layers must be a non-empty array")
        )
        layers_data = []
    for layer_number, raw_layer in enumerate(layers_data):
        if not isinstance(raw_layer, dict):
            issues.append(
                ValidationIssue(
                    "LAYER_INVALID", None, f"layer entry {layer_number} must be an object"
                )
            )
            continue
        raw_index = raw_layer.get("layer_index")
        if not _is_int(raw_index) or raw_index < 0:
            issues.append(
                ValidationIssue(
                    "LAYER_INVALID",
                    None,
                    f"layer entry {layer_number} layer_index must be a non-negative integer",
                )
            )
            layer_index = -1
        else:
            layer_index = int(raw_index)
        if layer_index >= 0:
            if layer_index in seen_layer_indices:
                issues.append(
                    ValidationIssue(
                        "DUPLICATE_LAYER_INDEX",
                        layer_index,
                        "layer_index is repeated in the scene",
                    )
                )
            seen_layer_indices.add(layer_index)

        transmittance = raw_layer.get("transmittance")
        if (
            not _is_number(transmittance)
            or not math.isfinite(float(transmittance))
            or not 0.0 <= float(transmittance) <= 1.0
        ):
            issues.append(
                ValidationIssue(
                    "TRANSMITTANCE_OUT_OF_RANGE",
                    layer_index,
                    "transmittance must be in [0, 1]",
                )
            )
            transmittance_value = 0.0
        else:
            transmittance_value = float(transmittance)

        scatter_color = _finite_vector(raw_layer.get("scatter_color"))
        if (
            scatter_color is None
            or np.any(scatter_color < 0.0)
            or np.any(scatter_color > 1.0)
        ):
            issues.append(
                ValidationIssue(
                    "SCATTER_COLOR_OUT_OF_RANGE",
                    layer_index,
                    "scatter_color must be an RGB vector in [0, 1]",
                )
            )
            scatter_color = np.asarray([1.0, 1.0, 1.0])

        texture_resolution = raw_layer.get("texture_resolution")
        if (
            not _is_int(texture_resolution)
            or not 4 <= texture_resolution <= 1024
        ):
            issues.append(
                ValidationIssue(
                    "TEXTURE_RESOLUTION_OUT_OF_RANGE",
                    layer_index,
                    "texture_resolution must be an integer in [4, 1024]",
                )
            )
            texture_resolution_value = 4
        else:
            texture_resolution_value = int(texture_resolution)

        mesh = _validate_mesh(raw_layer.get("mesh"), layer_index, issues)
        if layer_index >= 0 and mesh is not None and light_position is not None:
            if _light_on_mesh(mesh, light_position):
                issues.append(
                    ValidationIssue(
                        "LIGHT_INSIDE_LAYER",
                        layer_index,
                        "light position lies on or inside this layer mesh",
                    )
                )
        if (
            layer_index >= 0
            and mesh is not None
            and _is_number(transmittance)
            and 0.0 <= float(transmittance) <= 1.0
            and scatter_color is not None
            and texture_resolution_value >= 4
        ):
            layers.append(
                Layer(
                    layer_index=layer_index,
                    transmittance=transmittance_value,
                    scatter_color=scatter_color,
                    texture_resolution=texture_resolution_value,
                    mesh=mesh,
                )
            )

    if issues:
        raise SceneValidationError(issues)

    camera = Camera(
        position=camera_position,
        target=camera_target,
        up=camera_up,
        fov_y_deg=float(fov),
        near=float(near),
        far=float(far),
    )
    if np.linalg.norm(camera.right) <= 1e-12 or np.linalg.norm(camera.true_up) <= 1e-12:
        raise SceneValidationError(
            [ValidationIssue("SCENE_SCHEMA_INVALID", None, "camera basis is degenerate")]
        )
    layers.sort(key=lambda layer: layer.layer_index)
    return Scene(
        raw=data,
        digest=sha256_bytes(canonical_json(data)),
        seed=seed_value,
        image=ImageSpec(
            width=int(width),
            height=int(height),
            tile_width=int(tile_width),
            tile_height=int(tile_height),
            background=background,
        ),
        camera=camera,
        light=Light(
            position=light_position,
            color=light_color,
            intensity=float(light_intensity),
        ),
        shadow=Shadow(
            samples=int(shadow_samples),
            softness=float(saturation),
            probe_light_distances=tuple(valid_probes),
        ),
        layers=tuple(layers),
    )


def load_scene(path: str | Path) -> Scene:
    scene_path = Path(path)
    try:
        with open(scene_path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise SceneValidationError(
            [
                ValidationIssue(
                    "SCENE_JSON_INVALID",
                    None,
                    f"cannot read scene JSON: {exc.__class__.__name__}",
                )
            ]
        ) from exc
    return validate_scene_data(data)

