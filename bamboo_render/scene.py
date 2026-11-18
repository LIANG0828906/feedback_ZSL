from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import ALGORITHM_VERSION
from .errors import SceneError
from .io_utils import canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class BlockSpec:
    block_id: int
    x: int
    y: int
    width: int
    height: int


@dataclass(frozen=True)
class ImageSpec:
    width: int
    height: int
    chunk_width: int
    chunk_height: int

    @property
    def chunks_x(self) -> int:
        return (self.width + self.chunk_width - 1) // self.chunk_width

    @property
    def chunks_y(self) -> int:
        return (self.height + self.chunk_height - 1) // self.chunk_height

    @property
    def block_count(self) -> int:
        return self.chunks_x * self.chunks_y

    def blocks(self) -> list[BlockSpec]:
        result: list[BlockSpec] = []
        block_id = 0
        for by in range(self.chunks_y):
            for bx in range(self.chunks_x):
                x = bx * self.chunk_width
                y = by * self.chunk_height
                result.append(
                    BlockSpec(
                        block_id=block_id,
                        x=x,
                        y=y,
                        width=min(self.chunk_width, self.width - x),
                        height=min(self.chunk_height, self.height - y),
                    )
                )
                block_id += 1
        return result

    def block(self, block_id: int) -> BlockSpec:
        if block_id < 0 or block_id >= self.block_count:
            raise ValueError(f"block id {block_id} out of range")
        return self.blocks()[block_id]


@dataclass(frozen=True)
class CameraSpec:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_degrees: float
    near: float
    far: float

    @property
    def forward(self) -> np.ndarray:
        return _normalize(self.target - self.position)

    @property
    def right(self) -> np.ndarray:
        return _normalize(np.cross(self.forward, self.up))

    @property
    def true_up(self) -> np.ndarray:
        return _normalize(np.cross(self.right, self.forward))


@dataclass(frozen=True)
class LightSpec:
    position: np.ndarray
    intensity: float
    color: np.ndarray


@dataclass(frozen=True)
class WallSpec:
    point: np.ndarray
    normal: np.ndarray
    color: np.ndarray
    reflectance: float


@dataclass(frozen=True)
class ShadowSpec:
    sampling_density: int
    penumbra_per_gap: float


@dataclass(frozen=True)
class LinerSpec:
    transmittance: float
    scatter_color: np.ndarray
    reflectance: float
    fiber_contrast: float


@dataclass(frozen=True)
class LayerSpec:
    layer: int
    name: str
    air_gap: float
    plane_z: float
    weave_pitch: float
    strand_diameter: float
    texture_resolution: tuple[int, int]
    liner: LinerSpec
    strand_color: np.ndarray
    vertices: np.ndarray
    triangles: np.ndarray
    strand_ids: np.ndarray
    uvs: np.ndarray
    uv_transform: np.ndarray
    xy_min: np.ndarray
    xy_max: np.ndarray

    def uv_to_world(self, uv: np.ndarray) -> np.ndarray:
        transform = self.uv_transform
        inverse = np.linalg.inv(transform[:2, :])
        xy = (uv - transform[2, :]) @ inverse
        return np.array([xy[0], xy[1], self.plane_z], dtype=np.float64)

    def world_to_uv(self, points: np.ndarray) -> np.ndarray:
        flat = np.asarray(points, dtype=np.float64).reshape(-1, 3)
        a = np.column_stack([flat[:, 0], flat[:, 1], np.ones(flat.shape[0])])
        return a @ self.uv_transform


@dataclass(frozen=True)
class SceneSpec:
    schema_version: int
    scene_name: str
    seed: int
    image: ImageSpec
    camera: CameraSpec
    light: LightSpec
    wall: WallSpec
    shadow: ShadowSpec
    layers: tuple[LayerSpec, ...]
    source_digest: str
    source_size: int
    source_sha256: str

    @property
    def layer_numbers(self) -> tuple[int, ...]:
        return tuple(layer.layer for layer in self.layers)


@dataclass(frozen=True)
class LoadedScene:
    spec: SceneSpec
    raw: dict[str, Any]
    scene_digest: str


