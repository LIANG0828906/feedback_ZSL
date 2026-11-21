"""Battle scene data model and the versioned binary scene format."""

from __future__ import annotations

import math
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Sequence

from .errors import fail
from .math3d import EPS, Vec3, vmax, vmin
from .util import f32, sha256_hex

MAGIC = b"TACSCN01"
FORMAT_VERSION = 1
HEADER = struct.Struct("<8sIQIIffIII")
UNIT_RECORD = struct.Struct("<IHH7f")
VERTEX_RECORD = struct.Struct("<3f")
TRIANGLE_RECORD = struct.Struct("<I3I")

UNIT_AABB = 0
UNIT_CAPSULE = 1
MAX_RECORDS = 10_000_000


@dataclass(frozen=True)
class Unit:
    unit_id: int
    kind: str
    aabb_min: Vec3
    aabb_max: Vec3
    capsule_a: Vec3
    capsule_b: Vec3
    radius: float

    @classmethod
    def box(cls, unit_id: int, minimum: Sequence[float], maximum: Sequence[float]) -> "Unit":
        lower = (float(minimum[0]), float(minimum[1]), float(minimum[2]))
        upper = (float(maximum[0]), float(maximum[1]), float(maximum[2]))
        return cls(unit_id, "aabb", lower, upper, (0.0, 0.0, 0.0), (0.0, 0.0, 0.0), 0.0)

    @classmethod
    def capsule(
        cls,
        unit_id: int,
        start: Sequence[float],
        end: Sequence[float],
        radius: float,
    ) -> "Unit":
        a = (float(start[0]), float(start[1]), float(start[2]))
        b = (float(end[0]), float(end[1]), float(end[2]))
        return cls(unit_id, "capsule", (0.0, 0.0, 0.0), (0.0, 0.0, 0.0), a, b, float(radius))

    def bounds(self) -> tuple[Vec3, Vec3]:
        if self.kind == "aabb":
            return self.aabb_min, self.aabb_max
        lower = vmin(self.capsule_a, self.capsule_b)
        upper = vmax(self.capsule_a, self.capsule_b)
        r = self.radius
        return (
            (lower[0] - r, lower[1] - r, lower[2] - r),
            (upper[0] + r, upper[1] + r, upper[2] + r),
        )

    def translated(self, delta: Sequence[float]) -> "Unit":
        dx, dy, dz = float(delta[0]), float(delta[1]), float(delta[2])
        if self.kind == "aabb":
            return Unit.box(
                self.unit_id,
                (f32(self.aabb_min[0] + dx), f32(self.aabb_min[1] + dy), f32(self.aabb_min[2] + dz)),
                (f32(self.aabb_max[0] + dx), f32(self.aabb_max[1] + dy), f32(self.aabb_max[2] + dz)),
            )
        return Unit.capsule(
            self.unit_id,
            (f32(self.capsule_a[0] + dx), f32(self.capsule_a[1] + dy), f32(self.capsule_a[2] + dz)),
            (f32(self.capsule_b[0] + dx), f32(self.capsule_b[1] + dy), f32(self.capsule_b[2] + dz)),
            f32(self.radius),
        )


@dataclass(frozen=True)
class CoverTriangle:
    entity_id: int
    a: Vec3
    b: Vec3
    c: Vec3

    def centroid(self) -> Vec3:
        return ((self.a[0] + self.b[0] + self.c[0]) / 3.0,
                (self.a[1] + self.b[1] + self.c[1]) / 3.0,
                (self.a[2] + self.b[2] + self.c[2]) / 3.0)

    def bounds(self) -> tuple[Vec3, Vec3]:
        return vmin(vmin(self.a, self.b), self.c), vmax(vmax(self.a, self.b), self.c)


