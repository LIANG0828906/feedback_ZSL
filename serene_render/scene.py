from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import SceneParseError, SceneValidationError, ValidationIssue
from .mathutil import EPS, newell_normal, normalize, plane_basis


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def scene_digest(raw: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(raw)).hexdigest()


@dataclass(frozen=True)
class Camera:
    position: np.ndarray
    target: np.ndarray
    up: np.ndarray
    fov_y_deg: float
    near: float
    far: float
    width: int
    height: int
    forward: np.ndarray = field(init=False)
    right: np.ndarray = field(init=False)
    camera_up: np.ndarray = field(init=False)
    tan_half_fov: float = field(init=False)
    aspect: float = field(init=False)

    def __post_init__(self) -> None:
        forward = normalize(self.target - self.position)
        right = normalize(np.cross(forward, self.up))
        camera_up = normalize(np.cross(right, forward))
        object.__setattr__(self, "forward", forward)
        object.__setattr__(self, "right", right)
        object.__setattr__(self, "camera_up", camera_up)
        object.__setattr__(self, "tan_half_fov", math.tan(math.radians(self.fov_y_deg) * 0.5))
        object.__setattr__(self, "aspect", self.width / self.height)

    def world_ray_grid(self, xs: np.ndarray, ys: np.ndarray) -> np.ndarray:
        grid_x, grid_y = np.meshgrid(
            np.asarray(xs, dtype=np.float64),
            np.asarray(ys, dtype=np.float64),
            indexing="xy",
        )
        ndc_x = ((grid_x + 0.5) / self.width) * 2.0 - 1.0
        ndc_y = 1.0 - ((grid_y + 0.5) / self.height) * 2.0
        cam_x = ndc_x * self.tan_half_fov * self.aspect
        cam_y = ndc_y * self.tan_half_fov
        rays = (
            cam_x[..., None] * self.right[None, None, :]
            + cam_y[..., None] * self.camera_up[None, None, :]
            + self.forward[None, None, :]
        )
        return rays / np.linalg.norm(rays, axis=2, keepdims=True)

    def to_camera(self, world_points: np.ndarray) -> np.ndarray:
        points = np.asarray(world_points, dtype=np.float64)
        relative = points - self.position[None, :]
        return np.stack(
            (
                relative @ self.right,
                relative @ self.camera_up,
                relative @ self.forward,
            ),
            axis=-1,
        )

    def project_camera(self, camera_points: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
        points = np.asarray(camera_points, dtype=np.float64)
        z = points[..., 2]
        safe_z = np.where(np.abs(z) > 1.0e-15, z, 1.0e-15)
        ndc_x = points[..., 0] / (safe_z * self.tan_half_fov * self.aspect)
        ndc_y = points[..., 1] / (safe_z * self.tan_half_fov)
        screen_x = (ndc_x * 0.5 + 0.5) * self.width
        screen_y = (0.5 - ndc_y * 0.5) * self.height
        return screen_x, screen_y


@dataclass(frozen=True)
class Sun:
    direction: np.ndarray
    intensity: float
    color: np.ndarray
    light_distance: float
    source_position: np.ndarray
    angular_radius_deg: float


@dataclass(frozen=True)
class Ground:
    y: float
    bounds: tuple[float, float, float, float]
    albedo: np.ndarray
    render: bool


@dataclass(frozen=True)
class ShadowConfig:
    sample_density: int
    penumbra_base: float
    light_distance_scale: float
    max_width: float
    min_width: float
    strength: float


@dataclass(frozen=True)
class Layer:
    layer_index: int
    layer_id: str
    kind: str
    thickness: float
    vertices: np.ndarray
    triangles: np.ndarray
    material: dict[str, Any]
    normal: np.ndarray
    plane_point: np.ndarray
    centroid: np.ndarray
    basis_u: np.ndarray
    basis_v: np.ndarray
    uv_bounds: tuple[float, float, float, float]

    @property
    def is_silk(self) -> bool:
        return self.kind == "silk"

    @property
    def is_mullion(self) -> bool:
        return self.kind == "mullion"


@dataclass(frozen=True)
class Scene:
    raw: dict[str, Any]
    digest: str
    width: int
    height: int
    tile_size: int
    camera: Camera
    sun: Sun
    ground: Ground
    shadow: ShadowConfig
    layers: tuple[Layer, ...]
    ambient: np.ndarray
    exposure: float
    thickness_absorption: float

    def layer_by_index(self, layer_index: int) -> Layer:
        for layer in self.layers:
            if layer.layer_index == layer_index:
                return layer
        raise KeyError(layer_index)

    @property
    def silk_layers(self) -> tuple[Layer, ...]:
        return tuple(layer for layer in self.layers if layer.is_silk)

    @property
    def mullion_layers(self) -> tuple[Layer, ...]:
        return tuple(layer for layer in self.layers if layer.is_mullion)


class _Issues:
    def __init__(self) -> None:
        self.items: list[ValidationIssue] = []

    def add(
        self,
        field: str,
        message: str,
        layer_index: int | None = None,
        mesh_id: str | None = None,
    ) -> None:
        self.items.append(
            ValidationIssue(
                field=field,
                message=message,
                layer_index=layer_index,
                mesh_id=mesh_id,
            )
        )

    def raise_if_any(self) -> None:
        if self.items:
            raise SceneValidationError(self.items)


def load_scene(path: str | Path) -> Scene:
    source = Path(path)
    try:
        text = source.read_text(encoding="utf-8")
    except OSError as exc:
        raise SceneParseError(
            [ValidationIssue(field="$", message=f"cannot read scene file: {exc}")]
        ) from exc
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SceneParseError(
            [
                ValidationIssue(
                    field="$",
                    message=f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
                )
            ]
        ) from exc
    return parse_scene(raw)


def parse_scene(raw: Any) -> Scene:
    issues = _Issues()
    if not isinstance(raw, dict):
        issues.add("$", "scene root must be a JSON object")
        issues.raise_if_any()
        raise AssertionError("unreachable")

    digest = scene_digest(raw)
    image = _object(raw.get("image"), "image", issues)
    width = _positive_int(image.get("width"), "image.width", issues)
    height = _positive_int(image.get("height"), "image.height", issues)
    tile_size = _positive_int(image.get("tile_size"), "image.tile_size", issues)

    camera = _parse_camera(raw.get("camera"), width, height, issues)
    sun = _parse_sun(raw.get("sun"), issues)
    ground = _parse_ground(raw.get("ground"), issues)
    shadow = _parse_shadow(raw.get("shadow"), issues)

    ambient = _color(raw.get("ambient", [0.09, 0.10, 0.12]), "ambient", issues)
    exposure = _positive_float(raw.get("exposure", 1.0), "exposure", issues)
    absorption = _nonnegative_float(
        raw.get("thickness_absorption", 0.65), "thickness_absorption", issues
    )

    layers_raw = raw.get("layers")
    parsed_layers: list[Layer] = []
    if not isinstance(layers_raw, list):
        issues.add("layers", "layers must be a JSON array")
    elif not layers_raw:
        issues.add("layers", "layers must not be empty")
    else:
        seen_ids: dict[str, int] = {}
        seen_indexes: dict[int, int] = {}
        for position, item in enumerate(layers_raw):
            layer = _parse_layer(
                item,
                position,
                issues,
                seen_ids=seen_ids,
                seen_indexes=seen_indexes,
            )
            if layer is not None:
                parsed_layers.append(layer)

        if not issues.items:
            indexes = sorted(layer.layer_index for layer in parsed_layers)
            minimum = indexes[0]
            maximum = indexes[-1]
            if minimum not in (0, 1):
                issues.add(
                    "layer_index",
                    "layer indexes must be contiguous and start at 0 or 1",
                    layer_index=minimum,
                )
            for expected in range(minimum, maximum + 1):
                if expected not in seen_indexes:
                    issues.add(
                        "layer_index",
                        f"missing layer_index {expected}; layer indexes must be contiguous",
                        layer_index=expected,
                    )

    if parsed_layers and sun is not None:
        for layer in parsed_layers:
            parallel = abs(float(np.dot(layer.normal, sun.direction)))
            if parallel <= 1.0e-6:
                issues.add(
                    "sun.direction",
                    "sun direction is parallel to this layer plane",
                    layer_index=layer.layer_index,
                    mesh_id=layer.layer_id,
                )

    if ground is not None and sun is not None:
        if abs(float(np.dot(np.array([0.0, 1.0, 0.0]), sun.direction))) <= 1.0e-6:
            issues.add(
                "sun.direction",
                "sun direction is parallel to the ground receiver plane",
            )

    issues.raise_if_any()

    assert camera is not None
    assert sun is not None
    assert ground is not None
    assert shadow is not None
    parsed_layers.sort(key=lambda layer: layer.layer_index)
    return Scene(
        raw=raw,
        digest=digest,
        width=width,
        height=height,
        tile_size=tile_size,
        camera=camera,
        sun=sun,
        ground=ground,
        shadow=shadow,
        layers=tuple(parsed_layers),
        ambient=ambient,
        exposure=exposure,
        thickness_absorption=absorption,
    )


def _parse_layer(
    item: Any,
    position: int,
    issues: _Issues,
    seen_ids: dict[str, int],
    seen_indexes: dict[int, int],
) -> Layer | None:
    if not isinstance(item, dict):
        issues.add(f"layers[{position}]", "layer must be a JSON object")
        return None

    layer_index = _required_int(item.get("layer_index"), "layer_index", issues)
    layer_id = _required_string(item.get("id"), "id", issues)
    kind = _required_string(item.get("kind"), "kind", issues)
    if kind is not None and kind not in {"silk", "mullion"}:
        issues.add("kind", "kind must be 'silk' or 'mullion'", layer_index, layer_id)
        kind = None
    thickness = _positive_float(item.get("thickness"), "thickness", issues, layer_index, layer_id)

    vertices = _parse_vertices(item.get("vertices"), issues, layer_index, layer_id)
    triangles = _parse_triangles(item.get("triangles"), vertices, issues, layer_index, layer_id)

    material: dict[str, Any] = {}
    if kind == "silk":
        transmittance = _unit_float(
            item.get("transmittance"), "transmittance", issues, layer_index, layer_id
        )
        tint = _color(item.get("tint"), "tint", issues, layer_index, layer_id)
        seed = _required_int(item.get("pattern_seed"), "pattern_seed", issues, layer_index, layer_id)
        resolution = _resolution(
            item.get("texture_resolution"), issues, layer_index, layer_id
        )
        texture_scale = _positive_float(
            item.get("texture_scale", 0.42), "texture_scale", issues, layer_index, layer_id
        )
        scatter = _unit_float(
            item.get("scatter_strength", 0.88),
            "scatter_strength",
            issues,
            layer_index,
            layer_id,
        )
        material.update(
            {
                "transmittance": transmittance,
                "tint": tint,
                "pattern_seed": seed,
                "texture_resolution": resolution,
                "texture_scale": texture_scale,
                "scatter_strength": scatter,
            }
        )
    elif kind == "mullion":
        reflectance = _unit_float(
            item.get("reflectance"), "reflectance", issues, layer_index, layer_id
        )
        width = _positive_float(item.get("width"), "width", issues, layer_index, layer_id)
        color = _color(
            item.get("color", [0.21, 0.14, 0.08]),
            "color",
            issues,
            layer_index,
            layer_id,
        )
        material.update({"reflectance": reflectance, "width": width, "color": color})

    if layer_index is not None:
        if layer_index in seen_indexes:
            issues.add(
                "layer_index",
                f"duplicate layer_index {layer_index}",
                layer_index,
                layer_id,
            )
        else:
            seen_indexes[layer_index] = position
    if layer_id is not None:
        if layer_id in seen_ids:
            issues.add(
                "id",
                f"duplicate layer/mesh id '{layer_id}'",
                layer_index,
                layer_id,
            )
        else:
            seen_ids[layer_id] = position

    if (
        layer_index is None
        or layer_id is None
        or kind is None
        or thickness is None
        or vertices is None
        or triangles is None
    ):
        return None

    try:
        normal = newell_normal(vertices, triangles)
    except ValueError:
        issues.add(
            "vertices",
            "mesh has no non-degenerate triangle from which to derive a plane normal",
            layer_index,
            layer_id,
        )
        return None

    centroid = np.mean(vertices, axis=0)
    scale = max(1.0, float(np.ptp(vertices, axis=0).max()))
    distances = np.abs((vertices - centroid) @ normal)
    if float(distances.max()) > 1.0e-5 * scale:
        issues.add(
            "vertices",
            "mesh vertices are not coplanar; each layer must define one plane",
            layer_index,
            layer_id,
        )
        return None

    basis_u, basis_v = plane_basis(normal)
    relative = vertices - centroid
    u_coords = relative @ basis_u
    v_coords = relative @ basis_v
    uv_bounds = (
        float(u_coords.min()),
        float(u_coords.max()),
        float(v_coords.min()),
        float(v_coords.max()),
    )

    return Layer(
        layer_index=layer_index,
        layer_id=layer_id,
        kind=kind,
        thickness=thickness,
        vertices=vertices,
        triangles=triangles,
        material=material,
        normal=normal,
        plane_point=centroid,
        centroid=centroid,
        basis_u=basis_u,
        basis_v=basis_v,
        uv_bounds=uv_bounds,
    )


def _parse_camera(
    item: Any, width: int | None, height: int | None, issues: _Issues
) -> Camera | None:
    obj = _object(item, "camera", issues)
    position = _vector3(obj.get("position"), "camera.position", issues)
    target = _vector3(obj.get("target"), "camera.target", issues)
    up = _vector3(obj.get("up"), "camera.up", issues)
    fov = _float_in_range(
        obj.get("fov_y_deg"), "camera.fov_y_deg", issues, lower=0.0, upper=180.0, inclusive=False
    )
    near = _positive_float(obj.get("near"), "camera.near", issues)
    far = _positive_float(obj.get("far"), "camera.far", issues)

    if near is not None and far is not None and far <= near:
        issues.add("camera.far", "camera.far must be greater than camera.near")
    if position is not None and target is not None:
        if float(np.linalg.norm(target - position)) <= 1.0e-9:
            issues.add("camera.target", "camera.target must differ from camera.position")
    if position is not None and target is not None and up is not None:
        forward = target - position
        if float(np.linalg.norm(np.cross(forward, up))) <= 1.0e-9:
            issues.add("camera.up", "camera.up must not be parallel to the view direction")

    if (
        position is None
        or target is None
        or up is None
        or fov is None
        or near is None
        or far is None
        or width is None
        or height is None
        or issues.items
    ):
        return None
    return Camera(
        position=position,
        target=target,
        up=up,
        fov_y_deg=fov,
        near=near,
        far=far,
        width=width,
        height=height,
    )


def _parse_sun(item: Any, issues: _Issues) -> Sun | None:
    obj = _object(item, "sun", issues)
    direction = _vector3(obj.get("direction"), "sun.direction", issues)
    intensity = _positive_float(obj.get("intensity", 1.0), "sun.intensity", issues)
    color = _color(obj.get("color", [1.0, 0.96, 0.86]), "sun.color", issues)
    light_distance = _positive_float(
        obj.get("light_distance", 12.0), "sun.light_distance", issues
    )
    angular_radius = _nonnegative_float(
        obj.get("angular_radius_deg", 0.55), "sun.angular_radius_deg", issues
    )

    if direction is not None:
        length = float(np.linalg.norm(direction))
        if abs(length - 1.0) > 1.0e-5:
            issues.add(
                "sun.direction",
                f"sun.direction must be a unit vector (length={length:.9g})",
            )
        elif length > 0.0:
            direction = direction / length

    if (
        direction is None
        or intensity is None
        or color is None
        or light_distance is None
        or angular_radius is None
        or issues.items
    ):
        return None
    source_position = -direction * light_distance
    return Sun(
        direction=direction,
        intensity=intensity,
        color=color,
        light_distance=light_distance,
        source_position=source_position,
        angular_radius_deg=angular_radius,
    )


def _parse_ground(item: Any, issues: _Issues) -> Ground | None:
    obj = _object(item, "ground", issues)
    y = _finite_float(obj.get("y"), "ground.y", issues)
    bounds_raw = obj.get("bounds")
    bounds: tuple[float, float, float, float] | None = None
    if not isinstance(bounds_raw, list) or len(bounds_raw) != 4:
        issues.add("ground.bounds", "ground.bounds must be [xmin, xmax, zmin, zmax]")
    else:
        values = [
            _finite_float(value, f"ground.bounds[{index}]", issues)
            for index, value in enumerate(bounds_raw)
        ]
        if all(value is not None for value in values):
            if values[0] >= values[1]:
                issues.add("ground.bounds", "ground.bounds xmin must be less than xmax")
            if values[2] >= values[3]:
                issues.add("ground.bounds", "ground.bounds zmin must be less than zmax")
            bounds = (float(values[0]), float(values[1]), float(values[2]), float(values[3]))
    albedo = _color(obj.get("albedo", [0.42, 0.37, 0.30]), "ground.albedo", issues)
    render = obj.get("render", True)
    if not isinstance(render, bool):
        issues.add("ground.render", "ground.render must be a boolean")
        render = True
    if y is None or bounds is None or albedo is None or issues.items:
        return None
    return Ground(y=y, bounds=bounds, albedo=albedo, render=render)


def _parse_shadow(item: Any, issues: _Issues) -> ShadowConfig | None:
    obj = _object(item, "shadow", issues)
    density = _positive_int(obj.get("sample_density", 24), "shadow.sample_density", issues)
    if density is not None and density > 4096:
        issues.add("shadow.sample_density", "shadow.sample_density must be at most 4096")
    base = _positive_float(
        obj.get("penumbra_base", 0.055), "shadow.penumbra_base", issues
    )
    scale = _positive_float(
        obj.get("light_distance_scale", 18.0),
        "shadow.light_distance_scale",
        issues,
    )
    max_width = _positive_float(
        obj.get("max_width", 4.0), "shadow.max_width", issues
    )
    min_width = _positive_float(
        obj.get("min_width", 0.0005), "shadow.min_width", issues
    )
    strength = _unit_float(
        obj.get("strength", 0.78), "shadow.strength", issues
    )
    if min_width is not None and max_width is not None and min_width > max_width:
        issues.add("shadow.min_width", "shadow.min_width must not exceed shadow.max_width")
    if (
        density is None
        or base is None
        or scale is None
        or max_width is None
        or min_width is None
        or strength is None
        or issues.items
    ):
        return None
    return ShadowConfig(
        sample_density=density,
        penumbra_base=base,
        light_distance_scale=scale,
        max_width=max_width,
        min_width=min_width,
        strength=strength,
    )


def _parse_vertices(
    value: Any,
    issues: _Issues,
    layer_index: int | None,
    layer_id: str | None,
) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) < 3:
        issues.add(
            "vertices",
            "vertices must be an array containing at least three [x,y,z] points",
            layer_index,
            layer_id,
        )
        return None
    rows: list[list[float]] = []
    valid = True
    for index, point in enumerate(value):
        if not isinstance(point, list) or len(point) != 3:
            issues.add(
                "vertices",
                f"vertices[{index}] must be [x,y,z]",
                layer_index,
                layer_id,
            )
            valid = False
            continue
        row: list[float] = []
        for axis, number in enumerate(point):
            if _is_number(number):
                row.append(float(number))
            else:
                issues.add(
                    "vertices",
                    f"vertices[{index}][{axis}] must be a finite number",
                    layer_index,
                    layer_id,
                )
                valid = False
                row.append(0.0)
        rows.append(row)
    if not valid:
        return None
    return np.asarray(rows, dtype=np.float64)