def load_scene(path: str | Path) -> LoadedScene:
    path = Path(path)
    try:
        raw_bytes = path.read_bytes()
    except OSError as exc:
        raise SceneError("SCENE_READ_ERROR", f"cannot read scene file: {exc}") from exc
    try:
        raw = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SceneError("JSON_PARSE_ERROR", f"scene is not valid UTF-8 JSON: {exc}") from exc
    return load_scene_data(raw, source_sha256=sha256_bytes(raw_bytes))


def load_scene_data(raw: Any, source_sha256: str | None = None) -> LoadedScene:
    spec = validate_scene(raw, source_sha256=source_sha256)
    digest_payload = {
        "algorithm": ALGORITHM_VERSION,
        "scene": raw,
    }
    digest = sha256_bytes(canonical_json_bytes(digest_payload))
    raw_bytes = canonical_json_bytes(raw)
    if source_sha256 is None:
        source_sha256 = sha256_bytes(raw_bytes)
    return LoadedScene(spec=spec, raw=raw, scene_digest=digest)


def validate_scene(raw: Any, source_sha256: str | None = None) -> SceneSpec:
    root = _mapping(raw, "scene")
    schema_version = _integer(root.get("schema_version"), "schema_version")
    if schema_version != 1:
        raise SceneError(
            "UNSUPPORTED_SCHEMA_VERSION",
            f"expected schema_version 1, got {schema_version}",
        )
    scene_name = _string(root.get("scene_name"), "scene_name")
    seed = _integer(root.get("seed"), "seed", minimum=0, maximum=(1 << 63) - 1)

    image = _parse_image(_mapping(root.get("image"), "image"))
    camera = _parse_camera(_mapping(root.get("camera"), "camera"), image)
    light = _parse_light(_mapping(root.get("light"), "light"))
    wall = _parse_wall(_mapping(root.get("inner_wall"), "inner_wall"), camera, image)
    shadow = _parse_shadow(_mapping(root.get("shadow"), "shadow"))

    layer_values = _sequence(root.get("layers"), "layers")
    if len(layer_values) == 0:
        raise SceneError("EMPTY_LAYER_STACK", "at least one bamboo layer is required")
    if len(layer_values) > 32:
        raise SceneError("TOO_MANY_LAYERS", "at most 32 bamboo layers are supported")

    origin_z = _number(root.get("layer_origin_z", 0.0), "layer_origin_z")
    parsed_layers: list[LayerSpec] = []
    seen_layers: set[int] = set()
    for input_index, value in enumerate(layer_values):
        layer_map = _mapping(value, f"layers[{input_index}]")
        layer_number = _integer(
            layer_map.get("layer"),
            f"layers[{input_index}].layer",
            minimum=1,
            maximum=99999,
        )
        if layer_number in seen_layers:
            raise SceneError(
                "DUPLICATE_LAYER_NUMBER",
                f"layer number {layer_number} appears more than once",
                layer=layer_number,
            )
        seen_layers.add(layer_number)
        parsed_layers.append(_parse_layer(layer_map, layer_number))

    parsed_layers.sort(key=lambda item: item.layer)
    cumulative_z = origin_z
    final_layers: list[LayerSpec] = []
    for index, layer in enumerate(parsed_layers):
        if index == 0:
            if abs(layer.air_gap) > 1e-9:
                raise SceneError(
                    "INNERMOST_AIR_GAP_INVALID",
                    "the innermost layer must declare air_gap 0.0",
                    layer=layer.layer,
                )
        else:
            if layer.air_gap <= 0.0:
                raise SceneError(
                    "AIR_GAP_INVALID",
                    "air_gap from the preceding layer must be positive",
                    layer=layer.layer,
                )
            cumulative_z += layer.air_gap
        layer_with_plane = _with_plane_z(layer, cumulative_z)
        final_layers.append(layer_with_plane)

    for layer in final_layers:
        _validate_planar_mesh(layer)
        _validate_uv_transform(layer)
        _validate_light_not_in_layer(light, layer)

    if float(light.position[2]) >= final_layers[0].plane_z:
        raise SceneError(
            "LIGHT_STACK_ORDER_INVALID",
            "point light must lie before the innermost layer along +Z",
            layer=final_layers[0].layer,
        )

    wall_z = float(wall.point[2])
    outer_z = final_layers[-1].plane_z
    if wall_z <= outer_z + 1e-9:
        raise SceneError(
            "WALL_PLANE_INVALID",
            "inner wall must lie beyond the outermost bamboo layer along +Z",
        )

    return SceneSpec(
        schema_version=schema_version,
        scene_name=scene_name,
        seed=seed,
        image=image,
        camera=camera,
        light=light,
        wall=wall,
        shadow=shadow,
        layers=tuple(final_layers),
        source_digest="",
        source_size=0,
        source_sha256=source_sha256 or "",
    )