@dataclass(frozen=True)
class SceneData:
    version: int
    grid_width: int
    grid_height: int
    cell_size: float
    ground_y: float
    units: tuple[Unit, ...]
    cover_triangles: tuple[CoverTriangle, ...]
    passable_mask: bytes

    @property
    def mask_row_bytes(self) -> int:
        return (self.grid_width + 7) // 8

    @property
    def world_width(self) -> float:
        return self.grid_width * self.cell_size

    @property
    def world_height(self) -> float:
        return self.grid_height * self.cell_size

    def is_walkable(self, x: int, z: int) -> bool:
        if x < 0 or z < 0 or x >= self.grid_width or z >= self.grid_height:
            return False
        byte_index = z * self.mask_row_bytes + (x >> 3)
        bit = x & 7
        return bool(self.passable_mask[byte_index] & (1 << bit))

    def compute_hash(self) -> str:
        chunks: list[bytes] = [
            MAGIC,
            struct.pack(
                "<IQIIffII",
                FORMAT_VERSION,
                self.version,
                self.grid_width,
                self.grid_height,
                float(self.cell_size),
                float(self.ground_y),
                len(self.units),
                len(self.cover_triangles),
            ),
            self.passable_mask,
        ]
        for unit in self.units:
            chunks.append(struct.pack("<I", unit.unit_id))
            chunks.append(unit.kind.encode("ascii"))
            for value in (*unit.aabb_min, *unit.aabb_max, *unit.capsule_a, *unit.capsule_b, unit.radius):
                chunks.append(struct.pack("<d", float(value)))
        for triangle in self.cover_triangles:
            chunks.append(struct.pack("<I", triangle.entity_id))
            for value in (*triangle.a, *triangle.b, *triangle.c):
                chunks.append(struct.pack("<d", float(value)))
        return sha256_hex(b"".join(chunks))

    def to_metadata(self) -> dict[str, object]:
        return {
            "version": self.version,
            "hash": self.compute_hash(),
            "grid": [self.grid_width, self.grid_height],
            "cell_size": self.cell_size,
            "units": len(self.units),
            "cover_triangles": len(self.cover_triangles),
        }


def encode_scene(scene: SceneData) -> bytes:
    if not 0 < scene.grid_width < 2**32 // 8 or not 0 < scene.grid_height < 2**32 // 8:
        fail("E_SCENE_INVALID", "grid dimensions are invalid", "scene.header.grid")
    if not math.isfinite(scene.cell_size) or scene.cell_size <= 0.0:
        fail("E_SCENE_INVALID", "cell size must be positive and finite", "scene.header.cell_size")
    if not math.isfinite(scene.ground_y):
        fail("E_SCENE_INVALID", "ground height must be finite", "scene.header.ground_y")
    if scene.version <= 0:
        fail("E_SCENE_INVALID", "scene version must be positive", "scene.header.version")

    vertex_count = len(scene.cover_triangles) * 3
    chunks = [HEADER.pack(
        MAGIC,
        FORMAT_VERSION,
        scene.version,
        scene.grid_width,
        scene.grid_height,
        float(scene.cell_size),
        float(scene.ground_y),
        len(scene.units),
        vertex_count,
        len(scene.cover_triangles),
    )]
    for unit in scene.units:
        if unit.kind == "aabb":
            kind = UNIT_AABB
            values = (*unit.aabb_min, *unit.aabb_max, 0.0)
        elif unit.kind == "capsule":
            kind = UNIT_CAPSULE
            values = (*unit.capsule_a, *unit.capsule_b, unit.radius)
        else:
            fail("E_SCENE_INVALID", f"unknown unit kind {unit.kind!r}", f"scene.units[{unit.unit_id}].kind")
        chunks.append(UNIT_RECORD.pack(unit.unit_id, kind, 0, *values))

    for triangle in scene.cover_triangles:
        for vertex in (triangle.a, triangle.b, triangle.c):
            chunks.append(VERTEX_RECORD.pack(*vertex))

    for index, triangle in enumerate(scene.cover_triangles):
        base = index * 3
        chunks.append(TRIANGLE_RECORD.pack(triangle.entity_id, base, base + 1, base + 2))
    chunks.append(scene.passable_mask)
    return b"".join(chunks)


def write_scene(path: str | Path, scene: SceneData) -> None:
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    Path(path).write_bytes(encode_scene(scene))


def read_scene(path: str | Path) -> SceneData:
    try:
        data = Path(path).read_bytes()
    except OSError as exc:
        fail("E_SCENE_INVALID", f"cannot read scene: {exc}", "scene.file")
    return decode_scene(data)


