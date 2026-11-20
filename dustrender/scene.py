"""Scene container, binary parser, geometry construction, and validation."""

from __future__ import annotations

import copy
import json
import math
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .density import DensityField, build_density_fields
from .errors import scene_invalid
from .geometry import ray_aabb_interval, ray_sphere_interval
from .util import canonical_json_bytes, sha256_bytes

MAGIC = b"DUSTVOL1"
HEADER_SIZE = 52


def _require(mapping: dict[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        raise scene_invalid(f"{path}.{key}", "required field is missing")
    return mapping[key]


def _finite_number(value: Any, path: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise scene_invalid(path, "must be numeric")
    result = float(value)
    if not math.isfinite(result):
        raise scene_invalid(path, "must be finite")
    if minimum is not None and result < minimum:
        raise scene_invalid(path, f"must be >= {minimum}")
    return result


def _vector3(value: Any, path: str, minimum: float | None = None) -> np.ndarray:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise scene_invalid(path, "must be a three-element numeric array")
    return np.asarray(
        [_finite_number(item, f"{path}[{index}]", minimum) for index, item in enumerate(value)],
        dtype=np.float64,
    )


def _matrix(value: Any, rows: int, cols: int, path: str) -> np.ndarray:
    if not isinstance(value, (list, tuple)) or len(value) != rows:
        raise scene_invalid(path, f"must be a {rows}x{cols} matrix")
    result = []
    for row_index, row in enumerate(value):
        if not isinstance(row, (list, tuple)) or len(row) != cols:
            raise scene_invalid(f"{path}[{row_index}]", f"must contain {cols} numbers")
        result.append(
            [
                _finite_number(item, f"{path}[{row_index}][{column_index}]")
                for column_index, item in enumerate(row)
            ]
        )
    return np.asarray(result, dtype=np.float64)


@dataclass(frozen=True)
class Shape:
    kind: str
    center: np.ndarray
    radius: float | None = None
    minimum: np.ndarray | None = None
    maximum: np.ndarray | None = None

    def intersect(self, origin: np.ndarray, direction: np.ndarray) -> tuple[float, float] | None:
        if self.kind == "sphere":
            return ray_sphere_interval(origin, direction, self.center, float(self.radius))
        return ray_aabb_interval(origin, direction, self.minimum, self.maximum)

    def contains(self, other: "Shape") -> bool:
        return shape_contains(self, other)

    def bounds(self) -> tuple[np.ndarray, np.ndarray]:
        if self.kind == "sphere":
            radius = float(self.radius)
            return self.center - radius, self.center + radius
        return self.minimum.copy(), self.maximum.copy()

    def to_dict(self) -> dict[str, Any]:
        if self.kind == "sphere":
            return {"type": "sphere", "center": self.center.tolist(), "radius": self.radius}
        return {"type": "box", "min": self.minimum.tolist(), "max": self.maximum.tolist()}


def shape_contains(parent: Shape, child: Shape) -> bool:
    child_min, child_max = child.bounds()
    if parent.kind == "box":
        return bool(
            np.all(child_min >= parent.minimum - 1e-8)
            and np.all(child_max <= parent.maximum + 1e-8)
        )
    corners = np.asarray(
        [
            [x, y, z]
            for x in (child_min[0], child_max[0])
            for y in (child_min[1], child_max[1])
            for z in (child_min[2], child_max[2])
        ],
        dtype=np.float64,
    )
    distances = np.linalg.norm(corners - parent.center, axis=1)
    return bool(np.max(distances) <= float(parent.radius) + 1e-8)


@dataclass(frozen=True)
class Medium:
    id: str
    shape: Shape
    scattering: np.ndarray
    absorption: np.ndarray
    phase_g: float
    ior: float
    parent: str | None
    density_field: str | None
    density_scale: float
    density_bias: float

    def intersect(self, origin: np.ndarray, direction: np.ndarray) -> tuple[float, float] | None:
        return self.shape.intersect(origin, direction)

    def density(self, fields: dict[str, DensityField], point: np.ndarray) -> float:
        if self.density_field is None:
            return 1.0
        field = fields[self.density_field]
        return max(0.0, self.density_bias + self.density_scale * field.sample(point))

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.id,
            "shape": self.shape.to_dict(),
            "scattering": self.scattering.tolist(),
            "absorption": self.absorption.tolist(),
            "phase_g": self.phase_g,
            "ior": self.ior,
            "parent": self.parent,
        }
        if self.density_field is not None:
            result["density"] = {
                "field": self.density_field,
                "scale": self.density_scale,
                "bias": self.density_bias,
            }
        return result


@dataclass(frozen=True)
class Camera:
    intrinsics: np.ndarray
    world_to_camera: np.ndarray
    near: float
    far: float
    width: int
    height: int

    @property
    def origin(self) -> np.ndarray:
        rotation = self.world_to_camera[:3, :3]
        translation = self.world_to_camera[:3, 3]
        return -rotation.T @ translation

    @property
    def rotation_world_from_camera(self) -> np.ndarray:
        return self.world_to_camera[:3, :3].T

    def ray(self, x: int, y: int) -> tuple[np.ndarray, np.ndarray]:
        camera_direction = np.asarray(
            [
                (x + 0.5 - self.intrinsics[0, 2]) / self.intrinsics[0, 0],
                (y + 0.5 - self.intrinsics[1, 2]) / self.intrinsics[1, 1],
                -1.0,
            ],
            dtype=np.float64,
        )
        direction = self.rotation_world_from_camera @ camera_direction
        direction /= np.linalg.norm(direction)
        return self.origin, direction


@dataclass(frozen=True)
class Light:
    direction: np.ndarray
    color: np.ndarray
    intensity: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "direction": self.direction.tolist(),
            "color": self.color.tolist(),
            "intensity": self.intensity,
        }


@dataclass
class Scene:
    scene_id: str
    width: int
    height: int
    camera: Camera
    light: Light
    ambient: np.ndarray
    media: tuple[Medium, ...]
    density_fields: dict[str, DensityField]
    metadata: dict[str, Any]
    payload: bytes
    source_sha256: str
    payload_sha256: str
    _serialized_sha256: str | None = None

    @property
    def scene_sha256(self) -> str:
        return self._serialized_sha256 or self.source_sha256

    @property
    def density_version(self) -> dict[str, int]:
        return {name: field.version for name, field in sorted(self.density_fields.items())}

    def to_bytes(self) -> bytes:
        metadata = canonical_json_bytes(self.metadata)
        payload = self.payload
        if len(metadata) > 0xFFFFFFFF or len(payload) > 0xFFFFFFFF:
            raise scene_invalid("$", "scene is too large for the DUSTVOL1 container")
        header = (
            MAGIC
            + struct.pack("<III", 1, len(metadata), len(payload))
            + bytes.fromhex(self.payload_sha256)
        )
        return header + metadata + payload

    def write(self, path: str | Path) -> None:
        from .util import atomic_write_bytes

        atomic_write_bytes(path, self.to_bytes())

    def with_density_block(
        self,
        field_name: str,
        block_index: Sequence[int],
        values: np.ndarray,
        *,
        version_increment: int = 1,
    ) -> "Scene":
        if field_name not in self.density_fields:
            raise scene_invalid("$.density_fields", f"unknown density field {field_name!r}")
        field = self.density_fields[field_name]
        wanted = tuple(int(value) for value in block_index)
        block = next((item for item in field.blocks if item.index == wanted), None)
        if block is None:
            raise scene_invalid(
                "$.density_fields", f"unknown density block {list(wanted)}"
            )
        new_values = np.asarray(values, dtype="<f4")
        if new_values.shape != block.shape:
            raise scene_invalid(
                "$.density_fields", "replacement density block has an incompatible shape"
            )
        if not np.all(np.isfinite(new_values)) or np.any(new_values < 0):
            raise scene_invalid(
                "$.density_fields", "replacement density values must be finite and non-negative"
            )
        metadata = copy.deepcopy(self.metadata)
        raw_field = next(
            item for item in metadata["density_fields"] if item["name"] == field_name
        )
        raw_block = next(
            item for item in raw_field["blocks"] if tuple(item["index"]) == wanted
        )
        raw_block["hash"] = sha256_bytes(new_values.tobytes(order="C"))
        raw_field["version"] = int(raw_field["version"]) + int(version_increment)
        payload = bytearray(self.payload)
        start = int(raw_block["offset"])
        end = start + int(raw_block["count"]) * 4
        payload[start:end] = new_values.tobytes(order="C")
        return scene_from_parts(metadata, bytes(payload), serialized=True)

    def media_bounds(self) -> tuple[np.ndarray, np.ndarray]:
        minima = []
        maxima = []
        for medium in self.media:
            minimum, maximum = medium.shape.bounds()
            minima.append(minimum)
            maxima.append(maximum)
        return np.min(np.stack(minima), axis=0), np.max(np.stack(maxima), axis=0)

    @classmethod
    def load(cls, path: str | Path) -> "Scene":
        try:
            data = Path(path).read_bytes()
        except OSError as exc:
            raise scene_invalid("$", f"cannot read scene: {exc}") from exc
        return cls.from_bytes(data)

    @classmethod
    def from_bytes(cls, data: bytes) -> "Scene":
        if len(data) < HEADER_SIZE:
            raise scene_invalid("$", "truncated DUSTVOL1 header")
        if data[:8] != MAGIC:
            raise scene_invalid("$.magic", "invalid scene signature")
        version, metadata_length, payload_length = struct.unpack_from("<III", data, 8)
        if version != 1:
            raise scene_invalid("$.schema", f"unsupported container version {version}")
        expected_size = HEADER_SIZE + metadata_length + payload_length
        if len(data) != expected_size:
            raise scene_invalid(
                "$.payload",
                "binary size does not match header lengths",
                expected=expected_size,
                actual=len(data),
            )
        expected_payload_hash = data[20:52].hex()
        metadata_bytes = data[HEADER_SIZE : HEADER_SIZE + metadata_length]
        payload = data[HEADER_SIZE + metadata_length :]
        try:
            metadata = json.loads(metadata_bytes.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise scene_invalid("$.metadata", "metadata is not valid UTF-8 JSON") from exc
        scene = scene_from_parts(
            metadata, payload, serialized=False, source_sha256=sha256_bytes(data)
        )
        if scene.payload_sha256 != expected_payload_hash:
            raise scene_invalid(
                "$.payload_sha256",
                "payload SHA-256 does not match the container header",
                expected=expected_payload_hash,
                actual=scene.payload_sha256,
            )
        return scene


def scene_from_parts(
    metadata: dict[str, Any],
    payload: bytes,
    *,
    serialized: bool,
    source_sha256: str | None = None,
) -> Scene:
    if not isinstance(metadata, dict):
        raise scene_invalid("$", "metadata must be a JSON object")
    schema = _require(metadata, "schema", "$")
    if schema != 1:
        raise scene_invalid("$.schema", f"unsupported scene schema {schema!r}")
    scene_id = _require(metadata, "scene_id", "$")
    if not isinstance(scene_id, str) or not scene_id:
        raise scene_invalid("$.scene_id", "must be a non-empty string")

    screen = _require(metadata, "screen", "$")
    if not isinstance(screen, dict):
        raise scene_invalid("$.screen", "must be an object")
    width = _require(screen, "width", "$.screen")
    height = _require(screen, "height", "$.screen")
    for path, value in (("$.screen.width", width), ("$.screen.height", height)):
        if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
            raise scene_invalid(path, "must be a positive integer")
    if width > 16384 or height > 16384:
        raise scene_invalid("$.screen", "screen dimensions are unreasonably large")

    raw_camera = _require(metadata, "camera", "$")
    if not isinstance(raw_camera, dict):
        raise scene_invalid("$.camera", "must be an object")
    intrinsics = _matrix(
        _require(raw_camera, "intrinsics", "$.camera"), 3, 3, "$.camera.intrinsics"
    )
    world_to_camera = _matrix(
        _require(raw_camera, "world_to_camera", "$.camera"),
        4,
        4,
        "$.camera.world_to_camera",
    )
    if abs(float(np.linalg.det(intrinsics))) <= 1e-12:
        raise scene_invalid("$.camera.intrinsics", "camera matrix is not invertible")
    if abs(float(np.linalg.det(world_to_camera))) <= 1e-12:
        raise scene_invalid("$.camera.world_to_camera", "camera matrix is not invertible")
    if abs(intrinsics[0, 0]) <= 1e-12 or abs(intrinsics[1, 1]) <= 1e-12:
        raise scene_invalid("$.camera.intrinsics", "focal lengths must be non-zero")
    near = _finite_number(
        _require(raw_camera, "near", "$.camera"), "$.camera.near", minimum=0.0
    )
    far = _finite_number(
        _require(raw_camera, "far", "$.camera"), "$.camera.far", minimum=0.0
    )
    if near <= 0 or far <= near:
        raise scene_invalid("$.camera.far", "require 0 < near < far")

    raw_light = _require(metadata, "light", "$")
    if not isinstance(raw_light, dict):
        raise scene_invalid("$.light", "must be an object")
    light_direction = _vector3(
        _require(raw_light, "direction", "$.light"), "$.light.direction"
    )
    norm = float(np.linalg.norm(light_direction))
    if norm <= 1e-12:
        raise scene_invalid("$.light.direction", "direction must be non-zero")
    light_direction /= norm
    light_color = _vector3(_require(raw_light, "color", "$.light"), "$.light.color", 0.0)
    intensity = _finite_number(
        _require(raw_light, "intensity", "$.light"), "$.light.intensity", minimum=0.0
    )
    ambient = _vector3(_require(metadata, "ambient", "$"), "$.ambient", 0.0)

    fields = build_density_fields(_require(metadata, "density_fields", "$"), payload)
    raw_media = _require(metadata, "media", "$")
    if not isinstance(raw_media, list):
        raise scene_invalid("$.media", "must be an array")
    media: list[Medium] = []
    ids: set[str] = set()
    for index, raw in enumerate(raw_media):
        mp = f"$.media[{index}]"
        if not isinstance(raw, dict):
            raise scene_invalid(mp, "must be an object")
        medium_id = _require(raw, "id", mp)
        if not isinstance(medium_id, str) or not medium_id:
            raise scene_invalid(f"{mp}.id", "must be a non-empty string")
        if medium_id in ids:
            raise scene_invalid(f"{mp}.id", f"duplicate medium id {medium_id!r}")
        ids.add(medium_id)
        shape = _parse_shape(_require(raw, "shape", mp), f"{mp}.shape")
        scattering = _vector3(
            _require(raw, "scattering", mp), f"{mp}.scattering", minimum=0.0
        )
        absorption = _vector3(
            _require(raw, "absorption", mp), f"{mp}.absorption", minimum=0.0
        )
        phase_g = _finite_number(_require(raw, "phase_g", mp), f"{mp}.phase_g")
        if not -1.0 <= phase_g <= 1.0:
            raise scene_invalid(f"{mp}.phase_g", "must be in [-1, 1]")
        ior = _finite_number(_require(raw, "ior", mp), f"{mp}.ior", minimum=1e-6)
        parent = raw.get("parent")
        if parent is not None and (not isinstance(parent, str) or not parent):
            raise scene_invalid(f"{mp}.parent", "must be null or a non-empty string")
        density_field: str | None = None
        density_scale = 1.0
        density_bias = 0.0
        if "density" in raw:
            density = raw["density"]
            if not isinstance(density, dict):
                raise scene_invalid(f"{mp}.density", "must be an object")
            field_name = _require(density, "field", f"{mp}.density")
            if field_name not in fields:
                raise scene_invalid(
                    f"{mp}.density.field", f"unknown density field {field_name!r}"
                )
            density_field = str(field_name)
            density_scale = _finite_number(
                density.get("scale", 1.0), f"{mp}.density.scale", minimum=0.0
            )
            density_bias = _finite_number(
                density.get("bias", 0.0), f"{mp}.density.bias", minimum=0.0
            )
        media.append(
            Medium(
                id=medium_id,
                shape=shape,
                scattering=scattering,
                absorption=absorption,
                phase_g=phase_g,
                ior=ior,
                parent=parent,
                density_field=density_field,
                density_scale=density_scale,
                density_bias=density_bias,
            )
        )

    by_id = {medium.id: medium for medium in media}
    for index, medium in enumerate(media):
        if medium.parent is None:
            continue
        if medium.parent not in by_id:
            raise scene_invalid(
                f"$.media[{index}].parent",
                f"unknown parent medium {medium.parent!r}",
            )
        snapped = medium.id
        seen = {snapped}
        parent = by_id[medium.parent]
        while True:
            if parent.id in seen:
                raise scene_invalid(
                    f"$.media[{index}].parent", "medium parent hierarchy contains a cycle"
                )
            seen.add(parent.id)
            if parent.parent is None:
                break
            parent = by_id[parent.parent]
        if not by_id[medium.parent].shape.contains(medium.shape):
            raise scene_invalid(
                f"$.media[{index}].parent",
                "child medium is not contained by its parent",
            )

    camera = Camera(
        intrinsics=intrinsics,
        world_to_camera=world_to_camera,
        near=near,
        far=far,
        width=width,
        height=height,
    )
    light = Light(light_direction, light_color, intensity)
    payload_hash = sha256_bytes(payload)
    scene = Scene(
        scene_id=scene_id,
        width=width,
        height=height,
        camera=camera,
        light=light,
        ambient=ambient,
        media=tuple(media),
        density_fields=fields,
        metadata=metadata,
        payload=payload,
        source_sha256=source_sha256 or "",
        payload_sha256=payload_hash,
        _serialized_sha256=sha256_bytes(
            MAGIC
            + struct.pack("<III", 1, len(canonical_json_bytes(metadata)), len(payload))
            + bytes.fromhex(payload_hash)
            + canonical_json_bytes(metadata)
            + payload
        )
        if serialized
        else None,
    )
    if source_sha256 is None:
        scene.source_sha256 = scene.scene_sha256
    return scene


def _parse_shape(raw: Any, path: str) -> Shape:
    if not isinstance(raw, dict):
        raise scene_invalid(path, "must be an object")
    kind = _require(raw, "type", path)
    if kind == "sphere":
        center = _vector3(_require(raw, "center", path), f"{path}.center")
        radius = _finite_number(
            _require(raw, "radius", path), f"{path}.radius", minimum=0.0
        )
        if radius <= 1e-8:
            raise scene_invalid(f"{path}.radius", "sphere radius is degenerate")
        return Shape("sphere", center=center, radius=radius)
    if kind in {"box", "aabb"}:
        if "min" in raw and "max" in raw:
            minimum = _vector3(raw["min"], f"{path}.min")
            maximum = _vector3(raw["max"], f"{path}.max")
        elif "center" in raw and ("half_size" in raw or "half_extents" in raw):
            center = _vector3(raw["center"], f"{path}.center")
            key = "half_size" if "half_size" in raw else "half_extents"
            half = _vector3(raw[key], f"{path}.{key}", minimum=0.0)
            minimum = center - half
            maximum = center + half
        else:
            raise scene_invalid(
                path, "box requires min/max or center with half_size/half_extents"
            )
        if np.any(maximum <= minimum):
            raise scene_invalid(f"{path}.max", "box bounds are degenerate")
        return Shape("box", center=(minimum + maximum) * 0.5, minimum=minimum, maximum=maximum)
    raise scene_invalid(f"{path}.type", "must be 'sphere', 'box', or 'aabb'")