def _parse_image(image: dict[str, Any]) -> ImageSpec:
    width = _integer(image.get("width"), "image.width", minimum=1, maximum=4096)
    height = _integer(image.get("height"), "image.height", minimum=1, maximum=4096)
    chunk = _sequence(image.get("chunk_size"), "image.chunk_size")
    if len(chunk) != 2:
        raise SceneError("CHUNK_SIZE_INVALID", "image.chunk_size must contain two integers")
    chunk_width = _integer(chunk[0], "image.chunk_size[0]", minimum=1, maximum=width)
    chunk_height = _integer(chunk[1], "image.chunk_size[1]", minimum=1, maximum=height)
    return ImageSpec(width, height, chunk_width, chunk_height)


def _parse_camera(camera: dict[str, Any], image: ImageSpec) -> CameraSpec:
    position = _vector(camera.get("position"), "camera.position", length=3)
    target = _vector(camera.get("target"), "camera.target", length=3)
    up = _vector(camera.get("up"), "camera.up", length=3)
    if float(np.linalg.norm(target - position)) <= 1e-9:
        raise SceneError("CAMERA_INVALID", "camera position and target must differ")
    forward = _normalize(target - position)
    if float(np.linalg.norm(np.cross(forward, up))) <= 1e-9:
        raise SceneError("CAMERA_INVALID", "camera up vector must not be parallel to view direction")
    fov = _number(camera.get("fov_y_degrees"), "camera.fov_y_degrees")
    if not 1.0 <= fov <= 179.0:
        raise SceneError("CAMERA_INVALID", "camera.fov_y_degrees must be in [1, 179]")
    near = _number(camera.get("near"), "camera.near")
    far = _number(camera.get("far"), "camera.far")
    if near <= 0.0 or far <= near:
        raise SceneError("CAMERA_INVALID", "camera requires 0 < near < far")
    if image.width <= 0 or image.height <= 0:
        raise SceneError("CAMERA_INVALID", "camera aspect requires positive image dimensions")
    return CameraSpec(position, target, _normalize(up), fov, near, far)


def _parse_light(light: dict[str, Any]) -> LightSpec:
    position = _vector(light.get("position"), "light.position", length=3)
    intensity = _number(light.get("intensity"), "light.intensity")
    if not 0.0 < intensity <= 1.0e6:
        raise SceneError("LIGHT_INVALID", "light.intensity must be in (0, 1e6]")
    color = _vector(light.get("color"), "light.color", length=3)
    _validate_unit_interval(color, "light.color")
    return LightSpec(position, intensity, color)


def _parse_wall(wall: dict[str, Any], camera: CameraSpec, image: ImageSpec) -> WallSpec:
    point = _vector(wall.get("point"), "inner_wall.point", length=3)
    normal = _vector(wall.get("normal"), "inner_wall.normal", length=3)
    normal_length = float(np.linalg.norm(normal))
    if normal_length <= 1e-9:
        raise SceneError("WALL_PLANE_INVALID", "inner_wall.normal must be non-zero")
    normal = normal / normal_length
    color = _vector(wall.get("color"), "inner_wall.color", length=3)
    _validate_unit_interval(color, "inner_wall.color")
    reflectance = _number(wall.get("reflectance"), "inner_wall.reflectance")
    if not 0.0 <= reflectance <= 1.0:
        raise SceneError("REFLECTANCE_OUT_OF_RANGE", "inner_wall.reflectance must be in [0, 1]")

    cos_fov = math.tan(math.radians(camera.fov_y_degrees) * 0.5)
    aspect = image.width / image.height
    for sx, sy in ((0.0, 0.0), (1.0, 0.0), (0.0, 1.0), (1.0, 1.0), (0.5, 0.5)):
        dx = (2.0 * sx - 1.0) * cos_fov * aspect
        dy = (1.0 - 2.0 * sy) * cos_fov
        direction = _normalize(camera.forward + dx * camera.right + dy * camera.true_up)
        denominator = float(np.dot(direction, normal))
        if abs(denominator) <= 1e-9:
            raise SceneError("WALL_PLANE_INVALID", "camera ray is parallel to the inner wall")
        t = float(np.dot(point - camera.position, normal) / denominator)
        if t <= camera.near:
            raise SceneError(
                "WALL_PLANE_INVALID",
                "inner wall does not intersect the camera frustum in front of the camera",
            )
    return WallSpec(point, normal, color, reflectance)


