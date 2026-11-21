"""Self-checking local binary scene and snapshot formats.

The on-disk format is little-endian and deliberately boring.  A payload
checksum catches truncation and byte corruption before a partially decoded
scene can enter the version store.
"""

from __future__ import annotations

import hashlib
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_SCENE_INVALID, E_SNAPSHOT_INVALID, PickError, fail
from .model import Cover, Grid, Scene, Unit, validate_scene

SCENE_MAGIC = b"TPSC"
SNAPSHOT_MAGIC = b"TPSV"
FORMAT_VERSION = 1
HEADER = struct.Struct("<4sHHI32s")
MAX_PAYLOAD = 64 * 1024 * 1024


def _pack_header(magic: bytes, payload: bytes) -> bytes:
    checksum = hashlib.sha256(payload).digest()
    return HEADER.pack(magic, FORMAT_VERSION, 0, len(payload), checksum) + payload


def _unpack_envelope(
    data: bytes,
    *,
    magic: bytes,
    code: str,
    source: str,
) -> bytes:
    if len(data) < HEADER.size:
        fail(
            code,
            "binary envelope is truncated",
            field="header",
            source=source,
            actual=len(data),
            expected_minimum=HEADER.size,
        )
    actual_magic, version, flags, payload_len, checksum = HEADER.unpack_from(data)
    if actual_magic != magic:
        fail(
            code,
            "binary magic does not match the expected format",
            field="header.magic",
            source=source,
            expected=magic.decode("ascii"),
            actual=actual_magic.decode("ascii", "replace"),
        )
    if version != FORMAT_VERSION:
        fail(
            code,
            "unsupported binary format version",
            field="header.version",
            source=source,
            expected=FORMAT_VERSION,
            actual=version,
        )
    if flags != 0:
        fail(
            code,
            "unsupported binary flags",
            field="header.flags",
            source=source,
            actual=flags,
        )
    if payload_len > MAX_PAYLOAD:
        fail(
            code,
            "binary payload exceeds the supported size",
            field="header.payload_length",
            source=source,
            actual=payload_len,
            maximum=MAX_PAYLOAD,
        )
    expected_size = HEADER.size + payload_len
    if len(data) != expected_size:
        fail(
            code,
            "binary envelope length does not match its payload",
            field="header.payload_length",
            source=source,
            expected=expected_size,
            actual=len(data),
        )
    payload = data[HEADER.size :]
    actual_checksum = hashlib.sha256(payload).digest()
    if actual_checksum != checksum:
        fail(
            code,
            "binary payload checksum mismatch",
            field="header.checksum",
            source=source,
            expected=checksum.hex(),
            actual=actual_checksum.hex(),
        )
    return payload


class _Reader:
    def __init__(self, data: bytes, code: str, source: str):
        self.data = data
        self.offset = 0
        self.code = code
        self.source = source

    def take(self, size: int, field: str) -> bytes:
        if size < 0 or self.offset + size > len(self.data):
            fail(
                self.code,
                "binary payload is truncated",
                field=field,
                source=self.source,
                offset=self.offset,
                requested=size,
                remaining=len(self.data) - self.offset,
            )
        value = self.data[self.offset : self.offset + size]
        self.offset += size
        return value

    def unpack(self, fmt: str, field: str) -> tuple[Any, ...]:
        size = struct.calcsize(fmt)
        return struct.unpack(fmt, self.take(size, field))

    def u8(self, field: str) -> int:
        return self.unpack("<B", field)[0]

    def u16(self, field: str) -> int:
        return self.unpack("<H", field)[0]

    def u32(self, field: str) -> int:
        return self.unpack("<I", field)[0]

    def f64(self, field: str) -> float:
        return self.unpack("<d", field)[0]

    def done(self) -> None:
        if self.offset != len(self.data):
            fail(
                self.code,
                "binary payload has trailing bytes",
                field="payload.trailing",
                source=self.source,
                offset=self.offset,
                length=len(self.data),
            )


def _encode_u32(value: int) -> bytes:
    if value < 0 or value > 0xFFFFFFFF:
        raise ValueError("u32 out of range")
    return struct.pack("<I", value)