def _parse_triangles(
    value: Any,
    vertices: np.ndarray | None,
    issues: _Issues,
    layer_index: int | None,
    layer_id: str | None,
) -> np.ndarray | None:
    if not isinstance(value, list) or not value:
        issues.add(
            "triangles",
            "triangles must be a non-empty array of index triples",
            layer_index,
            layer_id,
        )
        return None
    rows: list[list[int]] = []
    valid = True
    vertex_count = vertices.shape[0] if vertices is not None else -1
    for index, tri in enumerate(value):
        if not isinstance(tri, list) or len(tri) != 3:
            issues.add(
                "triangles",
                f"triangles[{index}] must contain three vertex indices",
                layer_index,
                layer_id,
            )
            valid = False
            continue
        row: list[int] = []
        for corner, number in enumerate(tri):
            if not _is_int(number):
                issues.add(
                    "triangles",
                    f"triangles[{index}][{corner}] must be an integer",
                    layer_index,
                    layer_id,
                )
                valid = False
                row.append(0)
                continue
            integer = int(number)
            row.append(integer)
            if vertex_count >= 0 and not (0 <= integer < vertex_count):
                issues.add(
                    "triangles",
                    f"triangle index {integer} is outside [0, {vertex_count - 1}]",
                    layer_index,
                    layer_id,
                )
                valid = False
        if len(set(row)) != 3:
            issues.add(
                "triangles",
                f"triangles[{index}] repeats a vertex index",
                layer_index,
                layer_id,
            )
            valid = False
        rows.append(row)
    if not valid:
        return None

    triangles = np.asarray(rows, dtype=np.int64)
    if vertices is not None:
        for index, tri in enumerate(triangles):
            a, b, c = vertices[tri[0]], vertices[tri[1]], vertices[tri[2]]
            area = float(np.linalg.norm(np.cross(b - a, c - a))) * 0.5
            if area <= 1.0e-10:
                issues.add(
                    "triangles",
                    f"triangles[{index}] is degenerate (area={area:.3g})",
                    layer_index,
                    layer_id,
                )
                valid = False
    return triangles if valid else None