def _parse_shadow(shadow: dict[str, Any]) -> ShadowSpec:
    density = _integer(shadow.get("sampling_density"), "shadow.sampling_density", minimum=1, maximum=65)
    penumbra = _number(shadow.get("penumbra_per_gap"), "shadow.penumbra_per_gap")
    if not 0.0 < penumbra <= 10.0:
        raise SceneError(
            "SHADOW_SAMPLING_INVALID",
            "shadow.penumbra_per_gap must be in (0, 10]",
        )
    return ShadowSpec(density, penumbra)


def _parse_layer(layer_map: dict[str, Any], layer_number: int) -> LayerSpec:
    prefix = f"layer {layer_number}"
    name = _string(layer_map.get("name", f"layer_{layer_number}"), f"{prefix}.name")
    air_gap = _number(layer_map.get("air_gap"), f"{prefix}.air_gap")
    if air_gap < 0.0:
        raise SceneError("AIR_GAP_INVALID", "air_gap must be non-negative", layer=layer_number)
    pitch = _number(layer_map.get("weave_pitch"), f"{prefix}.weave_pitch")
    if pitch <= 0.0:
        raise SceneError("WEAVE_PITCH_INVALID", "weave_pitch must be positive", layer=layer_number)
    diameter = _number(layer_map.get("strand_diameter"), f"{prefix}.strand_diameter")
    if diameter <= 0.0 or diameter >= pitch:
        raise SceneError(
            "STRAND_DIAMETER_INVALID",
            "strand_diameter must be positive and smaller than weave_pitch",
            layer=layer_number,
        )
    resolution = _sequence(layer_map.get("texture_resolution"), f"{prefix}.texture_resolution")
    if len(resolution) != 2:
        raise SceneError(
            "TEXTURE_RESOLUTION_INVALID",
            "texture_resolution must contain two integers",
            layer=layer_number,
        )
    tex_w = _integer(resolution[0], f"{prefix}.texture_resolution[0]", minimum=8, maximum=2048)
    tex_h = _integer(resolution[1], f"{prefix}.texture_resolution[1]", minimum=8, maximum=2048)

    liner_map = _mapping(layer_map.get("liner"), f"{prefix}.liner")
    transmittance = _number(liner_map.get("transmittance"), f"{prefix}.liner.transmittance")
    if not 0.0 <= transmittance <= 1.0:
        raise SceneError(
            "TRANSMITTANCE_OUT_OF_RANGE",
            "liner.transmittance must be in [0, 1]",
            layer=layer_number,
        )
    scatter_color = _vector(liner_map.get("scatter_color"), f"{prefix}.liner.scatter_color", length=3)
    _validate_unit_interval(scatter_color, f"{prefix}.liner.scatter_color", layer=layer_number)
    reflectance = _number(liner_map.get("reflectance"), f"{prefix}.liner.reflectance")
    if not 0.0 <= reflectance <= 1.0:
        raise SceneError(
            "REFLECTANCE_OUT_OF_RANGE",
            "liner.reflectance must be in [0, 1]",
            layer=layer_number,
        )
    fiber_contrast = _number(
        liner_map.get("fiber_contrast", 0.2),
        f"{prefix}.liner.fiber_contrast",
    )
    if not 0.0 <= fiber_contrast <= 1.0:
        raise SceneError(
            "FIBER_CONTRAST_OUT_OF_RANGE",
            "liner.fiber_contrast must be in [0, 1]",
            layer=layer_number,
        )
    strand_color = _vector(layer_map.get("strand_color"), f"{prefix}.strand_color", length=3)
    _validate_unit_interval(strand_color, f"{prefix}.strand_color", layer=layer_number)

    mesh = _mapping(layer_map.get("mesh"), f"{prefix}.mesh")
    vertices = _matrix(mesh.get("vertices"), f"{prefix}.mesh.vertices", columns=3)
    if vertices.shape[0] < 3:
        raise SceneError(
            "MESH_TOO_SMALL",
            "mesh must contain at least three vertices",
            layer=layer_number,
        )
    triangles = _matrix(mesh.get("triangles"), f"{prefix}.mesh.triangles", columns=3)
    if triangles.shape[0] == 0:
        raise SceneError("MESH_EMPTY", "mesh must contain at least one triangle", layer=layer_number)
    if not np.all(np.equal(triangles, np.floor(triangles))):
        raise SceneError(
            "MESH_INDEX_INVALID",
            "triangle vertex indices must be integers",
            layer=layer_number,
        )
    triangle_indices = triangles.astype(np.int64)
    if np.any(triangle_indices < 0) or np.any(triangle_indices >= vertices.shape[0]):
        bad = int(np.argmax((triangle_indices < 0) | (triangle_indices >= vertices.shape[0])))
        bad_tri = bad // 3
        bad_vertex = int(triangle_indices.reshape(-1)[bad])
        raise SceneError(
            "MESH_INDEX_OUT_OF_RANGE",
            "triangle references a vertex outside the vertex array",
            layer=layer_number,
            details={"triangle_index": bad_tri, "vertex_index": bad_vertex},
        )
    strand_values = _sequence(mesh.get("strand_ids"), f"{prefix}.mesh.strand_ids")
    if len(strand_values) != triangle_indices.shape[0]:
        raise SceneError(
            "STRAND_ID_COUNT_MISMATCH",
            "strand_ids must contain one integer per triangle",
            layer=layer_number,
        )
    strand_ids = np.asarray(
        [_integer(value, f"{prefix}.mesh.strand_ids[{i}]", minimum=0) for i, value in enumerate(strand_values)],
        dtype=np.int64,
    )
    for triangle_index in range(triangle_indices.shape[0]):
        tri = triangle_indices[triangle_index]
        if len(set(int(v) for v in tri)) != 3:
            raise SceneError(
                "DEGENERATE_TRIANGLE",
                "triangle repeats a vertex index",
                layer=layer_number,
                details={"triangle_index": triangle_index},
            )
        p0, p1, p2 = vertices[tri]
        area = 0.5 * float(np.linalg.norm(np.cross(p1 - p0, p2 - p0)))
        if not np.isfinite(area) or area <= 1e-12:
            raise SceneError(
                "DEGENERATE_TRIANGLE",
                "triangle area is zero or too small",
                layer=layer_number,
                details={"triangle_index": triangle_index},
            )

    uv_values = mesh.get("uvs")
    if uv_values is None:
        x_min, x_max = float(vertices[:, 0].min()), float(vertices[:, 0].max())
        y_min, y_max = float(vertices[:, 1].min()), float(vertices[:, 1].max())
        if x_max - x_min <= 1e-12 or y_max - y_min <= 1e-12:
            raise SceneError("MESH_DEGENERATE_BOUNDS", "mesh has zero XY extent", layer=layer_number)
        uvs = np.column_stack(
            [
                (vertices[:, 0] - x_min) / (x_max - x_min),
                (vertices[:, 1] - y_min) / (y_max - y_min),
            ]
        )
    else:
        uvs = _matrix(uv_values, f"{prefix}.mesh.uvs", columns=2)
        if uvs.shape[0] != vertices.shape[0]:
            raise SceneError(
                "MESH_UV_COUNT_MISMATCH",
                "uvs must contain one pair per vertex",
                layer=layer_number,
            )
        if np.any(uvs < -1e-9) or np.any(uvs > 1.0 + 1e-9):
            raise SceneError(
                "MESH_UV_OUT_OF_RANGE",
                "uv coordinates must lie in [0, 1]",
                layer=layer_number,
            )
        uvs = np.clip(uvs, 0.0, 1.0)

    design = np.column_stack([vertices[:, 0], vertices[:, 1], np.ones(vertices.shape[0])])
    if np.linalg.matrix_rank(design) < 3:
        raise SceneError(
            "MESH_DEGENERATE_BOUNDS",
            "mesh XY coordinates do not span a surface for UV mapping",
            layer=layer_number,
        )
    transform, _, _, _ = np.linalg.lstsq(design, uvs, rcond=None)
    residual = np.max(np.abs(design @ transform - uvs))
    if residual > 1e-4:
        raise SceneError(
            "MESH_UV_INVALID",
            "uvs must be an affine mapping of planar layer XY coordinates",
            layer=layer_number,
        )

    return LayerSpec(
        layer=layer_number,
        name=name,
        air_gap=float(air_gap),
        plane_z=0.0,
        weave_pitch=float(pitch),
        strand_diameter=float(diameter),
        texture_resolution=(tex_w, tex_h),
        liner=LinerSpec(
            transmittance=float(transmittance),
            scatter_color=scatter_color,
            reflectance=float(reflectance),
            fiber_contrast=float(fiber_contrast),
        ),
        strand_color=strand_color,
        vertices=vertices,
        triangles=triangle_indices,
        strand_ids=strand_ids,
        uvs=uvs,
        uv_transform=transform,
        xy_min=np.array([vertices[:, 0].min(), vertices[:, 1].min()], dtype=np.float64),
        xy_max=np.array([vertices[:, 0].max(), vertices[:, 1].max()], dtype=np.float64),
    )


