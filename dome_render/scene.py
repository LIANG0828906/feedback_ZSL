"""Scene JSON parsing, deterministic validation, and derived data."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SceneIssue, ValidationFailure
from .math3d import (
    as_vec3,
    camera_basis,
    finite_float,
    mesh_is_closed,
    normalize,
    point_in_closed_mesh,
    triangle_areas,
    triangle_normals,
)

SCHEMA_VERSION = 1
LAYER_PLANE_EPS = 1.0e-6
UNIT_EPS = 1.0e-6
DEGENERATE_AREA_EPS = 1.0e-12


def _derive_planar_uvs(vertices: np.ndarray, normal: np.ndarray) -> np.ndarray:
    """Derive stable 0..1 planar UVs when a board omits explicit UVs."""
    variances = np.var(vertices, axis=0)
    ordered = sorted(range(3), key=lambda axis: (-float(variances[axis]), axis))
    keep = ordered[:2]
    u = vertices[:, keep[0]]
    v = vertices[:, keep[1]]
    u_span = float(np.max(u) - np.min(u))
    v_span = float(np.max(v) - np.min(v))
    u_norm = (u - float(np.min(u))) / max(u_span, 1.0e-15)
    v_norm = (v - float(np.min(v))) / max(v_span, 1.0e-15)
    return np.stack((u_norm, v_norm), axis=1)


def _affine_uv_residual(vertices: np.ndarray, uvs: np.ndarray) -> tuple[float, int]:
    design = np.column_stack((uvs, np.ones(len(uvs), dtype=np.float64)))
    coefficients, _, rank, _ = np.linalg.lstsq(design, vertices, rcond=None)
    residual = float(np.max(np.linalg.norm(design @ coefficients - vertices, axis=1)))
    return residual, int(rank)


@dataclass
class Mesh:
    vertices: np.ndarray
    triangles: np.ndarray
    normals: np.ndarray
    areas: np.ndarray
    closed: bool
    uvs: np.ndarray | None
    location: str


@dataclass
class Rib:
    rib_id: int
    mesh: Mesh
    reflectance: float
    anisotropy: float
    roughness: float
    base_color: np.ndarray
    location: str


@dataclass
class Board:
    board_id: int
    mesh: Mesh
    transmission: float
    texture_seed: int
    resolution: tuple[int, int]
    tint: np.ndarray
    thickness: float
    location: str


@dataclass
class Layer:
    index: int
    ribs: list[Rib]
    boards: list[Board]
    normal: np.ndarray
    plane_point: np.ndarray
    location: str


@dataclass
class Light:
    light_id: int
    direction: np.ndarray
    irradiance: np.ndarray
    angular_radius: float
    source_distance: float
    distance_softness: float
    location: str


@dataclass
class Wall:
    point: np.ndarray
    normal: np.ndarray
    u_axis: np.ndarray
    v_axis: np.ndarray
    half_size: np.ndarray
    color: np.ndarray
    emission: np.ndarray
    shadow_floor: float
    resolution: tuple[int, int]


@dataclass
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    near: float
    far: float
    forward: np.ndarray = field(init=False)
    right: np.ndarray = field(init=False)
    true_up: np.ndarray = field(init=False)
    tan_half_fov: float = field(init=False)

    def __post_init__(self) -> None:
        self.forward, self.right, self.true_up = camera_basis(self.position, self.target, self.up)
        self.tan_half_fov = math.tan(math.radians(self.fov_y_deg) * 0.5)


@dataclass
class RenderConfig:
    width: int
    height: int
    block_size: tuple[int, int]
    supersample: int
    shadow_samples: int
    projection_density: int
    exposure: float
    background: np.ndarray

    @property
    def blocks_x(self) -> int:
        return (self.width + self.block_size[0] - 1) // self.block_size[0]

    @property
    def blocks_y(self) -> int:
        return (self.height + self.block_size[1] - 1) // self.block_size[1]

    @property
    def block_count(self) -> int:
        return self.blocks_x * self.blocks_y

    def block_rect(self, block_id: int) -> tuple[int, int, int, int]:
        bx = block_id % self.blocks_x
        by = block_id // self.blocks_x
        x0 = bx * self.block_size[0]
        y0 = by * self.block_size[1]
        x1 = min(self.width, x0 + self.block_size[0])
        y1 = min(self.height, y0 + self.block_size[1])
        return x0, y0, x1 - x0, y1 - y0


@dataclass
class Scene:
    raw: dict[str, Any]
    renderer: RenderConfig
    camera: Camera
    lights: list[Light]
    layers: list[Layer]
    wall: Wall
    canonical_json: str
    digest: str
    summary: dict[str, Any]

    @property
    def sample_offsets(self) -> list[tuple[float, float]]:
        n = self.renderer.supersample
        return [((i + 0.5) / n, (j + 0.5) / n) for j in range(n) for i in range(n)]


class _Validator:
    def __init__(self, raw: Any, source: str):
        self.raw = raw
        self.source = source
        self.issues: list[SceneIssue] = []

    def issue(self, code: str, location: str, field_name: str, message: str, value: Any | None = None) -> None:
        self.issues.append(SceneIssue(code, location, field_name, message, value))

    def obj(self, value: Any, location: str, field_name: str) -> dict[str, Any] | None:
        if not isinstance(value, dict):
            self.issue("E_OBJECT_REQUIRED", location, field_name, "Expected a JSON object.")
            return None
        return value

    def array(self, value: Any, location: str, field_name: str) -> list[Any] | None:
        if not isinstance(value, list):
            self.issue("E_ARRAY_REQUIRED", location, field_name, "Expected a JSON array.")
            return None
        return value

    def vec3(self, value: Any, location: str, field_name: str, *, unit: bool = False) -> np.ndarray | None:
        if not isinstance(value, list) or len(value) != 3 or not all(finite_float(v) for v in value):
            self.issue("E_VECTOR3_INVALID", location, field_name, "Expected three finite numbers.", value)
            return None
        vec = as_vec3(value)
        if unit and abs(float(np.linalg.norm(vec)) - 1.0) > UNIT_EPS:
            self.issue("E_DIRECTION_NOT_UNIT", location, field_name, "Direction light vector must be unit length.", value)
        return vec

    def number(self, value: Any, location: str, field_name: str, *, low: float | None = None, high: float | None = None) -> float | None:
        if not finite_float(value):
            self.issue("E_NUMBER_INVALID", location, field_name, "Expected a finite number.", value)
            return None
        number = float(value)
        if low is not None and number < low:
            self.issue("E_NUMBER_RANGE", location, field_name, f"Value must be >= {low}.", value)
        if high is not None and number > high:
            self.issue("E_NUMBER_RANGE", location, field_name, f"Value must be <= {high}.", value)
        return number

    def integer(self, value: Any, location: str, field_name: str, *, low: int | None = None) -> int | None:
        if isinstance(value, bool) or not isinstance(value, int):
            self.issue("E_INTEGER_INVALID", location, field_name, "Expected an integer.", value)
            return None
        if low is not None and value < low:
            self.issue("E_INTEGER_RANGE", location, field_name, f"Value must be >= {low}.", value)
        return value

    def mesh(self, value: Any, location: str, *, need_uv: bool) -> Mesh | None:
        mesh_obj = self.obj(value, location, "mesh")
        if mesh_obj is None:
            return None
        vertices_raw = self.array(mesh_obj.get("vertices"), location + ".vertices", "vertices")
        triangles_raw = self.array(mesh_obj.get("triangles"), location + ".triangles", "triangles")
        if vertices_raw is None or triangles_raw is None:
            return None
        if not vertices_raw:
            self.issue("E_MESH_EMPTY", location + ".vertices", "vertices", "Mesh must contain vertices.")
            return None
        if not triangles_raw:
            self.issue("E_MESH_EMPTY", location + ".triangles", "triangles", "Mesh must contain triangles.")
            return None
        try:
            vertices = np.asarray(vertices_raw, dtype=np.float64)
        except (TypeError, ValueError):
            self.issue("E_VERTICES_INVALID", location + ".vertices", "vertices", "Vertices must be numeric N x 3 arrays.")
            return None
        if vertices.ndim != 2 or vertices.shape[1] != 3 or not np.isfinite(vertices).all():
            self.issue("E_VERTICES_INVALID", location + ".vertices", "vertices", "Vertices must be finite N x 3 numbers.")
            return None
        triangles = np.empty((0, 3), dtype=np.int64)
        try:
            triangles = np.asarray(triangles_raw, dtype=np.int64)
        except (TypeError, ValueError):
            self.issue("E_TRIANGLES_INVALID", location + ".triangles", "triangles", "Triangles must be integer index triples.")
            return None
        if triangles.ndim != 2 or triangles.shape[1] != 3:
            self.issue("E_TRIANGLES_INVALID", location + ".triangles", "triangles", "Triangles must be M x 3 indices.")
            return None
        invalid_index = False
        for index, triangle in enumerate(triangles):
            for corner, vertex_index in enumerate(triangle):
                if int(vertex_index) < 0 or int(vertex_index) >= len(vertices):
                    self.issue(
                        "E_MESH_INDEX_OUT_OF_RANGE",
                        f"{location}.triangles[{index}][{corner}]",
                        f"triangles[{index}][{corner}]",
                        "Triangle index is outside the vertex array.",
                        int(vertex_index),
                    )
                    invalid_index = True
        if invalid_index:
            return None
        areas = triangle_areas(vertices, triangles)
        for index, area in enumerate(areas):
            if not math.isfinite(float(area)) or float(area) <= DEGENERATE_AREA_EPS:
                self.issue(
                    "E_TRIANGLE_DEGENERATE",
                    f"{location}.triangles[{index}]",
                    f"triangles[{index}]",
                    "Triangle has zero or near-zero area.",
                )
        uvs: np.ndarray | None = None
        if "uvs" in mesh_obj:
            uvs_raw = self.array(mesh_obj.get("uvs"), location + ".uvs", "uvs")
            if uvs_raw is not None:
                try:
                    uvs = np.asarray(uvs_raw, dtype=np.float64)
                except (TypeError, ValueError):
                    uvs = None
                if uvs is None or uvs.ndim != 2 or uvs.shape[1] != 2 or len(uvs) != len(vertices) or not np.isfinite(uvs).all():
                    self.issue("E_UV_INVALID", location + ".uvs", "uvs", "UV array must be finite N x 2 and match vertices.")
                    uvs = None
        elif need_uv:
            uvs = None
        normals = triangle_normals(vertices, triangles)
        return Mesh(
            vertices=vertices,
            triangles=triangles,
            normals=normals,
            areas=areas,
            closed=mesh_is_closed(triangles),
            uvs=uvs,
            location=location,
        )

    def parse(self) -> Scene:
        raw_obj = self.obj(self.raw, "$", "$")
        if raw_obj is None:
            raise ValidationFailure(self.issues)
        if raw_obj.get("schema_version") != SCHEMA_VERSION:
            self.issue("E_SCHEMA_VERSION", "$.schema_version", "schema_version", f"Expected schema version {SCHEMA_VERSION}.", raw_obj.get("schema_version"))

        renderer_obj = self.obj(raw_obj.get("renderer"), "$.renderer", "renderer")
        config = self.parse_renderer(renderer_obj or {})
        camera = self.parse_camera(raw_obj.get("camera"))
        lights = self.parse_lights(raw_obj.get("lights"))
        wall = self.parse_wall(raw_obj.get("dome", {}).get("inner_wall") if isinstance(raw_obj.get("dome"), dict) else None)
        layers = self.parse_layers(raw_obj.get("dome", {}).get("layers") if isinstance(raw_obj.get("dome"), dict) else None)

        if layers:
            for layer in layers:
                for light in lights:
                    if abs(float(np.dot(light.direction, layer.normal))) <= LAYER_PLANE_EPS:
                        self.issue(
                            "E_LIGHT_PARALLEL_LAYER",
                            f"{light.location}.direction",
                            "direction",
                            f"Direction light is parallel to layer {layer.index} plane.",
                            light.direction.tolist(),
                        )

        if camera is not None:
            for layer in layers:
                for rib in layer.ribs:
                    if rib.mesh.closed and point_in_closed_mesh(camera.position, rib.mesh.vertices, rib.mesh.triangles):
                        self.issue(
                            "E_CAMERA_INSIDE_GEOMETRY",
                            "$.camera.position",
                            "position",
                            f"Camera is inside closed rib geometry {layer.location}.ribs[{rib.rib_id}].mesh.",
                            camera.position.tolist(),
                        )
                for board in layer.boards:
                    if board.mesh.closed and point_in_closed_mesh(camera.position, board.mesh.vertices, board.mesh.triangles):
                        self.issue(
                            "E_CAMERA_INSIDE_GEOMETRY",
                            "$.camera.position",
                            "position",
                            f"Camera is inside closed star-board geometry {board.location}.mesh.",
                            camera.position.tolist(),
                        )
            if wall is not None:
                signed = float(np.dot(camera.position - wall.point, wall.normal))
                if abs(signed) <= 1.0e-7:
                    relative = camera.position - wall.point
                    if abs(float(np.dot(relative, wall.u_axis))) <= wall.half_size[0] + 1.0e-7 and abs(float(np.dot(relative, wall.v_axis))) <= wall.half_size[1] + 1.0e-7:
                        self.issue("E_CAMERA_INSIDE_GEOMETRY", "$.camera.position", "position", "Camera lies on the dome inner-wall plane.", camera.position.tolist())

        if self.issues:
            raise ValidationFailure(self.issues)
        assert config is not None and camera is not None and wall is not None
        canonical = json.dumps(raw_obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False)
        import hashlib

        digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
        summary = {
            "digest": digest,
            "schema_version": SCHEMA_VERSION,
            "image": {
                "width": config.width,
                "height": config.height,
                "block_size": [config.block_size[0], config.block_size[1]],
                "blocks_x": config.blocks_x,
                "blocks_y": config.blocks_y,
                "block_count": config.block_count,
            },
            "camera": {
                "position": [round(float(v), 9) for v in camera.position],
                "target": [round(float(v), 9) for v in camera.target],
            },
            "layers": [
                {
                    "index": layer.index,
                    "rib_ids": [rib.rib_id for rib in layer.ribs],
                    "board_ids": [board.board_id for board in layer.boards],
                }
                for layer in layers
            ],
            "lights": [
                {
                    "id": light.light_id,
                    "direction": [round(float(v), 9) for v in light.direction],
                    "irradiance": [round(float(v), 9) for v in light.irradiance],
                }
                for light in lights
            ],
        }
        return Scene(
            raw=raw_obj,
            renderer=config,
            camera=camera,
            lights=lights,
            layers=layers,
            wall=wall,
            canonical_json=canonical,
            digest=digest,
            summary=summary,
        )

    def parse_renderer(self, renderer_obj: dict[str, Any]) -> RenderConfig | None:
        location = "$.renderer"
        width = self.integer(renderer_obj.get("width"), location + ".width", "width", low=1)
        height = self.integer(renderer_obj.get("height"), location + ".height", "height", low=1)
        block_raw = self.array(renderer_obj.get("block_size"), location + ".block_size", "block_size")
        block_size = (0, 0)
        if block_raw is not None:
            if len(block_raw) != 2:
                self.issue("E_BLOCK_SIZE", location + ".block_size", "block_size", "block_size must have two integers.")
            else:
                bw = self.integer(block_raw[0], location + ".block_size[0]", "block_size[0]", low=1)
                bh = self.integer(block_raw[1], location + ".block_size[1]", "block_size[1]", low=1)
                if bw is not None and bh is not None:
                    block_size = (bw, bh)
        sampling_obj = self.obj(renderer_obj.get("sampling", {}), location + ".sampling", "sampling") or {}
        supersample = self.integer(sampling_obj.get("supersample", 1), location + ".sampling.supersample", "supersample", low=1)
        shadow_samples = self.integer(sampling_obj.get("shadow_samples", 8), location + ".sampling.shadow_samples", "shadow_samples", low=1)
        projection_density = self.integer(sampling_obj.get("projection_density", 1), location + ".sampling.projection_density", "projection_density", low=1)
        exposure = self.number(renderer_obj.get("exposure", 1.0), location + ".exposure", "exposure", low=1.0e-9)
        background_raw = renderer_obj.get("background", [0.0, 0.0, 0.0, 0.0])
        background = None
        if not isinstance(background_raw, list) or len(background_raw) != 4 or not all(finite_float(v) for v in background_raw):
            self.issue("E_BACKGROUND_INVALID", location + ".background", "background", "background must be four finite numbers.", background_raw)
        else:
            background = np.asarray(background_raw, dtype=np.float64)
        if width is None or height is None or supersample is None or shadow_samples is None or projection_density is None or exposure is None or background is None:
            return None
        if supersample is not None and supersample > 8:
            self.issue("E_SAMPLING_RANGE", location + ".sampling.supersample", "supersample", "supersample must be <= 8.")
        if shadow_samples is not None and shadow_samples > 128:
            self.issue("E_SAMPLING_RANGE", location + ".sampling.shadow_samples", "shadow_samples", "shadow_samples must be <= 128.")
        if projection_density is not None and projection_density > 8:
            self.issue("E_SAMPLING_RANGE", location + ".sampling.projection_density", "projection_density", "projection_density must be <= 8.")
        return RenderConfig(
            width=width,
            height=height,
            block_size=block_size,
            supersample=supersample,
            shadow_samples=shadow_samples,
            projection_density=projection_density,
            exposure=exposure,
            background=background,
        )

    def parse_camera(self, value: Any) -> Camera | None:
        location = "$.camera"
        camera_obj = self.obj(value, location, "camera")
        if camera_obj is None:
            return None
        position = self.vec3(camera_obj.get("position"), location + ".position", "position")
        target = self.vec3(camera_obj.get("target"), location + ".target", "target")
        up = self.vec3(camera_obj.get("up"), location + ".up", "up")
        fov = self.number(camera_obj.get("fov_y_deg"), location + ".fov_y_deg", "fov_y_deg", low=1.0e-6, high=179.999999)
        near = self.number(camera_obj.get("near"), location + ".near", "near", low=1.0e-9)
        far = self.number(camera_obj.get("far"), location + ".far", "far", low=1.0e-9)
        if position is None or target is None or up is None or fov is None or near is None or far is None:
            return None
        forward = target - position
        if float(np.linalg.norm(forward)) <= 1.0e-9:
            self.issue("E_CAMERA_LOOK", location + ".target", "target", "Camera target must differ from position.", target.tolist())
            return None
        if float(np.linalg.norm(up)) <= 1.0e-9:
            self.issue("E_CAMERA_UP", location + ".up", "up", "Camera up vector must be non-zero.", up.tolist())
            return None
        if float(np.linalg.norm(np.cross(forward, up))) <= 1.0e-9:
            self.issue("E_CAMERA_UP", location + ".up", "up", "Camera up vector must not be parallel to view direction.", up.tolist())
            return None
        if far <= near:
            self.issue("E_CAMERA_CLIP", location + ".far", "far", "far must be greater than near.", far)
            return None
        return Camera(position=position, target=target, up=up, fov_y_deg=fov, near=near, far=far)

    def parse_lights(self, value: Any) -> list[Light]:
        location = "$.lights"
        lights_raw = self.array(value, location, "lights")
        if lights_raw is None:
            return []
        if len(lights_raw) != 3:
            self.issue("E_LIGHT_COUNT", location, "lights", "Exactly three direction lights are required.", len(lights_raw))
        lights: list[Light] = []
        seen_ids: set[int] = set()
        for index, raw_light in enumerate(lights_raw):
            item_loc = f"{location}[{index}]"
            light_obj = self.obj(raw_light, item_loc, f"lights[{index}]")
            if light_obj is None:
                continue
            light_id = self.integer(light_obj.get("id", index), item_loc + ".id", "id", low=0)
            direction_raw = light_obj.get("direction")
            direction = self.vec3(direction_raw, item_loc + ".direction", "direction", unit=True)
            irradiance = self.vec3(light_obj.get("irradiance"), item_loc + ".irradiance", "irradiance")
            if irradiance is not None and np.any(irradiance < 0.0):
                self.issue("E_IRRADIANCE_NEGATIVE", item_loc + ".irradiance", "irradiance", "Irradiance must be non-negative.", irradiance.tolist())
            angular_deg = self.number(light_obj.get("angular_radius_deg", 2.0), item_loc + ".angular_radius_deg", "angular_radius_deg", low=0.0, high=89.0)
            source_distance = self.number(light_obj.get("source_distance", 1.0), item_loc + ".source_distance", "source_distance", low=1.0e-9)
            distance_softness = self.number(light_obj.get("distance_softness", 0.0), item_loc + ".distance_softness", "distance_softness", low=0.0)
            if light_id is not None and light_id in seen_ids:
                self.issue("E_LIGHT_ID_DUPLICATE", item_loc + ".id", "id", "Light id is duplicated.", light_id)
            if light_id is not None:
                seen_ids.add(light_id)
            if direction is not None and irradiance is not None and angular_deg is not None and source_distance is not None and distance_softness is not None and light_id is not None:
                lights.append(
                    Light(
                        light_id=light_id,
                        direction=direction,
                        irradiance=irradiance,
                        angular_radius=math.radians(angular_deg),
                        source_distance=source_distance,
                        distance_softness=distance_softness,
                        location=item_loc,
                    )
                )
        lights.sort(key=lambda item: item.light_id)
        return lights

    def parse_layers(self, value: Any) -> list[Layer]:
        location = "$.dome.layers"
        layers_raw = self.array(value, location, "layers")
        if layers_raw is None:
            return []
        if not layers_raw:
            self.issue("E_LAYER_EMPTY", location, "layers", "At least one dome layer is required.")
            return []
        layers: list[Layer] = []
        seen_layers: set[int] = set()
        for list_index, raw_layer in enumerate(layers_raw):
            item_loc = f"{location}[{list_index}]"
            layer_obj = self.obj(raw_layer, item_loc, f"layers[{list_index}]")
            if layer_obj is None:
                continue
            layer_index = self.integer(layer_obj.get("index"), item_loc + ".index", "index", low=0)
            if layer_index is not None and layer_index in seen_layers:
                self.issue("E_LAYER_INDEX_DUPLICATE", item_loc + ".index", "index", "Layer index is duplicated.", layer_index)
            if layer_index is not None:
                seen_layers.add(layer_index)
            ribs = self.parse_ribs(layer_obj.get("ribs"), item_loc + ".ribs")
            boards = self.parse_boards(layer_obj.get("star_boards"), item_loc + ".star_boards")
            if not ribs:
                self.issue("E_RIB_MISSING", item_loc + ".ribs", "ribs", "Each layer must contain at least one rib.")
            if not boards:
                self.issue("E_BOARD_MISSING", item_loc + ".star_boards", "star_boards", "Each layer must contain at least one star board.")
            if not ribs or not boards or layer_index is None:
                continue
            normal = self.layer_normal(boards, item_loc)
            plane_point = boards[0].mesh.vertices[0].copy()
            if normal is not None:
                layers.append(
                    Layer(
                        index=layer_index,
                        ribs=ribs,
                        boards=boards,
                        normal=normal,
                        plane_point=plane_point,
                        location=item_loc,
                    )
                )
        if seen_layers and seen_layers != set(range(max(seen_layers) + 1)):
            missing = sorted(set(range(max(seen_layers) + 1)) - seen_layers)
            for index in missing:
                self.issue("E_LAYER_INDEX_MISSING", location, "layers", f"Layer index {index} is missing.", index)
        layers.sort(key=lambda item: item.index)
        return layers

    def layer_normal(self, boards: list[Board], layer_location: str) -> np.ndarray | None:
        reference = boards[0]
        reference_normal = normalize(reference.mesh.normals[0])
        point = reference.mesh.vertices[0]
        valid = True
        for board in boards:
            deviations = np.abs((board.mesh.vertices - point) @ reference_normal)
            if float(np.max(deviations)) > 1.0e-6:
                self.issue(
                    "E_LAYER_NOT_PLANAR",
                    board.location + ".mesh.vertices",
                    "vertices",
                    f"Star board vertices are not coplanar with layer {layer_location}.",
                )
                valid = False
            if float(np.min(np.abs(board.mesh.normals @ reference_normal))) < 1.0 - 1.0e-7:
                self.issue(
                    "E_LAYER_NOT_PLANAR",
                    board.location + ".mesh.triangles",
                    "triangles",
                    "All star-board triangles in one layer must share a plane orientation.",
                )
                valid = False
        return reference_normal if valid else None

    def parse_ribs(self, value: Any, location: str) -> list[Rib]:
        ribs_raw = self.array(value, location, "ribs")
        if ribs_raw is None:
            return []
        ribs: list[Rib] = []
        seen: set[int] = set()
        for index, raw_rib in enumerate(ribs_raw):
            item_loc = f"{location}[{index}]"
            rib_obj = self.obj(raw_rib, item_loc, f"ribs[{index}]")
            if rib_obj is None:
                continue
            rib_id = self.integer(rib_obj.get("id"), item_loc + ".id", "id", low=0)
            if rib_id is not None and rib_id in seen:
                self.issue("E_RIB_ID_DUPLICATE", item_loc + ".id", "id", "Rib id is duplicated within its layer.", rib_id)
            if rib_id is not None:
                seen.add(rib_id)
            mesh = self.mesh(rib_obj.get("mesh"), item_loc + ".mesh", need_uv=False)
            material_obj = self.obj(rib_obj.get("material"), item_loc + ".material", "material")
            if mesh is None or material_obj is None or rib_id is None:
                continue
            reflectance = self.number(material_obj.get("reflectance"), item_loc + ".material.reflectance", "reflectance", low=0.0, high=1.0)
            anisotropy = self.number(material_obj.get("anisotropy"), item_loc + ".material.anisotropy", "anisotropy", low=-1.0, high=1.0)
            roughness = self.number(material_obj.get("roughness", 0.3), item_loc + ".material.roughness", "roughness", low=0.01, high=1.0)
            base_raw = material_obj.get("base_color", [1.0, 0.72, 0.28])
            if not isinstance(base_raw, list) or len(base_raw) != 3 or not all(finite_float(v) for v in base_raw) or any(float(v) < 0.0 for v in base_raw):
                self.issue("E_COLOR_INVALID", item_loc + ".material.base_color", "base_color", "base_color must be three non-negative finite numbers.", base_raw)
                continue
            if reflectance is None or anisotropy is None or roughness is None:
                continue
            ribs.append(
                Rib(
                    rib_id=rib_id,
                    mesh=mesh,
                    reflectance=reflectance,
                    anisotropy=anisotropy,
                    roughness=roughness,
                    base_color=np.asarray(base_raw, dtype=np.float64),
                    location=item_loc,
                )
            )
        if seen and seen != set(range(max(seen) + 1)):
            for missing in sorted(set(range(max(seen) + 1)) - seen):
                self.issue("E_RIB_ID_MISSING", location, "ribs", f"Rib id {missing} is missing.", missing)
        ribs.sort(key=lambda item: item.rib_id)
        return ribs

    def parse_boards(self, value: Any, location: str) -> list[Board]:
        boards_raw = self.array(value, location, "star_boards")
        if boards_raw is None:
            return []
        boards: list[Board] = []
        seen: set[int] = set()
        for index, raw_board in enumerate(boards_raw):
            item_loc = f"{location}[{index}]"
            board_obj = self.obj(raw_board, item_loc, f"star_boards[{index}]")
            if board_obj is None:
                continue
            board_id = self.integer(board_obj.get("id"), item_loc + ".id", "id", low=0)
            if board_id is not None and board_id in seen:
                self.issue("E_BOARD_ID_DUPLICATE", item_loc + ".id", "id", "Star-board id is duplicated within its layer.", board_id)
            if board_id is not None:
                seen.add(board_id)
            mesh = self.mesh(board_obj.get("mesh"), item_loc + ".mesh", need_uv=True)
            material_obj = self.obj(board_obj.get("material"), item_loc + ".material", "material")
            if mesh is None or material_obj is None or board_id is None:
                continue
            if mesh.uvs is None:
                mesh.uvs = _derive_planar_uvs(mesh.vertices, mesh.normals[0])
            residual, uv_rank = _affine_uv_residual(mesh.vertices, mesh.uvs)
            if uv_rank < 3 or residual > 1.0e-6:
                self.issue(
                    "E_BOARD_UV_NOT_AFFINE",
                    item_loc + ".mesh.uvs",
                    "uvs",
                    "Star-board UV mapping must be a non-degenerate affine mapping on its plane.",
                    {"rank": uv_rank, "residual": round(residual, 12)},
                )
            transmission = self.number(material_obj.get("transmission"), item_loc + ".material.transmission", "transmission", low=0.0, high=1.0)
            seed = self.integer(material_obj.get("texture_seed"), item_loc + ".material.texture_seed", "texture_seed")
            resolution_raw = self.array(material_obj.get("resolution"), item_loc + ".material.resolution", "resolution")
            resolution: tuple[int, int] | None = None
            if resolution_raw is not None:
                if len(resolution_raw) != 2:
                    self.issue("E_STAR_RESOLUTION", item_loc + ".material.resolution", "resolution", "resolution must contain two integers.")
                else:
                    rw = self.integer(resolution_raw[0], item_loc + ".material.resolution[0]", "resolution[0]", low=1)
                    rh = self.integer(resolution_raw[1], item_loc + ".material.resolution[1]", "resolution[1]", low=1)
                    if rw is not None and rh is not None:
                        resolution = (rw, rh)
            tint_raw = material_obj.get("tint", [0.82, 0.9, 1.0])
            if not isinstance(tint_raw, list) or len(tint_raw) != 3 or not all(finite_float(v) for v in tint_raw) or any(float(v) < 0.0 for v in tint_raw):
                self.issue("E_COLOR_INVALID", item_loc + ".material.tint", "tint", "tint must be three non-negative finite numbers.", tint_raw)
                continue
            thickness = self.number(board_obj.get("thickness", 0.1), item_loc + ".thickness", "thickness", low=1.0e-9)
            if transmission is None or seed is None or resolution is None or thickness is None:
                continue
            boards.append(
                Board(
                    board_id=board_id,
                    mesh=mesh,
                    transmission=transmission,
                    texture_seed=seed,
                    resolution=resolution,
                    tint=np.asarray(tint_raw, dtype=np.float64),
                    thickness=thickness,
                    location=item_loc,
                )
            )
        if seen and seen != set(range(max(seen) + 1)):
            for missing in sorted(set(range(max(seen) + 1)) - seen):
                self.issue("E_BOARD_ID_MISSING", location, "star_boards", f"Star-board id {missing} is missing.", missing)
        boards.sort(key=lambda item: item.board_id)
        return boards

    def parse_wall(self, value: Any) -> Wall | None:
        location = "$.dome.inner_wall"
        wall_obj = self.obj(value, location, "inner_wall")
        if wall_obj is None:
            return None
        plane_obj = self.obj(wall_obj.get("plane"), location + ".plane", "plane")
        if plane_obj is None:
            return None
        point = self.vec3(plane_obj.get("point"), location + ".plane.point", "point")
        normal_raw = self.vec3(plane_obj.get("normal"), location + ".plane.normal", "normal")
        if point is None or normal_raw is None:
            return None
        if float(np.linalg.norm(normal_raw)) <= 1.0e-9:
            self.issue("E_WALL_PLANE", location + ".plane.normal", "normal", "Wall plane normal must be non-zero.", normal_raw.tolist())
            return None
        normal = normalize(normal_raw)
        u_axis = self.vec3(wall_obj.get("u_axis", [1.0, 0.0, 0.0]), location + ".u_axis", "u_axis")
        v_axis = self.vec3(wall_obj.get("v_axis", [0.0, 1.0, 0.0]), location + ".v_axis", "v_axis")
        if u_axis is None or v_axis is None:
            return None
        u_axis = u_axis - normal * float(np.dot(u_axis, normal))
        v_axis = v_axis - normal * float(np.dot(v_axis, normal))
        if float(np.linalg.norm(u_axis)) <= 1.0e-9 or float(np.linalg.norm(v_axis)) <= 1.0e-9:
            self.issue("E_WALL_PLANE", location, "u_axis", "Wall u/v axes must not be parallel to the normal.")
            return None
        u_axis = normalize(u_axis)
        v_axis = v_axis - u_axis * float(np.dot(v_axis, u_axis))
        if float(np.linalg.norm(v_axis)) <= 1.0e-9:
            self.issue("E_WALL_PLANE", location, "v_axis", "Wall axes must be independent.")
            return None
        v_axis = normalize(v_axis)
        half_raw = self.array(wall_obj.get("half_size"), location + ".half_size", "half_size")
        if half_raw is None or len(half_raw) != 2:
            self.issue("E_WALL_SIZE", location + ".half_size", "half_size", "half_size must contain two positive numbers.")
            return None
        half_u = self.number(half_raw[0], location + ".half_size[0]", "half_size[0]", low=1.0e-9)
        half_v = self.number(half_raw[1], location + ".half_size[1]", "half_size[1]", low=1.0e-9)
        color_raw = wall_obj.get("color", [0.02, 0.03, 0.06])
        emission_raw = wall_obj.get("emission", [0.02, 0.03, 0.06])
        for field_name, color_raw_value in (("color", color_raw), ("emission", emission_raw)):
            if not isinstance(color_raw_value, list) or len(color_raw_value) != 3 or not all(finite_float(v) for v in color_raw_value) or any(float(v) < 0.0 for v in color_raw_value):
                self.issue("E_COLOR_INVALID", f"{location}.{field_name}", field_name, f"{field_name} must be three non-negative finite numbers.", color_raw_value)
                return None
        shadow_floor = self.number(wall_obj.get("shadow_floor", 0.25), location + ".shadow_floor", "shadow_floor", low=0.0, high=1.0)
        resolution_raw = wall_obj.get("resolution", [256, 192])
        if not isinstance(resolution_raw, list) or len(resolution_raw) != 2:
            self.issue("E_WALL_RESOLUTION", location + ".resolution", "resolution", "resolution must contain two integers.", resolution_raw)
            return None
        rw = self.integer(resolution_raw[0], location + ".resolution[0]", "resolution[0]", low=1)
        rh = self.integer(resolution_raw[1], location + ".resolution[1]", "resolution[1]", low=1)
        if half_u is None or half_v is None or shadow_floor is None or rw is None or rh is None:
            return None
        return Wall(
            point=point,
            normal=normal,
            u_axis=u_axis,
            v_axis=v_axis,
            half_size=np.array([half_u, half_v], dtype=np.float64),
            color=np.asarray(color_raw, dtype=np.float64),
            emission=np.asarray(emission_raw, dtype=np.float64),
            shadow_floor=shadow_floor,
            resolution=(rw, rh),
        )


def load_scene(path: str | Path) -> Scene:
    source = str(path)
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise ValidationFailure([SceneIssue("E_SCENE_IO", "$", "$", f"Unable to read scene: {exc.__class__.__name__}")])
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ValidationFailure([SceneIssue("E_SCENE_JSON", f"$.line{exc.lineno}", "$", f"Invalid JSON: {exc.msg}")])
    return _Validator(raw, source).parse()