def decode_scene(data: bytes) -> SceneData:
    if len(data) < HEADER.size:
        fail(
            "E_SCENE_INVALID",
            "scene header is truncated",
            "scene.header",
            first_conflict={"expected_bytes": HEADER.size, "actual_bytes": len(data)},
        )
    magic, fmt, version, width, height, cell_size, ground_y, unit_count, vertex_count, tri_count = HEADER.unpack_from(data, 0)
    if magic != MAGIC:
        fail("E_SCENE_INVALID", "scene magic mismatch", "scene.header.magic", first_conflict={"magic": magic.hex()})
    if fmt != FORMAT_VERSION:
        fail("E_SCENE_INVALID", "unsupported scene format version", "scene.header.format_version", first_conflict={"format_version": fmt})
    if version <= 0:
        fail("E_SCENE_INVALID", "scene version must be positive", "scene.header.version", first_conflict={"version": version})
    if width <= 0 or height <= 0:
        fail("E_SCENE_INVALID", "grid dimensions must be positive", "scene.header.grid", first_conflict={"width": width, "height": height})
    if not math.isfinite(cell_size) or cell_size <= 0.0:
        fail("E_SCENE_INVALID", "cell size must be positive and finite", "scene.header.cell_size", first_conflict={"cell_size": cell_size})
    if not math.isfinite(ground_y):
        fail("E_SCENE_INVALID", "ground height must be finite", "scene.header.ground_y")
    if max(unit_count, vertex_count, tri_count) > MAX_RECORDS:
        fail("E_SCENE_INVALID", "scene record count exceeds safety limit", "scene.header.counts")
    if tri_count * 3 != vertex_count:
        fail(
            "E_SCENE_INVALID",
            "triangle count does not match indexed vertex count",
            "scene.header.vertex_count",
            first_conflict={"vertex_count": vertex_count, "triangle_count": tri_count},
        )

    row_bytes = (width + 7) // 8
    expected_size = (
        HEADER.size
        + unit_count * UNIT_RECORD.size
        + vertex_count * VERTEX_RECORD.size
        + tri_count * TRIANGLE_RECORD.size
        + height * row_bytes
    )
    if len(data) != expected_size:
        fail(
            "E_SCENE_INVALID",
            "scene payload length mismatch",
            "scene.payload.length",
            first_conflict={"expected_bytes": expected_size, "actual_bytes": len(data)},
        )

    offset = HEADER.size
    units: list[Unit] = []
    for index in range(unit_count):
        unit_id, kind_code, _flags, p0x, p0y, p0z, p1x, p1y, p1z, radius = UNIT_RECORD.unpack_from(data, offset)
        offset += UNIT_RECORD.size
        if not all(math.isfinite(value) for value in (p0x, p0y, p0z, p1x, p1y, p1z, radius)):
            fail(
                "E_SCENE_INVALID",
                "unit bounds contain a non-finite value",
                f"scene.units[{index}].bounds",
                first_conflict={"record_index": index, "unit_id": unit_id},
            )
        if kind_code == UNIT_AABB:
            lower = (p0x, p0y, p0z)
            upper = (p1x, p1y, p1z)
            for axis, name in enumerate(("x", "y", "z")):
                if lower[axis] >= upper[axis]:
                    fail(
                        "E_SCENE_INVALID",
                        "degenerate AABB unit bounds",
                        f"scene.units[{index}].bounds.{name}",
                        first_conflict={"record_index": index, "unit_id": unit_id, "axis": name},
                    )
            units.append(Unit(unit_id, "aabb", lower, upper, (0.0, 0.0, 0.0), (0.0, 0.0, 0.0), 0.0))
        elif kind_code == UNIT_CAPSULE:
            a = (p0x, p0y, p0z)
            b = (p1x, p1y, p1z)
            if radius <= 0.0:
                fail(
                    "E_SCENE_INVALID",
                    "degenerate capsule radius",
                    f"scene.units[{index}].radius",
                    first_conflict={"record_index": index, "unit_id": unit_id},
                )
            if (a[0] - b[0]) ** 2 + (a[1] - b[1]) ** 2 + (a[2] - b[2]) ** 2 <= EPS * EPS:
                fail(
                    "E_SCENE_INVALID",
                    "degenerate capsule segment",
                    f"scene.units[{index}].segment",
                    first_conflict={"record_index": index, "unit_id": unit_id},
                )
            units.append(Unit(unit_id, "capsule", (0.0, 0.0, 0.0), (0.0, 0.0, 0.0), a, b, radius))
        else:
            fail(
                "E_SCENE_INVALID",
                "unknown unit bounds kind",
                f"scene.units[{index}].kind",
                first_conflict={"record_index": index, "unit_id": unit_id, "kind": kind_code},
            )

    vertices: list[Vec3] = []
    for index in range(vertex_count):
        vertex = VERTEX_RECORD.unpack_from(data, offset)
        offset += VERTEX_RECORD.size
        if not all(math.isfinite(value) for value in vertex):
            fail(
                "E_SCENE_INVALID",
                "cover vertex contains a non-finite value",
                f"scene.cover_vertices[{index}]",
                first_conflict={"record_index": index},
            )
        vertices.append((float(vertex[0]), float(vertex[1]), float(vertex[2])))

    triangles: list[CoverTriangle] = []
    for index in range(tri_count):
        entity_id, i0, i1, i2 = TRIANGLE_RECORD.unpack_from(data, offset)
        offset += TRIANGLE_RECORD.size
        for slot, vertex_index in enumerate((i0, i1, i2)):
            if vertex_index >= vertex_count:
                fail(
                    "E_SCENE_INVALID",
                    "cover triangle index is out of range",
                    f"scene.cover_triangles[{index}].indices[{slot}]",
                    first_conflict={
                        "record_index": index,
                        "slot": slot,
                        "index": vertex_index,
                        "vertex_count": vertex_count,
                    },
                )
        a, b, c = vertices[i0], vertices[i1], vertices[i2]
        ab = (b[0] - a[0], b[1] - a[1], b[2] - a[2])
        ac = (c[0] - a[0], c[1] - a[1], c[2] - a[2])
        cx = ab[1] * ac[2] - ab[2] * ac[1]
        cy = ab[2] * ac[0] - ab[0] * ac[2]
        cz = ab[0] * ac[1] - ab[1] * ac[0]
        if cx * cx + cy * cy + cz * cz <= EPS * EPS:
            fail(
                "E_SCENE_INVALID",
                "cover triangle is degenerate",
                f"scene.cover_triangles[{index}]",
                first_conflict={"record_index": index, "entity_id": entity_id},
            )
        triangles.append(CoverTriangle(entity_id, a, b, c))

    mask_size = height * row_bytes
    mask = bytes(data[offset:offset + mask_size])
    if len(mask) != mask_size:
        fail(
            "E_SCENE_INVALID",
            "walkability bitmask length mismatch",
            "scene.passable_mask",
            first_conflict={"expected_bytes": mask_size, "actual_bytes": len(mask)},
        )
    if width % 8:
        unused_mask = (0xFF << (width % 8)) & 0xFF
        for row in range(height):
            byte = mask[row * row_bytes + row_bytes - 1]
            if byte & unused_mask:
                fail(
                    "E_SCENE_INVALID",
                    "walkability bitmask has non-zero unused bits",
                    f"scene.passable_mask.rows[{row}]",
                    first_conflict={"row": row, "unused_bits": byte & unused_mask},
                )

    return SceneData(
        version=version,
        grid_width=width,
        grid_height=height,
        cell_size=float(cell_size),
        ground_y=float(ground_y),
        units=tuple(units),
        cover_triangles=tuple(triangles),
        passable_mask=mask,
    )


def make_passable_mask(width: int, height: int, blocked: Iterable[tuple[int, int]] = ()) -> bytes:
    row_bytes = (width + 7) // 8
    mask = bytearray(row_bytes * height)
    for z in range(height):
        for x in range(width):
            mask[z * row_bytes + (x >> 3)] |= 1 << (x & 7)
    for x, z in blocked:
        if 0 <= x < width and 0 <= z < height:
            mask[z * row_bytes + (x >> 3)] &= ~(1 << (x & 7))
    return bytes(mask)


def set_mask_bit(mask: bytes, width: int, x: int, z: int, walkable: bool) -> bytes:
    row_bytes = (width + 7) // 8
    data = bytearray(mask)
    index = z * row_bytes + (x >> 3)
    if walkable:
        data[index] |= 1 << (x & 7)
    else:
        data[index] &= ~(1 << (x & 7))
    return bytes(data)