def _with_plane_z(layer: LayerSpec, plane_z: float) -> LayerSpec:
    vertices = layer.vertices.copy()
    vertices[:, 2] = plane_z
    return LayerSpec(
        layer=layer.layer,
        name=layer.name,
        air_gap=layer.air_gap,
        plane_z=float(plane_z),
        weave_pitch=layer.weave_pitch,
        strand_diameter=layer.strand_diameter,
        texture_resolution=layer.texture_resolution,
        liner=layer.liner,
        strand_color=layer.strand_color,
        vertices=vertices,
        triangles=layer.triangles,
        strand_ids=layer.strand_ids,
        uvs=layer.uvs,
        uv_transform=layer.uv_transform,
        xy_min=layer.xy_min,
        xy_max=layer.xy_max,
    )


def _validate_planar_mesh(layer: LayerSpec) -> None:
    z_error = float(np.max(np.abs(layer.vertices[:, 2] - layer.plane_z)))
    if z_error > 1e-6:
        raise SceneError(
            "MESH_NOT_PLANAR",
            "all mesh vertices must lie on the layer's declared plane",
            layer=layer.layer,
            details={"z_error": stable_scalar(z_error)},
        )


def _validate_uv_transform(layer: LayerSpec) -> None:
    if not np.all(np.isfinite(layer.uv_transform)):
        raise SceneError("MESH_UV_INVALID", "uv transform is not finite", layer=layer.layer)
    determinant = float(np.linalg.det(layer.uv_transform[:2, :]))
    if abs(determinant) <= 1e-12:
        raise SceneError("MESH_UV_INVALID", "uv transform is singular", layer=layer.layer)