def encode_scene(scene: Scene) -> bytes:
    validate_scene(scene)
    grid = scene.grid
    parts: list[bytes] = [
        struct.pack(
            "<QdIIdddI",
            scene.structure_version,
            scene.ground_height,
            grid.cols,
            grid.rows,
            grid.cell_size,
            grid.origin_x,
            grid.origin_y,
            len(grid.walkable),
        ),
        grid.walkable,
        _encode_u32(len(scene.vertices)),
    ]
    for vertex in scene.vertices:
        parts.append(struct.pack("<3d", *vertex))
    parts.append(_encode_u32(len(scene.units)))
    for unit in scene.units:
        name = unit.name.encode("utf-8")
        if len(name) > 0xFFFF:
            raise ValueError("unit name is too long")
        bounds_kind = 0 if unit.bounds_kind == "aabb" else 1
        flags = 1 if unit.active else 0
        parts.append(struct.pack("<IBBH", unit.unit_id, bounds_kind, flags, len(name)))
        parts.append(name)
        parts.append(struct.pack("<6d", *unit.minimum, *unit.maximum))
        if bounds_kind == 1:
            assert unit.capsule_a is not None and unit.capsule_b is not None
            parts.append(
                struct.pack(
                    "<7d",
                    *unit.capsule_a,
                    *unit.capsule_b,
                    unit.radius,
                )
            )
    parts.append(_encode_u32(len(scene.covers)))
    for cover in scene.covers:
        name = cover.name.encode("utf-8")
        if len(name) > 0xFFFF:
            raise ValueError("cover name is too long")
        parts.append(
            struct.pack(
                "<IIIIH",
                cover.cover_id,
                cover.i0,
                cover.i1,
                cover.i2,
                len(name),
            )
        )
        parts.append(name)
    payload = b"".join(parts)
    return _pack_header(SCENE_MAGIC, payload)


