"""Scene model and validation."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass
from typing import Iterable

import numpy as np

from .errors import AppError, scene_error


_EPS = 1e-12


@dataclass(frozen=True, slots=True)
class Camera:
    width: int
    height: int
    intrinsics: np.ndarray
    world_to_camera: np.ndarray
    camera_to_world: np.ndarray
    position: np.ndarray

    def ray_direction(self, pixel_x: int, pixel_y: int) -> np.ndarray:
        pixel = np.array(
            [pixel_x + 0.5, pixel_y + 0.5, 1.0],
            dtype=np.float64,
        )
        direction_camera = np.linalg.solve(self.intrinsics, pixel)
        direction_world = self.camera_to_world[:3, :3] @ direction_camera
        return direction_world / np.linalg.norm(direction_world)


@dataclass(frozen=True, slots=True)
class DirectionalLight:
    direction: np.ndarray
    color: np.ndarray
    intensity: float

    @property
    def radiance(self) -> np.ndarray:
        return self.color * self.intensity


@dataclass(frozen=True, slots=True)
class DensityBlock:
    block_id: int
    medium_id: str
    dims: tuple[int, int, int]
    bounds_min: np.ndarray
    bounds_max: np.ndarray
    offset: int
    count: int
    version: int
    values: np.ndarray

    def sample(self, point: np.ndarray) -> float:
        return _trilinear(
            self.values,
            self.bounds_min,
            self.bounds_max,
            point,
        )


@dataclass(frozen=True, slots=True)
class Medium:
    medium_id: str
    kind: str
    bounds_min: np.ndarray
    bounds_max: np.ndarray
    center: np.ndarray | None
    radius: float | None
    density_block_id: int
    sigma_s: float
    sigma_a: float
    phase_g: float
    density_block: DensityBlock

    @property
    def sigma_t(self) -> float:
        return self.sigma_s + self.sigma_a

    def contains(self, point: np.ndarray) -> bool:
        if self.kind == "aabb":
            return bool(
                np.all(point >= self.bounds_min)
                and np.all(point <= self.bounds_max)
            )
        assert self.center is not None and self.radius is not None
        delta = point - self.center
        return float(np.dot(delta, delta)) <= self.radius * self.radius

    def intersect_interval(
        self, origin: np.ndarray, direction: np.ndarray, t_max: float
    ) -> tuple[float, float] | None:
        if self.kind == "aabb":
            with np.errstate(divide="ignore", invalid="ignore"):
                inv = 1.0 / direction
                t0 = (self.bounds_min - origin) * inv
                t1 = (self.bounds_max - origin) * inv
            near = np.minimum(t0, t1)
            far = np.maximum(t0, t1)
            enter = float(np.max(near))
            exit_ = float(np.min(far))
        else:
            assert self.center is not None and self.radius is not None
            oc = origin - self.center
            a = float(np.dot(direction, direction))
            b = 2.0 * float(np.dot(oc, direction))
            c = float(np.dot(oc, oc)) - self.radius * self.radius
            discriminant = b * b - 4.0 * a * c
            if discriminant < 0.0:
                return None
            root = math.sqrt(max(0.0, discriminant))
            enter = (-b - root) / (2.0 * a)
            exit_ = (-b + root) / (2.0 * a)
        enter = max(0.0, enter)
        exit_ = min(t_max, exit_)
        if exit_ <= enter + _EPS:
            return None
        return enter, exit_

    def sample_density(self, point: np.ndarray) -> float:
        return max(0.0, self.density_block.sample(point))


@dataclass(frozen=True, slots=True)
class Scene:
    scene_id: str
    camera: Camera
    light: DirectionalLight
    background: np.ndarray
    media: tuple[Medium, ...]
    density_blocks: tuple[DensityBlock, ...]
    header: dict
    density_version: str
    fingerprint: str

    @property
    def width(self) -> int:
        return self.camera.width

    @property
    def height(self) -> int:
        return self.camera.height


def _trilinear(
    values: np.ndarray,
    bounds_min: np.ndarray,
    bounds_max: np.ndarray,
    point: np.ndarray,
) -> float:
    if np.any(point < bounds_min) or np.any(point > bounds_max):
        return 0.0
    extent = bounds_max - bounds_min
    normalized = np.clip((point - bounds_min) / extent, 0.0, 1.0)
    nx, ny, nz = values.shape
    x = normalized[0] * (nx - 1)
    y = normalized[1] * (ny - 1)
    z = normalized[2] * (nz - 1)
    x0 = min(int(math.floor(x)), nx - 1)
    y0 = min(int(math.floor(y)), ny - 1)
    z0 = min(int(math.floor(z)), nz - 1)
    x1 = min(x0 + 1, nx - 1)
    y1 = min(y0 + 1, ny - 1)
    z1 = min(z0 + 1, nz - 1)
    fx = x - x0
    fy = y - y0
    fz = z - z0
    c00 = values[z0, y0, x0] * (1 - fx) + values[z0, y0, x1] * fx
    c10 = values[z0, y1, x0] * (1 - fx) + values[z0, y1, x1] * fx
    c01 = values[z1, y0, x0] * (1 - fx) + values[z1, y0, x1] * fx
    c11 = values[z1, y1, x0] * (1 - fx) + values[z1, y1, x1] * fx
    c0 = c00 * (1 - fy) + c10 * fy
    c1 = c01 * (1 - fy) + c11 * fy
    return float(c0 * (1 - fz) + c1 * fz)


def _matrix(value: object, field_name: str, rows: int, cols: int) -> np.ndarray:
    try:
        matrix = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise scene_error("matrix must contain numbers", field_name=field_name) from exc
    if matrix.shape != (rows, cols):
        raise scene_error(
            f"matrix must have shape {rows}x{cols}, got {matrix.shape}",
            field_name=field_name,
        )
    if not np.all(np.isfinite(matrix)):
        raise scene_error("matrix contains a non-finite value", field_name=field_name)
    try:
        rank = np.linalg.matrix_rank(matrix)
    except np.linalg.LinAlgError as exc:
        raise scene_error("matrix rank check failed", field_name=field_name) from exc
    if rank < min(rows, cols):
        raise scene_error("matrix is singular", field_name=field_name)
    return matrix


def _vector(value: object, field_name: str, size: int) -> np.ndarray:
    try:
        vector = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError) as exc:
        raise scene_error(
            "vector must contain numbers", field_name=field_name
        ) from exc
    if vector.shape != (size,):
        raise scene_error(
            f"vector must have {size} components", field_name=field_name
        )
    if not np.all(np.isfinite(vector)):
        raise scene_error("vector contains a non-finite value", field_name=field_name)
    return vector


def _number(
    container: dict, key: str, field_name: str, medium_id: str | None = None
) -> float:
    if key not in container:
        raise scene_error(
            f"missing field: {key}",
            field_name=field_name,
            medium_id=medium_id,
        )
    value = container[key]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise scene_error(
            f"field {key} must be numeric",
            field_name=field_name,
            medium_id=medium_id,
        )
    numeric = float(value)
    if not math.isfinite(numeric):
        raise scene_error(
            f"field {key} must be finite",
            field_name=field_name,
            medium_id=medium_id,
        )
    return numeric


def _integer(
    container: dict,
    key: str,
    field_name: str,
    medium_id: str | None = None,
    minimum: int = 0,
) -> int:
    if key not in container:
        raise scene_error(
            f"missing field: {key}",
            field_name=field_name,
            medium_id=medium_id,
        )
    value = container[key]
    if isinstance(value, bool) or not isinstance(value, int):
        raise scene_error(
            f"field {key} must be an integer",
            field_name=field_name,
            medium_id=medium_id,
        )
    if value < minimum:
        raise scene_error(
            f"field {key} must be >= {minimum}",
            field_name=field_name,
            medium_id=medium_id,
        )
    return value


def _require_dict(
    container: dict,
    key: str,
    field_name: str,
    medium_id: str | None = None,
) -> dict:
    if key not in container:
        raise scene_error(
            f"missing field: {key}",
            field_name=field_name,
            medium_id=medium_id,
        )
    value = container[key]
    if not isinstance(value, dict):
        raise scene_error(
            f"field {key} must be an object",
            field_name=field_name,
            medium_id=medium_id,
        )
    return value


def _shape_bounds(
    kind: str,
    item: dict,
    field_prefix: str,
    medium_id: str,
) -> tuple[np.ndarray, np.ndarray, np.ndarray | None, float | None]:
    if kind == "aabb":
        if "min" not in item:
            raise scene_error(
                "missing field: min",
                field_name=f"{field_prefix}.min",
                medium_id=medium_id,
            )
        if "max" not in item:
            raise scene_error(
                "missing field: max",
                field_name=f"{field_prefix}.max",
                medium_id=medium_id,
            )
        lower = _vector(item["min"], f"{field_prefix}.min", 3)
        upper = _vector(item["max"], f"{field_prefix}.max", 3)
        if np.any(upper <= lower):
            raise scene_error(
                "AABB has non-positive extent",
                field_name=f"{field_prefix}.max",
                medium_id=medium_id,
            )
        return lower, upper, None, None
    if kind == "sphere":
        if "center" not in item:
            raise scene_error(
                "missing field: center",
                field_name=f"{field_prefix}.center",
                medium_id=medium_id,
            )
        center = _vector(item["center"], f"{field_prefix}.center", 3)
        radius = _number(item, "radius", f"{field_prefix}.radius", medium_id)
        if radius <= 0:
            raise scene_error(
                "sphere radius must be positive",
                field_name=f"{field_prefix}.radius",
                medium_id=medium_id,
            )
        return center - radius, center + radius, center, radius
    raise scene_error(
        "medium type must be aabb or sphere",
        field_name=f"{field_prefix}.type",
        medium_id=medium_id,
    )


def _contains_bounds(
    outer_min: np.ndarray,
    outer_max: np.ndarray,
    inner_min: np.ndarray,
    inner_max: np.ndarray,
) -> bool:
    return bool(np.all(outer_min <= inner_min) and np.all(outer_max >= inner_max))


def _spheres_disjoint(
    a: Medium, b: Medium
) -> bool:
    assert a.center is not None and b.center is not None
    assert a.radius is not None and b.radius is not None
    return float(np.linalg.norm(a.center - b.center)) > a.radius + b.radius + 1e-9


def _aabb_sphere_disjoint(a: Medium, sphere: Medium) -> bool:
    assert sphere.center is not None and sphere.radius is not None
    closest = np.clip(sphere.center, a.bounds_min, a.bounds_max)
    return float(np.linalg.norm(closest - sphere.center)) > sphere.radius + 1e-9


def _shapes_disjoint(a: Medium, b: Medium) -> bool:
    bbox_disjoint = bool(
        np.any(a.bounds_max < b.bounds_min) or np.any(b.bounds_max < a.bounds_min)
    )
    if bbox_disjoint:
        return True
    if a.kind == "sphere" and b.kind == "sphere":
        return _spheres_disjoint(a, b)
    if a.kind == "aabb" and b.kind == "sphere":
        return _aabb_sphere_disjoint(a, b)
    if a.kind == "sphere" and b.kind == "aabb":
        return _aabb_sphere_disjoint(b, a)
    return False


def _validate_nesting(media: tuple[Medium, ...]) -> None:
    for i, first in enumerate(media):
        for second in media[i + 1 :]:
            if _shapes_disjoint(first, second):
                continue
            first_inside = _contains_bounds(
                second.bounds_min,
                second.bounds_max,
                first.bounds_min,
                first.bounds_max,
            )
            second_inside = _contains_bounds(
                first.bounds_min,
                first.bounds_max,
                second.bounds_min,
                second.bounds_max,
            )
            if not first_inside and not second_inside:
                raise scene_error(
                    "media overlap without a full containment relationship",
                    field_name="media.intervals",
                    medium_id=second.medium_id,
                )
            if (
                first.kind == "sphere"
                and second.kind == "aabb"
                and second_inside
            ):
                corners = np.array(
                    [
                        [x, y, z]
                        for x in (second.bounds_min[0], second.bounds_max[0])
                        for y in (second.bounds_min[1], second.bounds_max[1])
                        for z in (second.bounds_min[2], second.bounds_max[2])
                    ]
                )
                assert first.center is not None and first.radius is not None
                if np.any(
                    np.linalg.norm(corners - first.center, axis=1)
                    > first.radius + 1e-9
                ):
                    raise scene_error(
                        "AABB is not fully contained by sphere",
                        field_name="media.intervals",
                        medium_id=second.medium_id,
                    )


def _density_version(blocks: Iterable[DensityBlock]) -> str:
    digest = hashlib.sha256()
    for block in sorted(blocks, key=lambda item: item.block_id):
        digest.update(
            f"{block.block_id}:{block.version}:{block.dims}:{block.count}:".encode()
        )
        digest.update(np.ascontiguousarray(block.values, dtype="<f4").tobytes())
    return digest.hexdigest()


def _scene_fingerprint(scene_id: str, header: dict, blocks: tuple[DensityBlock, ...]) -> str:
    digest = hashlib.sha256()
    import json

    digest.update(scene_id.encode("utf-8"))
    digest.update(
        json.dumps(header, sort_keys=True, separators=(",", ":")).encode("utf-8")
    )
    digest.update(_density_version(blocks).encode("ascii"))
    return digest.hexdigest()


def build_scene(header: dict, payload: bytes) -> Scene:
    if not isinstance(header, dict):
        raise scene_error("scene header must be an object", field_name="header")
    if "camera" not in header:
        raise scene_error("missing field: camera", field_name="camera")
    if "light" not in header:
        raise scene_error("missing field: light", field_name="light")
    if "media" not in header:
        raise scene_error("missing field: media", field_name="media")
    if "density_blocks" not in header:
        raise scene_error("missing field: density_blocks", field_name="density_blocks")

    scene_id = header.get("scene_id", "unnamed")
    if not isinstance(scene_id, str) or not scene_id:
        raise scene_error("scene_id must be non-empty", field_name="scene_id")

    camera_data = _require_dict(header, "camera", "camera")
    width = _integer(camera_data, "width", "camera.width", minimum=1)
    height = _integer(camera_data, "height", "camera.height", minimum=1)
    if width > 4096 or height > 4096:
        raise scene_error(
            "camera dimensions exceed preview limit",
            field_name="camera.width" if width > 4096 else "camera.height",
        )
    if "K" not in camera_data:
        raise scene_error("missing field: K", field_name="camera.K")
    if "world_to_camera" not in camera_data:
        raise scene_error(
            "missing field: world_to_camera", field_name="camera.world_to_camera"
        )
    intrinsics = _matrix(camera_data["K"], "camera.K", 3, 3)
    world_to_camera = _matrix(
        camera_data["world_to_camera"], "camera.world_to_camera", 4, 4
    )
    if abs(float(np.linalg.det(intrinsics))) < _EPS:
        raise scene_error("camera intrinsics are singular", field_name="camera.K")
    if abs(float(np.linalg.det(world_to_camera))) < _EPS:
        raise scene_error(
            "camera extrinsics are singular", field_name="camera.world_to_camera"
        )
    camera_to_world = np.linalg.inv(world_to_camera)
    position = camera_to_world[:3, 3].copy()

    light_data = _require_dict(header, "light", "light")
    if "direction" not in light_data:
        raise scene_error("missing field: direction", field_name="light.direction")
    direction = _vector(light_data["direction"], "light.direction", 3)
    norm = float(np.linalg.norm(direction))
    if norm < _EPS:
        raise scene_error(
            "light direction must be non-zero", field_name="light.direction"
        )
    direction = direction / norm
    color = _vector(light_data.get("color", [1.0, 1.0, 1.0]), "light.color", 3)
    intensity = _number(light_data, "intensity", "light.intensity")
    if intensity < 0:
        raise scene_error("light intensity must be non-negative", field_name="light.intensity")
    background = _vector(
        header.get("background", [0.0, 0.0, 0.0]), "background", 3
    )
    if np.any(background < 0):
        raise scene_error("background must be non-negative", field_name="background")

    raw_blocks = header["density_blocks"]
    if not isinstance(raw_blocks, list) or not raw_blocks:
        raise scene_error(
            "density_blocks must be a non-empty array", field_name="density_blocks"
        )
    blocks: list[DensityBlock] = []
    seen_block_ids: set[int] = set()
    payload_size = len(payload)
    for index, raw in enumerate(raw_blocks):
        prefix = f"density_blocks[{index}]"
        if not isinstance(raw, dict):
            raise scene_error(
                "density block must be an object", field_name=prefix
            )
        block_id = _integer(raw, "block_id", f"{prefix}.block_id")
        if block_id in seen_block_ids:
            raise scene_error(
                "duplicate density block id", field_name=f"{prefix}.block_id"
            )
        seen_block_ids.add(block_id)
        if "medium_id" not in raw or not isinstance(raw["medium_id"], str):
            raise scene_error(
                "missing or invalid medium_id",
                field_name=f"{prefix}.medium_id",
            )
        medium_id = raw["medium_id"]
        dims_raw = raw.get("dims")
        if (
            not isinstance(dims_raw, list)
            or len(dims_raw) != 3
            or any(
                isinstance(value, bool) or not isinstance(value, int) or value < 2
                for value in dims_raw
            )
        ):
            raise scene_error(
                "dims must contain three integers >= 2",
                field_name=f"{prefix}.dims",
                medium_id=medium_id,
            )
        dims = (dims_raw[0], dims_raw[1], dims_raw[2])
        bounds_raw = raw.get("bounds")
        if not isinstance(bounds_raw, list) or len(bounds_raw) != 6:
            raise scene_error(
                "bounds must contain six numbers",
                field_name=f"{prefix}.bounds",
                medium_id=medium_id,
            )
        bounds = _vector(bounds_raw, f"{prefix}.bounds", 6)
        bounds_min = bounds[:3]
        bounds_max = bounds[3:]
        if np.any(bounds_max <= bounds_min):
            raise scene_error(
                "density bounds have non-positive extent",
                field_name=f"{prefix}.bounds",
                medium_id=medium_id,
            )
        offset = _integer(raw, "offset", f"{prefix}.offset")
        count = _integer(raw, "count", f"{prefix}.count", minimum=1)
        version = _integer(raw, "version", f"{prefix}.version")
        expected_count = dims[0] * dims[1] * dims[2]
        if count != expected_count:
            raise scene_error(
                f"density count {count} does not match dims {dims}",
                field_name=f"{prefix}.count",
                medium_id=medium_id,
            )
        byte_offset = offset * 4
        byte_end = byte_offset + count * 4
        if offset < 0 or byte_offset > payload_size or byte_end > payload_size:
            raise scene_error(
                "density block points outside payload",
                field_name=f"{prefix}.offset",
                medium_id=medium_id,
            )
        values = np.frombuffer(
            payload, dtype="<f4", count=count, offset=byte_offset
        ).astype(np.float64)
        if not np.all(np.isfinite(values)):
            raise scene_error(
                "density values contain non-finite numbers",
                field_name=f"{prefix}.values",
                medium_id=medium_id,
            )
        if np.any(values < 0):
            raise scene_error(
                "density values must be non-negative",
                field_name=f"{prefix}.values",
                medium_id=medium_id,
            )
        blocks.append(
            DensityBlock(
                block_id=block_id,
                medium_id=medium_id,
                dims=dims,
                bounds_min=bounds_min,
                bounds_max=bounds_max,
                offset=offset,
                count=count,
                version=version,
                values=values.reshape((dims[2], dims[1], dims[0])),
            )
        )

    blocks_by_id = {block.block_id: block for block in blocks}
    raw_media = header["media"]
    if not isinstance(raw_media, list) or not raw_media:
        raise scene_error("media must be a non-empty array", field_name="media")
    media: list[Medium] = []
    seen_medium_ids: set[str] = set()
    used_blocks: set[int] = set()
    for index, raw in enumerate(raw_media):
        prefix = f"media[{index}]"
        if not isinstance(raw, dict):
            raise scene_error("medium must be an object", field_name=prefix)
        medium_id = raw.get("id")
        if not isinstance(medium_id, str) or not medium_id:
            raise scene_error(
                "medium id must be a non-empty string", field_name=f"{prefix}.id"
            )
        if medium_id in seen_medium_ids:
            raise scene_error(
                "duplicate medium id",
                field_name=f"{prefix}.id",
                medium_id=medium_id,
            )
        seen_medium_ids.add(medium_id)
        kind = raw.get("type")
        if kind not in {"aabb", "sphere"}:
            raise scene_error(
                "medium type must be aabb or sphere",
                field_name=f"{prefix}.type",
                medium_id=medium_id,
            )
        bounds_min, bounds_max, center, radius = _shape_bounds(
            kind, raw, prefix, medium_id
        )
        block_id = _integer(
            raw,
            "density_block",
            f"{prefix}.density_block",
            medium_id=medium_id,
        )
        if block_id not in blocks_by_id:
            raise scene_error(
                f"density block {block_id} does not exist",
                field_name=f"{prefix}.density_block",
                medium_id=medium_id,
            )
        block = blocks_by_id[block_id]
        if block.medium_id != medium_id:
            raise scene_error(
                "density block medium_id does not match medium",
                field_name=f"{prefix}.density_block",
                medium_id=medium_id,
            )
        used_blocks.add(block_id)
        sigma_s = _number(raw, "sigma_s", f"{prefix}.sigma_s", medium_id)
        sigma_a = _number(raw, "sigma_a", f"{prefix}.sigma_a", medium_id)
        phase_g = _number(raw, "phase_g", f"{prefix}.phase_g", medium_id)
        if sigma_s < 0 or sigma_a < 0:
            raise scene_error(
                "sigma_s and sigma_a must be non-negative",
                field_name=f"{prefix}.sigma_s",
                medium_id=medium_id,
            )
        if sigma_s + sigma_a <= 0:
            raise scene_error(
                "medium must have positive extinction",
                field_name=f"{prefix}.sigma_s",
                medium_id=medium_id,
            )
        if not -0.95 <= phase_g <= 0.95:
            raise scene_error(
                "phase_g must be in [-0.95, 0.95]",
                field_name=f"{prefix}.phase_g",
                medium_id=medium_id,
            )
        media.append(
            Medium(
                medium_id=medium_id,
                kind=kind,
                bounds_min=bounds_min,
                bounds_max=bounds_max,
                center=center,
                radius=radius,
                density_block_id=block_id,
                sigma_s=sigma_s,
                sigma_a=sigma_a,
                phase_g=phase_g,
                density_block=block,
            )
        )

    unused_blocks = sorted(seen_block_ids - used_blocks)
    if unused_blocks:
        first_unused = blocks_by_id[unused_blocks[0]]
        raise scene_error(
            f"density block {unused_blocks[0]} is not referenced by a medium",
            field_name="density_blocks",
            medium_id=first_unused.medium_id,
        )

    media_tuple = tuple(media)
    _validate_nesting(media_tuple)
    block_tuple = tuple(blocks)
    camera = Camera(
        width=width,
        height=height,
        intrinsics=intrinsics,
        world_to_camera=world_to_camera,
        camera_to_world=camera_to_world,
        position=position,
    )
    light = DirectionalLight(
        direction=direction,
        color=color,
        intensity=intensity,
    )
    density_version = _density_version(block_tuple)
    return Scene(
        scene_id=scene_id,
        camera=camera,
        light=light,
        background=background,
        media=media_tuple,
        density_blocks=block_tuple,
        header=header,
        density_version=density_version,
        fingerprint=_scene_fingerprint(scene_id, header, block_tuple),
    )
