"""Scene JSON parsing and full preflight validation.

The parser deliberately rejects the whole scene before any image output is
created.  Every per-piece failure carries the quadrant and piece index.
"""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import SceneValidationError, scene_error


@dataclass(frozen=True)
class GlassPiece:
    serial: int
    quadrant: int
    piece_index: int
    vertices: np.ndarray
    triangles: np.ndarray
    transmission_color: np.ndarray
    transmittance: float
    lead_width: float
    lead_depth: float
    enamel_reflectance: float
    normal: np.ndarray
    bounds_xy: tuple[float, float, float, float]


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    forward: np.ndarray
    right: np.ndarray
    camera_up: np.ndarray
    vertical_fov_degrees: float


@dataclass(frozen=True)
class Light:
    direction: np.ndarray
    color: np.ndarray
    intensity: float
    angular_radius: float


@dataclass(frozen=True)
class Wall:
    z: float
    width: float
    height: float
    center_x: float
    center_y: float
    color: np.ndarray
    block_width: float
    block_height: float


@dataclass(frozen=True)
class TextureSettings:
    size: int
    transmission_low: float
    transmission_high: float
    reflection_low: float
    reflection_high: float


@dataclass(frozen=True)
class ShadowSettings:
    resolution: int
    samples: int
    penumbra_depth_scale: float
    profile_depth_levels: int
    profile_span: float


@dataclass(frozen=True)
class RenderSettings:
    ambient_transmission: float
    exposure: float
    wall_ambient: float
    wall_direct: float
    gamma: float


@dataclass(frozen=True)
class ReflectionSettings:
    environment_color: np.ndarray
    intensity: float
    fresnel_power: float