def decode_scene(data: bytes, *, source: str = "<memory>") -> Scene:
    payload = _unpack_envelope(
        data,
        magic=SCENE_MAGIC,
        code=E_SCENE_INVALID,
        source=source,
    )
    reader = _Reader(payload, E_SCENE_INVALID, source)
    (
        structure_version,
        ground_height,
        cols,
        rows,
        cell_size,
        origin_x,
        origin_y,
        mask_len,
    ) = reader.unpack("<QdIIdddI", "scene.header")
    if cols == 0 or rows == 0:
        fail(
            E_SCENE_INVALID,
            "grid dimensions must be positive",
            field="grid.size",
            source=source,
            cols=cols,
            rows=rows,
        )
    if mask_len > ((cols * rows + 7) // 8) + 1024:
        fail(
            E_SCENE_INVALID,
            "walkable mask length is implausible for the grid",
            field="grid.walkable",
            source=source,
            cols=cols,
            rows=rows,
            mask_len=mask_len,
        )
    mask = reader.take(mask_len, "grid.walkable")
    vertex_count = reader.u32("vertices.count")
    if vertex_count > 1_000_000:
        fail(
            E_SCENE_INVALID,
            "vertex count exceeds the supported limit",
            field="vertices.count",
            source=source,
            actual=vertex_count,
        )
    vertices = tuple(
        (reader.f64(f"vertices[{i}].x"), reader.f64(f"vertices[{i}].y"), reader.f64(f"vertices[{i}].z"))
        for i in range(vertex_count)
    )
    unit_count = reader.u32("units.count")
    if unit_count > 1_000_000:
        fail(
            E_SCENE_INVALID,
            "unit count exceeds the supported limit",
            field="units.count",
            source=source,
            actual=unit_count,
        )
    units: list[Unit] = []
    for index in range(unit_count):
        unit_id, kind, flags, name_len = reader.unpack("<IBBH", f"units[{index}].header")
        if kind not in (0, 1):
            fail(
                E_SCENE_INVALID,
                "unit bounds kind is unknown",
                field=f"units[{index}].bounds_kind",
                entity_id=unit_id,
                source=source,
                actual=kind,
            )
        name_bytes = reader.take(name_len, f"units[{index}].name")
        try:
            name = name_bytes.decode("utf-8")
        except UnicodeDecodeError:
            fail(
                E_SCENE_INVALID,
                "unit name is not valid UTF-8",
                field=f"units[{index}].name",
                entity_id=unit_id,
                source=source,
            )
        minimum = (
            reader.f64(f"units[{index}].minimum.x"),
            reader.f64(f"units[{index}].minimum.y"),
            reader.f64(f"units[{index}].minimum.z"),
        )
        maximum = (
            reader.f64(f"units[{index}].maximum.x"),
            reader.f64(f"units[{index}].maximum.y"),
            reader.f64(f"units[{index}].maximum.z"),
        )
        capsule_a = None
        capsule_b = None
        radius = 0.0
        if kind == 1:
            values = reader.unpack("<7d", f"units[{index}].capsule")
            capsule_a = (values[0], values[1], values[2])
            capsule_b = (values[3], values[4], values[5])
            radius = values[6]
        units.append(
            Unit(
                unit_id=unit_id,
                name=name,
                bounds_kind="aabb" if kind == 0 else "capsule",
                minimum=minimum,
                maximum=maximum,
                capsule_a=capsule_a,
                capsule_b=capsule_b,
                radius=radius,
                active=bool(flags & 1),
            )
        )
    cover_count = reader.u32("covers.count")
    if cover_count > 1_000_000:
        fail(
            E_SCENE_INVALID,
            "cover count exceeds the supported limit",
            field="covers.count",
            source=source,
            actual=cover_count,
        )
    covers: list[Cover] = []
    for index in range(cover_count):
        cover_id, i0, i1, i2, name_len = reader.unpack(
            "<IIIIH", f"covers[{index}].header"
        )
        name_bytes = reader.take(name_len, f"covers[{index}].name")
        try:
            name = name_bytes.decode("utf-8")
        except UnicodeDecodeError:
            fail(
                E_SCENE_INVALID,
                "cover name is not valid UTF-8",
                field=f"covers[{index}].name",
                entity_id=cover_id,
                source=source,
            )
        covers.append(Cover(cover_id, i0, i1, i2, name))
    reader.done()
    scene = Scene(
        structure_version=structure_version,
        ground_height=ground_height,
        grid=Grid(cols, rows, cell_size, origin_x, origin_y, mask),
        vertices=vertices,
        units=tuple(units),
        covers=tuple(covers),
    )
    return validate_scene(scene)


def load_scene(path: str | Path) -> Scene:
    file_path = Path(path)
    try:
        data = file_path.read_bytes()
    except OSError as exc:
        fail(
            E_SCENE_INVALID,
            "scene file could not be read",
            field="file",
            source=str(file_path),
            error=str(exc),
        )
    return decode_scene(data, source=str(file_path))


def save_scene(path: str | Path, scene: Scene) -> None:
    Path(path).write_bytes(encode_scene(scene))


def scene_content_hash(scene: Scene) -> str:
    """Hash the complete validated region content, not the file envelope."""

    return hashlib.sha256(encode_scene(scene)).hexdigest()


@dataclass(frozen=True)
class EncodedSnapshot:
    version: int
    content_hash: str
    scene: Scene


def encode_snapshot(version: int, content_hash: str, scene: Scene) -> bytes:
    if version < 1:
        raise ValueError("snapshot version must be positive")
    scene_bytes = encode_scene(scene)
    if scene_content_hash(scene) != content_hash:
        raise ValueError("snapshot content hash does not match its scene")
    hash_bytes = bytes.fromhex(content_hash)
    payload = struct.pack("<Q32sI", version, hash_bytes, len(scene_bytes)) + scene_bytes
    return _pack_header(SNAPSHOT_MAGIC, payload)


def decode_snapshot(data: bytes, *, source: str = "<memory>") -> EncodedSnapshot:
    payload = _unpack_envelope(
        data,
        magic=SNAPSHOT_MAGIC,
        code=E_SNAPSHOT_INVALID,
        source=source,
    )
    reader = _Reader(payload, E_SNAPSHOT_INVALID, source)
    version, hash_bytes, scene_len = reader.unpack("<Q32sI", "snapshot.header")
    if version < 1:
        fail(
            E_SNAPSHOT_INVALID,
            "snapshot version must be positive",
            field="snapshot.version",
            source=source,
            actual=version,
        )
    scene_bytes = reader.take(scene_len, "snapshot.scene")
    reader.done()
    try:
        scene = decode_scene(scene_bytes, source=f"{source}:scene")
    except PickError as exc:
        raise PickError(
            code=E_SNAPSHOT_INVALID,
            message="snapshot contains an invalid scene",
            location={"field": "snapshot.scene", "source": source, "nested": exc.to_dict()},
            cause_code=exc.code,
        ) from exc
    actual_hash = scene_content_hash(scene)
    declared = hash_bytes.hex()
    if actual_hash != declared:
        fail(
            E_SNAPSHOT_INVALID,
            "snapshot content hash does not match the decoded scene",
            field="snapshot.content_hash",
            source=source,
            expected=declared,
            actual=actual_hash,
        )
    return EncodedSnapshot(version, declared, scene)


def save_snapshot(path: str | Path, version: int, content_hash: str, scene: Scene) -> None:
    Path(path).write_bytes(encode_snapshot(version, content_hash, scene))


def load_snapshot(path: str | Path) -> EncodedSnapshot:
    file_path = Path(path)
    try:
        data = file_path.read_bytes()
    except OSError as exc:
        fail(
            E_SNAPSHOT_INVALID,
            "snapshot file could not be read",
            field="file",
            source=str(file_path),
            error=str(exc),
        )
    return decode_snapshot(data, source=str(file_path))