def _validate_light_not_in_layer(light: LightSpec, layer: LayerSpec) -> None:
    radius = layer.strand_diameter * 0.5 + 1e-9
    for triangle_index, tri in enumerate(layer.triangles):
        p0, p1, p2 = layer.vertices[tri]
        distance = _point_triangle_distance(light.position, p0, p1, p2)
        if distance <= radius:
            raise SceneError(
                "LIGHT_INSIDE_LAYER_GEOMETRY",
                "point light lies inside the strand volume of this layer",
                layer=layer.layer,
                details={
                    "triangle_index": int(triangle_index),
                    "distance": stable_scalar(distance),
                },
            )


def _point_triangle_distance(point: np.ndarray, a: np.ndarray, b: np.ndarray, c: np.ndarray) -> float:
    ab = b - a
    ac = c - a
    ap = point - a
    d1 = float(np.dot(ab, ap))
    d2 = float(np.dot(ac, ap))
    if d1 <= 0.0 and d2 <= 0.0:
        return float(np.linalg.norm(point - a))
    bp = point - b
    d3 = float(np.dot(ab, bp))
    d4 = float(np.dot(ac, bp))
    if d3 >= 0.0 and d4 <= d3:
        return float(np.linalg.norm(point - b))
    vc = d1 * d4 - d3 * d2
    if vc <= 0.0 and d1 >= 0.0 and d3 <= 0.0:
        v = d1 / (d1 - d3)
        return float(np.linalg.norm(point - (a + v * ab)))
    cp = point - c
    d5 = float(np.dot(ab, cp))
    d6 = float(np.dot(ac, cp))
    if d6 >= 0.0 and d5 <= d6:
        return float(np.linalg.norm(point - c))
    vb = d5 * d2 - d1 * d6
    if vb <= 0.0 and d2 >= 0.0 and d6 <= 0.0:
        w = d2 / (d2 - d6)
        return float(np.linalg.norm(point - (a + w * ac)))
    va = d3 * d6 - d5 * d4
    if va <= 0.0 and (d4 - d3) >= 0.0 and (d5 - d6) >= 0.0:
        w = (d4 - d3) / ((d4 - d3) + (d5 - d6))
        return float(np.linalg.norm(point - (b + w * (c - b))))
    denominator = va + vb + vc
    if abs(denominator) <= 1e-15:
        return float(min(
            np.linalg.norm(point - a),
            np.linalg.norm(point - b),
            np.linalg.norm(point - c),
        ))
    v = vb / denominator
    w = vc / denominator
    return float(np.linalg.norm(point - (a + ab * v + ac * w)))