@dataclass(frozen=True)
class Scene:
    raw: dict[str, Any]
    digest: str
    schema_version: int
    name: str
    seed: int
    width: int
    height: int
    chunk_size: int
    camera: Camera
    light: Light
    wall: Wall
    window_center: np.ndarray
    window_normal: np.ndarray
    surround_outer_radius: float
    pieces: tuple[GlassPiece, ...]
    textures: TextureSettings
    shadows: ShadowSettings
    render: RenderSettings
    reflection: ReflectionSettings

    @property
    def chunks_x(self) -> int:
        return (self.width + self.chunk_size - 1) // self.chunk_size

    @property
    def chunks_y(self) -> int:
        return (self.height + self.chunk_size - 1) // self.chunk_size

    @property
    def chunk_count(self) -> int:
        return self.chunks_x * self.chunks_y

    def chunk_bounds(self, chunk_id: int) -> tuple[int, int, int, int]:
        if not 0 <= chunk_id < self.chunk_count:
            raise IndexError(f"chunk id out of range: {chunk_id}")
        cy, cx = divmod(chunk_id, self.chunks_x)
        x0 = cx * self.chunk_size
        y0 = cy * self.chunk_size
        return x0, y0, min(x0 + self.chunk_size, self.width), min(
            y0 + self.chunk_size, self.height
        )

    def chunk_id_at(self, x: int, y: int) -> int:
        return (y // self.chunk_size) * self.chunks_x + (x // self.chunk_size)

    def piece_mapping(self) -> list[dict[str, Any]]:
        return [
            {
                "serial": piece.serial,
                "quadrant": piece.quadrant,
                "piece_index": piece.piece_index,
            }
            for piece in self.pieces
        ]


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _finite_number(value: Any, path: str) -> float:
    if not _is_number(value) or not math.isfinite(float(value)):
        raise scene_error("RW_E_SCHEMA", f"{path} must be a finite number")
    return float(value)


def _positive_int(value: Any, path: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise scene_error("RW_E_SCHEMA", f"{path} must be a positive integer")
    return value


def _vec3(value: Any, path: str) -> np.ndarray:
    if not isinstance(value, list) or len(value) != 3:
        raise scene_error("RW_E_SCHEMA", f"{path} must be an array of 3 numbers")
    result = np.asarray(
        [_finite_number(item, f"{path}[{index}]") for index, item in enumerate(value)],
        dtype=np.float64,
    )
    return result


def _unit_vec3(value: Any, path: str) -> np.ndarray:
    vector = _vec3(value, path)
    length = float(np.linalg.norm(vector))
    if length <= 1e-12:
        raise scene_error("RW_E_GEOMETRY", f"{path} must not be the zero vector")
    return vector / length


def _object(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise scene_error("RW_E_SCHEMA", f"{path} must be an object")
    return value


def _get(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        raise scene_error("RW_E_SCHEMA", f"{path}.{key} is required")
    return mapping[key]


def _require_range(
    value: float, low: float, high: float, code: str, label: str, path: str
) -> float:
    if not low <= value <= high:
        raise scene_error(code, f"{path} ({label}) must be in [{low}, {high}]")
    return value


def canonical_scene_bytes(raw: dict[str, Any]) -> bytes:
    return json.dumps(
        raw, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode("utf-8")


def scene_digest(raw: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_scene_bytes(raw)).hexdigest()


def _parse_piece(
    raw_piece: dict[str, Any], raw_index: int, mesh_epsilon: float
) -> GlassPiece:
    quadrant_raw = raw_piece.get("quadrant")
    piece_raw = raw_piece.get("piece_index")
    if (
        not isinstance(quadrant_raw, int)
        or isinstance(quadrant_raw, bool)
        or quadrant_raw < 0
    ):
        raise scene_error(
            "RW_E_SCHEMA",
            f"glass_pieces[{raw_index}].quadrant must be a non-negative integer",
        )
    if (
        not isinstance(piece_raw, int)
        or isinstance(piece_raw, bool)
        or piece_raw < 0
    ):
        raise scene_error(
            "RW_E_SCHEMA",
            f"glass_pieces[{raw_index}].piece_index must be a non-negative integer",
        )
    quadrant = quadrant_raw
    piece_index = piece_raw

    mesh = _object(
        _get(raw_piece, "mesh", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].mesh",
    )
    vertices_raw = _get(mesh, "vertices", f"glass_pieces[{raw_index}].mesh")
    triangles_raw = _get(mesh, "triangles", f"glass_pieces[{raw_index}].mesh")
    if not isinstance(vertices_raw, list) or len(vertices_raw) < 3:
        raise scene_error(
            "RW_E_GEOMETRY",
            "mesh.vertices must contain at least three vertices",
            quadrant=quadrant,
            piece_index=piece_index,
        )
    if not isinstance(triangles_raw, list) or not triangles_raw:
        raise scene_error(
            "RW_E_GEOMETRY",
            "mesh.triangles must contain at least one triangle",
            quadrant=quadrant,
            piece_index=piece_index,
        )

    vertices = np.asarray(
        [
            _vec3(item, f"glass_pieces[{raw_index}].mesh.vertices[{vertex_index}]")
            for vertex_index, item in enumerate(vertices_raw)
        ],
        dtype=np.float64,
    )
    if not np.isfinite(vertices).all():
        raise scene_error(
            "RW_E_GEOMETRY",
            "mesh vertices must be finite",
            quadrant=quadrant,
            piece_index=piece_index,
        )

    triangles = np.empty((len(triangles_raw), 3), dtype=np.int32)
    for triangle_index, triangle in enumerate(triangles_raw):
        if not isinstance(triangle, list) or len(triangle) != 3:
            raise scene_error(
                "RW_E_GEOMETRY",
                f"triangle {triangle_index} must contain three indices",
                quadrant=quadrant,
                piece_index=piece_index,
            )
        for corner, index in enumerate(triangle):
            if not isinstance(index, int) or isinstance(index, bool):
                raise scene_error(
                    "RW_E_MESH_INDEX",
                    f"triangle {triangle_index} corner {corner} is not an integer index",
                    quadrant=quadrant,
                    piece_index=piece_index,
                    triangle=triangle_index,
                    corner=corner,
                    value=index,
                )
            if index < 0 or index >= len(vertices):
                raise scene_error(
                    "RW_E_MESH_INDEX",
                    f"triangle {triangle_index} corner {corner} index {index} "
                    f"is outside [0, {len(vertices) - 1}]",
                    quadrant=quadrant,
                    piece_index=piece_index,
                    triangle=triangle_index,
                    corner=corner,
                    value=index,
                    vertex_count=len(vertices),
                )
            triangles[triangle_index, corner] = index
        if len(set(triangle)) != 3:
            raise scene_error(
                "RW_E_DEGENERATE_TRIANGLE",
                f"triangle {triangle_index} repeats a vertex index",
                quadrant=quadrant,
                piece_index=piece_index,
                triangle=triangle_index,
            )
        a, b, c = vertices[triangles[triangle_index]]
        cross = np.cross(b - a, c - a)
        area2 = float(np.linalg.norm(cross))
        if area2 <= mesh_epsilon:
            raise scene_error(
                "RW_E_DEGENERATE_TRIANGLE",
                f"triangle {triangle_index} has area {area2 * 0.5:.3e}",
                quadrant=quadrant,
                piece_index=piece_index,
                triangle=triangle_index,
                area=area2 * 0.5,
            )

    color = _vec3(
        _get(raw_piece, "transmission_color", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].transmission_color",
    )
    if np.any(color < 0.0) or np.any(color > 1.0):
        raise scene_error(
            "RW_E_RANGE_COLOR",
            "transmission_color channels must be in [0, 1]",
            quadrant=quadrant,
            piece_index=piece_index,
        )

    transmittance = _finite_number(
        _get(raw_piece, "transmittance", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].transmittance",
    )
    if not 0.0 <= transmittance <= 1.0:
        raise scene_error(
            "RW_E_RANGE_TRANSMITTANCE",
            "transmittance must be in [0, 1]",
            quadrant=quadrant,
            piece_index=piece_index,
            value=transmittance,
        )

    lead_width = _finite_number(
        _get(raw_piece, "lead_width", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].lead_width",
    )
    lead_depth = _finite_number(
        _get(raw_piece, "lead_depth", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].lead_depth",
    )
    if lead_width < 0.0 or lead_depth < 0.0:
        raise scene_error(
            "RW_E_RANGE_LEAD",
            "lead_width and lead_depth must be non-negative",
            quadrant=quadrant,
            piece_index=piece_index,
        )

    reflectance = _finite_number(
        _get(raw_piece, "enamel_reflectance", f"glass_pieces[{raw_index}]"),
        f"glass_pieces[{raw_index}].enamel_reflectance",
    )
    if not 0.0 <= reflectance <= 1.0:
        raise scene_error(
            "RW_E_RANGE_REFLECTANCE",
            "enamel_reflectance must be in [0, 1]",
            quadrant=quadrant,
            piece_index=piece_index,
            value=reflectance,
        )

    normals = np.cross(
        vertices[triangles[:, 1]] - vertices[triangles[:, 0]],
        vertices[triangles[:, 2]] - vertices[triangles[:, 0]],
    )
    normal_raw = normals.sum(axis=0)
    normal_length = float(np.linalg.norm(normal_raw))
    if normal_length <= mesh_epsilon:
        raise scene_error(
            "RW_E_DEGENERATE_TRIANGLE",
            "combined mesh normal is degenerate",
            quadrant=quadrant,
            piece_index=piece_index,
        )
    normal = normal_raw / normal_length

    min_xy = vertices[:, :2].min(axis=0)
    max_xy = vertices[:, :2].max(axis=0)
    bounds_xy = (
        float(min_xy[0]),
        float(min_xy[1]),
        float(max_xy[0]),
        float(max_xy[1]),
    )
    return GlassPiece(
        serial=0,
        quadrant=quadrant,
        piece_index=piece_index,
        vertices=vertices,
        triangles=triangles,
        transmission_color=color,
        transmittance=transmittance,
        lead_width=lead_width,
        lead_depth=lead_depth,
        enamel_reflectance=reflectance,
        normal=normal,
        bounds_xy=bounds_xy,
    )


def _deduplicate_pieces(raw_pieces: Iterable[Any]) -> None:
    seen: dict[tuple[int, int], int] = {}
    for index, raw_piece in enumerate(raw_pieces):
        if not isinstance(raw_piece, dict):
            raise scene_error(
                "RW_E_SCHEMA", f"glass_pieces[{index}] must be an object"
            )
        quadrant = raw_piece.get("quadrant")
        piece_index = raw_piece.get("piece_index")
        if not isinstance(quadrant, int) or isinstance(quadrant, bool):
            continue
        if not isinstance(piece_index, int) or isinstance(piece_index, bool):
            continue
        key = (quadrant, piece_index)
        if key in seen:
            raise scene_error(
                "RW_E_DUPLICATE_PIECE",
                f"quadrant {quadrant}, piece {piece_index} appears more than once",
                quadrant=quadrant,
                piece_index=piece_index,
                first_index=seen[key],
                duplicate_index=index,
            )
        seen[key] = index


def _parse_scene(raw: Any) -> Scene:
    root = _object(raw, "scene")
    schema_version = _get(root, "schema_version", "scene")
    if schema_version != 1:
        raise scene_error(
            "RW_E_SCHEMA",
            f"unsupported schema_version {schema_version!r}; expected 1",
        )
    name = root.get("name", "rose-window")
    if not isinstance(name, str):
        raise scene_error("RW_E_SCHEMA", "scene.name must be a string")
    seed = _get(root, "seed", "scene")
    if not isinstance(seed, int) or isinstance(seed, bool) or seed < 0:
        raise scene_error("RW_E_SCHEMA", "scene.seed must be a non-negative integer")

    image = _object(_get(root, "image", "scene"), "scene.image")
    width = _positive_int(_get(image, "width", "scene.image"), "scene.image.width")
    height = _positive_int(_get(image, "height", "scene.image"), "scene.image.height")
    chunk_size = _positive_int(
        _get(image, "chunk_size", "scene.image"), "scene.image.chunk_size"
    )
    if width > 8192 or height > 8192:
        raise scene_error("RW_E_SCHEMA", "image dimensions must not exceed 8192")
    gamma = _finite_number(image.get("gamma", 2.2), "scene.image.gamma")
    if not 0.1 <= gamma <= 10.0:
        raise scene_error("RW_E_SCHEMA", "scene.image.gamma must be in [0.1, 10]")

    camera_raw = _object(_get(root, "camera", "scene"), "scene.camera")
    camera_position = _vec3(
        _get(camera_raw, "position", "scene.camera"), "scene.camera.position"
    )
    camera_target = _vec3(
        _get(camera_raw, "target", "scene.camera"), "scene.camera.target"
    )
    camera_up = _unit_vec3(
        camera_raw.get("up", [0.0, 1.0, 0.0]), "scene.camera.up"
    )
    camera_forward_raw = camera_target - camera_position
    if float(np.linalg.norm(camera_forward_raw)) <= 1e-9:
        raise scene_error(
            "RW_E_CAMERA",
            "camera.position and camera.target must differ",
        )
    camera_forward = camera_forward_raw / np.linalg.norm(camera_forward_raw)
    right_raw = np.cross(camera_forward, camera_up)
    if float(np.linalg.norm(right_raw)) <= 1e-9:
        raise scene_error("RW_E_CAMERA", "camera.up must not be parallel to view")
    camera_right = right_raw / np.linalg.norm(right_raw)
    camera_up_ortho = np.cross(camera_right, camera_forward)
    vertical_fov = _finite_number(
        camera_raw.get("vertical_fov_degrees", 55.0),
        "scene.camera.vertical_fov_degrees",
    )
    if not 1.0 < vertical_fov < 179.0:
        raise scene_error(
            "RW_E_CAMERA", "camera.vertical_fov_degrees must be in (1, 179)"
        )
    camera = Camera(
        position=camera_position,
        target=camera_target,
        up=camera_up,
        forward=camera_forward,
        right=camera_right,
        camera_up=camera_up_ortho,
        vertical_fov_degrees=vertical_fov,
    )

    light_raw = _object(_get(root, "light", "scene"), "scene.light")
    light = Light(
        direction=_unit_vec3(
            _get(light_raw, "direction", "scene.light"), "scene.light.direction"
        ),
        color=_vec3(light_raw.get("color", [1, 1, 1]), "scene.light.color"),
        intensity=_finite_number(
            _get(light_raw, "intensity", "scene.light"), "scene.light.intensity"
        ),
        angular_radius=_finite_number(
            light_raw.get("angular_radius", 0.0), "scene.light.angular_radius"
        ),
    )
    if np.any(light.color < 0.0):
        raise scene_error("RW_E_SCHEMA", "scene.light.color must be non-negative")
    if light.intensity < 0.0:
        raise scene_error("RW_E_SCHEMA", "scene.light.intensity must be non-negative")
    if light.angular_radius < 0.0 or light.angular_radius >= math.pi / 2:
        raise scene_error(
            "RW_E_SCHEMA", "scene.light.angular_radius must be in [0, pi/2)"
        )
    if abs(float(light.direction[2])) <= 1e-9:
        raise scene_error(
            "RW_E_GEOMETRY",
            "scene.light.direction must not be parallel to the wall plane",
        )

    wall_raw = _object(_get(root, "wall", "scene"), "scene.wall")
    wall_z = _finite_number(_get(wall_raw, "z", "scene.wall"), "scene.wall.z")
    wall_width = _finite_number(
        _get(wall_raw, "width", "scene.wall"), "scene.wall.width"
    )
    wall_height = _finite_number(
        _get(wall_raw, "height", "scene.wall"), "scene.wall.height"
    )
    if wall_width <= 0.0 or wall_height <= 0.0:
        raise scene_error("RW_E_GEOMETRY", "wall width and height must be positive")
    wall_center = wall_raw.get("center_xy", [0.0, 0.0])
    if not isinstance(wall_center, list) or len(wall_center) != 2:
        raise scene_error("RW_E_SCHEMA", "scene.wall.center_xy must have 2 numbers")
    block = wall_raw.get("block_size", [0.5, 0.3])
    if not isinstance(block, list) or len(block) != 2:
        raise scene_error("RW_E_SCHEMA", "scene.wall.block_size must have 2 numbers")
    block_width = _finite_number(block[0], "scene.wall.block_size[0]")
    block_height = _finite_number(block[1], "scene.wall.block_size[1]")
    if block_width <= 0.0 or block_height <= 0.0:
        raise scene_error("RW_E_GEOMETRY", "wall block sizes must be positive")
    wall = Wall(
        z=wall_z,
        width=wall_width,
        height=wall_height,
        center_x=_finite_number(wall_center[0], "scene.wall.center_xy[0]"),
        center_y=_finite_number(wall_center[1], "scene.wall.center_xy[1]"),
        color=_vec3(wall_raw.get("color", [0.34, 0.31, 0.27]), "scene.wall.color"),
        block_width=block_width,
        block_height=block_height,
    )
    if np.any(wall.color < 0.0) or np.any(wall.color > 1.0):
        raise scene_error("RW_E_RANGE_COLOR", "scene.wall.color must be in [0, 1]")

    window_raw = _object(_get(root, "window", "scene"), "scene.window")
    window_center = _vec3(
        window_raw.get("center", [0.0, 0.0, 0.0]), "scene.window.center"
    )
    window_normal = _unit_vec3(
        window_raw.get("normal", [0.0, 0.0, 1.0]), "scene.window.normal"
    )
    surround_outer = _finite_number(
        window_raw.get("surround_outer_radius", 2.2),
        "scene.window.surround_outer_radius",
    )
    if surround_outer <= 0.0:
        raise scene_error(
            "RW_E_GEOMETRY", "scene.window.surround_outer_radius must be positive"
        )

    texture_raw = _object(root.get("texture", {}), "scene.texture")
    texture = TextureSettings(
        size=_positive_int(texture_raw.get("size", 64), "scene.texture.size"),
        transmission_low=_finite_number(
            texture_raw.get("transmission_low", 0.82),
            "scene.texture.transmission_low",
        ),
        transmission_high=_finite_number(
            texture_raw.get("transmission_high", 1.12),
            "scene.texture.transmission_high",
        ),
        reflection_low=_finite_number(
            texture_raw.get("reflection_low", 0.78),
            "scene.texture.reflection_low",
        ),
        reflection_high=_finite_number(
            texture_raw.get("reflection_high", 1.22),
            "scene.texture.reflection_high",
        ),
    )
    if texture.size > 1024:
        raise scene_error("RW_E_SCHEMA", "scene.texture.size must not exceed 1024")
    if texture.transmission_low < 0 or texture.transmission_high < texture.transmission_low:
        raise scene_error("RW_E_SCHEMA", "invalid transmission texture range")
    if texture.reflection_low < 0 or texture.reflection_high < texture.reflection_low:
        raise scene_error("RW_E_SCHEMA", "invalid reflection texture range")

    shadow_raw = _object(root.get("shadow", {}), "scene.shadow")
    shadow = ShadowSettings(
        resolution=_positive_int(
            shadow_raw.get("resolution", 512), "scene.shadow.resolution"
        ),
        samples=_positive_int(
            shadow_raw.get("samples", 16), "scene.shadow.samples"
        ),
        penumbra_depth_scale=_finite_number(
            shadow_raw.get("penumbra_depth_scale", 1.25),
            "scene.shadow.penumbra_depth_scale",
        ),
        profile_depth_levels=_positive_int(
            shadow_raw.get("profile_depth_levels", 3),
            "scene.shadow.profile_depth_levels",
        ),
        profile_span=_finite_number(
            shadow_raw.get("profile_span", 0.36), "scene.shadow.profile_span"
        ),
    )
    if shadow.resolution > 4096:
        raise scene_error("RW_E_SCHEMA", "scene.shadow.resolution must not exceed 4096")
    if shadow.samples > 4096:
        raise scene_error("RW_E_SCHEMA", "scene.shadow.samples must not exceed 4096")
    if shadow.penumbra_depth_scale < 0 or shadow.profile_span <= 0:
        raise scene_error("RW_E_SCHEMA", "invalid shadow settings")

    render_raw = _object(root.get("render", {}), "scene.render")
    render = RenderSettings(
        ambient_transmission=_finite_number(
            render_raw.get("ambient_transmission", 0.18),
            "scene.render.ambient_transmission",
        ),
        exposure=_finite_number(
            render_raw.get("exposure", 1.0), "scene.render.exposure"
        ),
        wall_ambient=_finite_number(
            render_raw.get("wall_ambient", 0.20), "scene.render.wall_ambient"
        ),
        wall_direct=_finite_number(
            render_raw.get("wall_direct", 0.72), "scene.render.wall_direct"
        ),
        gamma=gamma,
    )
    if (
        render.ambient_transmission < 0
        or render.exposure <= 0
        or render.wall_ambient < 0
        or render.wall_direct < 0
    ):
        raise scene_error("RW_E_SCHEMA", "invalid scene.render values")

    reflection_raw = _object(root.get("reflection", {}), "scene.reflection")
    reflection = ReflectionSettings(
        environment_color=_vec3(
            reflection_raw.get("environment_color", [0.18, 0.24, 0.20]),
            "scene.reflection.environment_color",
        ),
        intensity=_finite_number(
            reflection_raw.get("intensity", 0.62), "scene.reflection.intensity"
        ),
        fresnel_power=_finite_number(
            reflection_raw.get("fresnel_power", 5.0),
            "scene.reflection.fresnel_power",
        ),
    )
    if np.any(reflection.environment_color < 0.0):
        raise scene_error(
            "RW_E_SCHEMA", "scene.reflection.environment_color must be non-negative"
        )
    if reflection.intensity < 0.0 or reflection.fresnel_power <= 0.0:
        raise scene_error("RW_E_SCHEMA", "invalid scene.reflection values")

    raw_pieces = _get(root, "glass_pieces", "scene")
    if not isinstance(raw_pieces, list) or not raw_pieces:
        raise scene_error("RW_E_SCHEMA", "scene.glass_pieces must be a non-empty array")
    _deduplicate_pieces(raw_pieces)

    quadrants_raw = root.get("quadrants")
    if quadrants_raw is not None:
        if not isinstance(quadrants_raw, list) or not quadrants_raw:
            raise scene_error("RW_E_SCHEMA", "scene.quadrants must be a non-empty array")
        seen_quadrants: set[int] = set()
        for quadrant in quadrants_raw:
            if (
                not isinstance(quadrant, int)
                or isinstance(quadrant, bool)
                or quadrant < 0
            ):
                raise scene_error(
                    "RW_E_SCHEMA", "scene.quadrants entries must be non-negative integers"
                )
            if quadrant in seen_quadrants:
                raise scene_error(
                    "RW_E_DUPLICATE_QUADRANT",
                    f"quadrant {quadrant} is declared more than once",
                    quadrant=quadrant,
                )
            seen_quadrants.add(quadrant)
    else:
        seen_quadrants = set()

    mesh_epsilon = _finite_number(
        root.get("mesh_epsilon", 1e-12), "scene.mesh_epsilon"
    )
    if mesh_epsilon <= 0:
        raise scene_error("RW_E_SCHEMA", "scene.mesh_epsilon must be positive")

    pieces: list[GlassPiece] = []
    for index, raw_piece in enumerate(raw_pieces):
        try:
            pieces.append(_parse_piece(raw_piece, index, mesh_epsilon))
        except SceneValidationError as exc:
            quadrant = raw_piece.get("quadrant")
            piece_index = raw_piece.get("piece_index")
            if exc.quadrant is None and isinstance(quadrant, int) and not isinstance(
                quadrant, bool
            ):
                exc.quadrant = quadrant
            if (
                exc.piece_index is None
                and isinstance(piece_index, int)
                and not isinstance(piece_index, bool)
            ):
                exc.piece_index = piece_index
            raise
    if seen_quadrants:
        for piece in pieces:
            if piece.quadrant not in seen_quadrants:
                raise scene_error(
                    "RW_E_QUADRANT_UNKNOWN",
                    f"quadrant {piece.quadrant} is not present in scene.quadrants",
                    quadrant=piece.quadrant,
                    piece_index=piece.piece_index,
                )

    pieces.sort(key=lambda item: (item.quadrant, item.piece_index))
    numbered_pieces = tuple(
        GlassPiece(
            serial=index + 1,
            quadrant=piece.quadrant,
            piece_index=piece.piece_index,
            vertices=piece.vertices,
            triangles=piece.triangles,
            transmission_color=piece.transmission_color,
            transmittance=piece.transmittance,
            lead_width=piece.lead_width,
            lead_depth=piece.lead_depth,
            enamel_reflectance=piece.enamel_reflectance,
            normal=piece.normal,
            bounds_xy=piece.bounds_xy,
        )
        for index, piece in enumerate(pieces)
    )

    return Scene(
        raw=root,
        digest=scene_digest(root),
        schema_version=1,
        name=name,
        seed=seed,
        width=width,
        height=height,
        chunk_size=chunk_size,
        camera=camera,
        light=light,
        wall=wall,
        window_center=window_center,
        window_normal=window_normal,
        surround_outer_radius=surround_outer,
        pieces=numbered_pieces,
        textures=texture,
        shadows=shadow,
        render=render,
        reflection=reflection,
    )


def parse_scene(raw: Any) -> Scene:
    try:
        return _parse_scene(raw)
    except SceneValidationError:
        raise
    except (KeyError, TypeError, ValueError, OverflowError) as exc:
        raise scene_error("RW_E_SCHEMA", f"invalid scene JSON: {exc}") from exc


def load_scene(path: str | Path) -> Scene:
    scene_path = Path(path)
    try:
        with scene_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except json.JSONDecodeError as exc:
        raise scene_error(
            "RW_E_JSON",
            f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
        ) from exc
    except OSError as exc:
        raise scene_error("RW_E_IO", f"cannot read scene {scene_path}: {exc}") from exc
    return parse_scene(raw)