def _resolution(
    value: Any,
    issues: _Issues,
    layer_index: int | None,
    layer_id: str | None,
) -> tuple[int, int] | None:
    if not isinstance(value, list) or len(value) != 2:
        issues.add(
            "texture_resolution",
            "texture_resolution must be [width, height]",
            layer_index,
            layer_id,
        )
        return None
    width = _positive_int(value[0], "texture_resolution[0]", issues, layer_index, layer_id)
    height = _positive_int(value[1], "texture_resolution[1]", issues, layer_index, layer_id)
    if width is None or height is None:
        return None
    if width > 4096 or height > 4096:
        issues.add(
            "texture_resolution",
            "texture_resolution dimensions must be at most 4096",
            layer_index,
            layer_id,
        )
        return None
    return (width, height)


def _object(value: Any, field: str, issues: _Issues) -> dict[str, Any]:
    if not isinstance(value, dict):
        issues.add(field, f"{field} must be a JSON object")
        return {}
    return value


def _is_number(value: Any) -> bool:
    return (
        not isinstance(value, bool)
        and isinstance(value, (int, float))
        and math.isfinite(float(value))
    )


def _is_int(value: Any) -> bool:
    return not isinstance(value, bool) and isinstance(value, int)


def _required_int(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> int | None:
    if value is None:
        issues.add(field, f"{field} is required", layer_index, mesh_id)
        return None
    if not _is_int(value):
        issues.add(field, f"{field} must be an integer", layer_index, mesh_id)
        return None
    return int(value)


def _required_string(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
) -> str | None:
    if value is None:
        issues.add(field, f"{field} is required", layer_index)
        return None
    if not isinstance(value, str) or not value:
        issues.add(field, f"{field} must be a non-empty string", layer_index)
        return None
    return value


def _positive_int(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> int | None:
    if not _is_int(value) or int(value) <= 0:
        issues.add(field, f"{field} must be a positive integer", layer_index, mesh_id)
        return None
    return int(value)


def _finite_float(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> float | None:
    if not _is_number(value):
        issues.add(field, f"{field} must be a finite number", layer_index, mesh_id)
        return None
    return float(value)


def _positive_float(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> float | None:
    result = _finite_float(value, field, issues, layer_index, mesh_id)
    if result is None:
        return None
    if result <= 0.0:
        issues.add(field, f"{field} must be greater than zero", layer_index, mesh_id)
        return None
    return result


def _nonnegative_float(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> float | None:
    result = _finite_float(value, field, issues, layer_index, mesh_id)
    if result is None:
        return None
    if result < 0.0:
        issues.add(field, f"{field} must be non-negative", layer_index, mesh_id)
        return None
    return result


def _unit_float(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> float | None:
    return _float_in_range(
        value,
        field,
        issues,
        lower=0.0,
        upper=1.0,
        inclusive=True,
        layer_index=layer_index,
        mesh_id=mesh_id,
    )


def _float_in_range(
    value: Any,
    field: str,
    issues: _Issues,
    lower: float,
    upper: float,
    inclusive: bool,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> float | None:
    result = _finite_float(value, field, issues, layer_index, mesh_id)
    if result is None:
        return None
    lower_ok = result >= lower if inclusive else result > lower
    upper_ok = result <= upper if inclusive else result < upper
    if not (lower_ok and upper_ok):
        bracket = "[" if inclusive else "("
        closing = "]" if inclusive else ")"
        issues.add(
            field,
            f"{field} must be in {bracket}{lower}, {upper}{closing}",
            layer_index,
            mesh_id,
        )
        return None
    return result


def _vector3(
    value: Any,
    field: str,
    issues: _Issues,
) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != 3:
        issues.add(field, f"{field} must be [x, y, z]")
        return None
    numbers: list[float] = []
    for axis, number in enumerate(value):
        if not _is_number(number):
            issues.add(field, f"{field}[{axis}] must be a finite number")
            return None
        numbers.append(float(number))
    return np.asarray(numbers, dtype=np.float64)


def _color(
    value: Any,
    field: str,
    issues: _Issues,
    layer_index: int | None = None,
    mesh_id: str | None = None,
) -> np.ndarray | None:
    if not isinstance(value, list) or len(value) != 3:
        issues.add(field, f"{field} must be [r, g, b]", layer_index, mesh_id)
        return None
    numbers: list[float] = []
    for axis, number in enumerate(value):
        parsed = _unit_float(
            number,
            f"{field}[{axis}]",
            issues,
            layer_index,
            mesh_id,
        )
        if parsed is None:
            return None
        numbers.append(parsed)
    return np.asarray(numbers, dtype=np.float64)


def _rect_grid(
    xmin: float,
    xmax: float,
    ymin: float,
    ymax: float,
    z: float,
    nx: int,
    ny: int,
) -> tuple[list[list[float]], list[list[int]]]:
    vertices: list[list[float]] = []
    for row in range(ny + 1):
        y = ymin + (ymax - ymin) * row / ny
        for column in range(nx + 1):
            x = xmin + (xmax - xmin) * column / nx
            vertices.append([x, y, z])
    triangles: list[list[int]] = []
    stride = nx + 1
    for row in range(ny):
        for column in range(nx):
            a = row * stride + column
            b = a + 1
            c = a + stride
            d = c + 1
            triangles.append([a, b, d])
            triangles.append([a, d, c])
    return vertices, triangles


def _append_quad(
    vertices: list[list[float]],
    triangles: list[list[int]],
    x0: float,
    x1: float,
    y0: float,
    y1: float,
    z: float,
) -> None:
    base = len(vertices)
    vertices.extend(
        [
            [x0, y0, z],
            [x1, y0, z],
            [x1, y1, z],
            [x0, y1, z],
        ]
    )
    triangles.append([base, base + 1, base + 2])
    triangles.append([base, base + 2, base + 3])


def _mullion_mesh() -> tuple[list[list[float]], list[list[int]]]:
    vertices: list[list[float]] = []
    triangles: list[list[int]] = []
    z = 0.0
    for x in (-1.13, -0.39, 0.39, 1.13):
        half = 0.021
        _append_quad(vertices, triangles, x - half, x + half, 0.1, 3.02, z)
    for y in (0.1, 0.76, 1.86, 3.02):
        half = 0.026
        _append_quad(vertices, triangles, -1.35, 1.35, y - half, y + half, z)
    return vertices, triangles


def make_sample_scene() -> dict[str, Any]:
    """Build the deterministic three-silk/two-mullion garden-door scene."""
    sun = np.array([0.46, -0.32, -0.828], dtype=np.float64)
    sun /= np.linalg.norm(sun)

    layers: list[dict[str, Any]] = []
    layer_specs = [
        (
            0,
            "silk_inner",
            "silk",
            -0.36,
            {
                "transmittance": 0.58,
                "tint": [0.91, 0.55, 0.30],
                "pattern_seed": 1301,
                "texture_resolution": [128, 128],
                "texture_scale": 0.42,
                "scatter_strength": 0.92,
            },
        ),
        (
            1,
            "lattice_inner",
            "mullion",
            -0.18,
            {
                "reflectance": 0.18,
                "width": 0.042,
                "color": [0.20, 0.13, 0.075],
            },
        ),
        (
            2,
            "silk_middle",
            "silk",
            0.0,
            {
                "transmittance": 0.64,
                "tint": [0.78, 0.40, 0.31],
                "pattern_seed": 2777,
                "texture_resolution": [144, 112],
                "texture_scale": 0.37,
                "scatter_strength": 0.86,
            },
        ),
        (
            3,
            "lattice_outer",
            "mullion",
            0.18,
            {
                "reflectance": 0.22,
                "width": 0.048,
                "color": [0.18, 0.11, 0.065],
            },
        ),
        (
            4,
            "silk_outer",
            "silk",
            0.36,
            {
                "transmittance": 0.56,
                "tint": [0.95, 0.74, 0.43],
                "pattern_seed": 4909,
                "texture_resolution": [160, 128],
                "texture_scale": 0.46,
                "scatter_strength": 0.90,
            },
        ),
    ]

    for layer_index, layer_id, kind, z, material in layer_specs:
        if kind == "silk":
            vertices, triangles = _rect_grid(-1.36, 1.36, 0.09, 3.03, z, 3, 4)
        else:
            vertices, triangles = _mullion_mesh()
            for vertex in vertices:
                vertex[2] = z
        layers.append(
            {
                "layer_index": layer_index,
                "id": layer_id,
                "kind": kind,
                "thickness": 0.18,
                "vertices": vertices,
                "triangles": triangles,
                **material,
            }
        )

    return {
        "format_version": 1,
        "image": {"width": 320, "height": 240, "tile_size": 64},
        "camera": {
            "position": [3.35, 2.55, 7.2],
            "target": [0.0, 1.55, 0.0],
            "up": [0.0, 1.0, 0.0],
            "fov_y_deg": 38.0,
            "near": 0.05,
            "far": 40.0,
        },
        "sun": {
            "direction": [float(value) for value in sun],
            "intensity": 1.12,
            "color": [1.0, 0.955, 0.84],
            "light_distance": 12.0,
            "angular_radius_deg": 0.55,
        },
        "ground": {
            "y": 0.0,
            "bounds": [-6.0, 6.0, -6.0, 6.0],
            "albedo": [0.43, 0.38, 0.31],
            "render": True,
        },
        "shadow": {
            "sample_density": 64,
            "penumbra_base": 0.80,
            "light_distance_scale": 18.0,
            "max_width": 1.20,
            "min_width": 0.0008,
            "strength": 0.78,
        },
        "ambient": [0.085, 0.095, 0.115],
        "exposure": 0.95,
        "thickness_absorption": 0.65,
        "layers": layers,
    }


def write_sample_scene(path: str | Path) -> Path:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(
        make_sample_scene(),
        indent=2,
        sort_keys=True,
        ensure_ascii=False,
        allow_nan=False,
    )
    output.write_text(payload + "\n", encoding="utf-8", newline="\n")
    return output