def stable_scalar(value: float, digits: int = 10) -> float:
    rounded = round(float(value), digits)
    return 0.0 if rounded == 0.0 else rounded


def _mapping(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise SceneError("INVALID_TYPE", f"{path} must be an object")
    return value


def _sequence(value: Any, path: str) -> list[Any] | tuple[Any, ...]:
    if not isinstance(value, (list, tuple)):
        raise SceneError("INVALID_TYPE", f"{path} must be an array")
    return value


def _string(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value:
        raise SceneError("INVALID_TYPE", f"{path} must be a non-empty string")
    return value


def _integer(
    value: Any,
    path: str,
    minimum: int | None = None,
    maximum: int | None = None,
) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise SceneError("INVALID_TYPE", f"{path} must be an integer")
    if minimum is not None and value < minimum:
        raise SceneError("VALUE_OUT_OF_RANGE", f"{path} must be >= {minimum}")
    if maximum is not None and value > maximum:
        raise SceneError("VALUE_OUT_OF_RANGE", f"{path} must be <= {maximum}")
    return int(value)


def _number(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise SceneError("INVALID_TYPE", f"{path} must be a number")
    result = float(value)
    if not math.isfinite(result):
        raise SceneError("VALUE_NOT_FINITE", f"{path} must be finite")
    return result


def _vector(value: Any, path: str, length: int) -> np.ndarray:
    values = _sequence(value, path)
    if len(values) != length:
        raise SceneError("INVALID_LENGTH", f"{path} must contain {length} numbers")
    return np.asarray([_number(item, f"{path}[{index}]") for index, item in enumerate(values)], dtype=np.float64)


def _matrix(value: Any, path: str, columns: int) -> np.ndarray:
    rows = _sequence(value, path)
    if len(rows) == 0:
        raise SceneError("INVALID_LENGTH", f"{path} must not be empty")
    parsed: list[list[float]] = []
    for index, row in enumerate(rows):
        values = _sequence(row, f"{path}[{index}]")
        if len(values) != columns:
            raise SceneError(
                "INVALID_LENGTH",
                f"{path}[{index}] must contain {columns} numbers",
            )
        parsed.append([_number(item, f"{path}[{index}][{column}]") for column, item in enumerate(values)])
    return np.asarray(parsed, dtype=np.float64)


def _validate_unit_interval(
    values: np.ndarray,
    path: str,
    layer: int | None = None,
) -> None:
    if np.any(values < 0.0) or np.any(values > 1.0):
        code = "REFLECTANCE_OUT_OF_RANGE" if path.endswith("reflectance") else "COLOR_OUT_OF_RANGE"
        raise SceneError(code, f"{path} values must be in [0, 1]", layer=layer)


def _normalize(vector: np.ndarray) -> np.ndarray:
    length = float(np.linalg.norm(vector))
    if length <= 1e-15:
        raise SceneError("VECTOR_INVALID", "cannot normalize a zero-length vector")
    return vector / length
