"""Custom binary scene container.

The format is intentionally small, explicitly versioned and fully validated
before a renderer is constructed.  See ``docs/FORMAT.md`` for the byte layout.
"""

from __future__ import annotations

import math
import struct
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError, ErrorCode, SceneError
from .model import (
    MEDIUM_BOX,
    MEDIUM_SPHERE,
    Camera,
    DensityField,
    DirectionalLight,
    Medium,
    Scene,
)

MAGIC = b"VSCN0001"
VERSION = 1
HEADER_SIZE = 276
MEDIA_RECORD_SIZE = 132
DENSITY_RECORD_SIZE = 32
CHUNK_RECORD_SIZE = 28
MAX_SCREEN_DIMENSION = 16384
MAX_MEDIA = 4096
MAX_DENSITY_FIELDS = 1024
MAX_CHUNKS = 2_000_000


def _check_range(data: bytes, offset: int, size: int, field: str) -> None:
    if offset < 0 or size < 0 or offset + size > len(data):
        raise SceneError(
            "truncated binary scene",
            field,
            offset,
            details={"required_bytes": offset + size, "actual_bytes": len(data)},
        )


def _unpack(
    data: bytes,
    offset: int,
    fmt: str,
    field: str,
    truncation_code: str = ErrorCode.SCENE_INVALID,
) -> tuple[Any, ...]:
    size = struct.calcsize(fmt)
    if offset < 0 or offset + size > len(data):
        raise SceneError(
            "truncated field",
            field,
            offset,
            code=truncation_code,
            details={"required_bytes": offset + size, "actual_bytes": len(data)},
        )
    return struct.unpack_from(fmt, data, offset)


def decode_scene(data: bytes) -> Scene:
    """Decode without semantic validation; ``load_scene`` validates."""

    data = bytes(data)
    _check_range(data, 0, 8, "magic")
    if data[:8] != MAGIC:
        raise SceneError(
            "invalid scene magic",
            "magic",
            0,
            details={"expected": MAGIC.decode("ascii"), "actual": data[:8].hex()},
        )
    _check_range(data, 0, HEADER_SIZE, "header")
    version = _unpack(data, 8, "<I", "version")[0]
    if version != VERSION:
        raise SceneError(
            "unsupported scene version",
            "version",
            8,
            details={"expected": VERSION, "actual": version},
        )
    width, height, media_count, density_count = _unpack(
        data, 12, "<IIII", "dimensions_and_counts"
    )
    if width <= 0 or height <= 0 or width > MAX_SCREEN_DIMENSION or height > MAX_SCREEN_DIMENSION:
        raise SceneError("invalid screen dimensions", "dimensions", 12)
    if media_count > MAX_MEDIA:
        raise SceneError("media count exceeds safety limit", "media_count", 20)
    if density_count > MAX_DENSITY_FIELDS:
        raise SceneError("density field count exceeds safety limit", "density_count", 24)

    light_values = _unpack(data, 28, "<6d", "light")
    light = DirectionalLight(
        direction=np.asarray(light_values[:3], dtype=np.float64),
        radiance=np.asarray(light_values[3:], dtype=np.float64),
    )
    intrinsics_values = _unpack(data, 76, "<9d", "camera.intrinsics")
    world_to_camera_values = _unpack(data, 148, "<16d", "camera.world_to_camera")
    camera = Camera(
        intrinsics=np.asarray(intrinsics_values, dtype=np.float64).reshape(3, 3),
        world_to_camera=np.asarray(world_to_camera_values, dtype=np.float64).reshape(
            4, 4
        ),
    )

    metadata_size = (
        HEADER_SIZE
        + media_count * MEDIA_RECORD_SIZE
        + density_count * DENSITY_RECORD_SIZE
    )
    _check_range(data, HEADER_SIZE, metadata_size - HEADER_SIZE, "metadata")
    media: list[Medium] = []
    cursor = HEADER_SIZE
    for index in range(media_count):
        record_offset = cursor
        (
            medium_id,
            kind,
            density_field_id,
            ss0,
            ss1,
            ss2,
            sa0,
            sa1,
            sa2,
            g,
            density_scale,
            a0,
            a1,
            a2,
            b0,
            b1,
            b2,
            b3,
        ) = _unpack(data, cursor, "<IB3xI15d", f"media[{index}]")
        cursor += MEDIA_RECORD_SIZE
        media.append(
            Medium(
                id=medium_id,
                kind=kind,
                density_field_id=density_field_id,
                sigma_s=np.array([ss0, ss1, ss2], dtype=np.float64),
                sigma_a=np.array([sa0, sa1, sa2], dtype=np.float64),
                g=g,
                density_scale=density_scale,
                shape_a=np.array([a0, a1, a2], dtype=np.float64),
                shape_b=np.array([b0, b1, b2, b3], dtype=np.float64),
                source_offset=record_offset,
            )
        )

    density_headers: list[tuple[int, tuple[int, int, int], tuple[int, int, int], int, int]] = []
    for index in range(density_count):
        record_offset = cursor
        field_id, nx, ny, nz, cx, cy, cz, chunk_count = _unpack(
            data, cursor, "<I7I", f"density_fields[{index}]"
        )
        cursor += DENSITY_RECORD_SIZE
        density_headers.append(
            ((field_id), (nx, ny, nz), (cx, cy, cz), chunk_count, record_offset)
        )

    total_chunks = sum(header[3] for header in density_headers)
    if total_chunks > MAX_CHUNKS:
        raise SceneError("chunk count exceeds safety limit", "density_chunks", cursor)
    _check_range(
        data,
        cursor,
        total_chunks * CHUNK_RECORD_SIZE,
        "density_chunk_index",
    )
    chunk_records: list[list[tuple[tuple[int, int, int], int, int, int]]] = [
        [] for _ in density_headers
    ]
    for field_index, header in enumerate(density_headers):
        expected_chunks = header[3]
        for _ in range(expected_chunks):
            record_offset = cursor
            cx, cy, cz, payload_offset, payload_length = _unpack(
                data,
                cursor,
                "<IIIQQ",
                f"density_fields[{field_index}].chunk_index",
                truncation_code=ErrorCode.DENSITY_TRUNCATED,
            )
            cursor += CHUNK_RECORD_SIZE
            chunk_records[field_index].append(
                ((cx, cy, cz), payload_offset, payload_length, record_offset)
            )

    density_fields: dict[int, DensityField] = {}
    payload_min = cursor
    payload_ranges: list[tuple[int, int, str]] = []
    for field_index, header in enumerate(density_headers):
        field_id, dims, chunk_shape, _chunk_count, record_offset = header
        if field_id in density_fields:
            raise SceneError(
                "duplicate density field id",
                f"density_fields[{field_id}].id",
                record_offset,
            )
        chunks: dict[tuple[int, int, int], np.ndarray] = {}
        chunk_offsets: dict[tuple[int, int, int], int] = {}
        for key, payload_offset, payload_length, _index_offset in chunk_records[field_index]:
            if key in chunks:
                raise SceneError(
                    "duplicate density chunk index",
                    f"density_fields[{field_index}].chunks[{key}]",
                    _index_offset,
                    code=ErrorCode.CHUNK_INDEX,
                )
            chunk_counts = tuple(
                (dims[i] + chunk_shape[i] - 1) // chunk_shape[i] for i in range(3)
            )
            if any(key[i] < 0 or key[i] >= chunk_counts[i] for i in range(3)):
                raise SceneError(
                    "density chunk index is outside the declared range",
                    f"density_fields[{field_index}].chunks[{key}]",
                    _index_offset,
                    code=ErrorCode.CHUNK_INDEX,
                    details={"chunk_counts": list(chunk_counts)},
                )
            expected = int(np.prod([min(chunk_shape[i], dims[i] - key[i] * chunk_shape[i]) for i in range(3)]))
            expected_bytes = expected * 8
            if payload_offset < 0 or payload_length != expected_bytes:
                raise SceneError(
                    "density chunk payload length mismatch",
                    f"density_fields[{field_index}].chunks[{key}]",
                    payload_offset,
                    code=ErrorCode.DENSITY_TRUNCATED,
                    details={
                        "expected_bytes": expected_bytes,
                        "actual_bytes": payload_length,
                    },
                )
            if payload_offset < payload_min:
                raise SceneError(
                    "density chunk payload overlaps metadata",
                    f"density_fields[{field_index}].chunks[{key}]",
                    payload_offset,
                    code=ErrorCode.CHUNK_INDEX,
                    details={"metadata_end": payload_min},
                )
            if payload_offset + payload_length > len(data):
                raise SceneError(
                    "truncated density chunk payload",
                    f"density_fields[{field_index}].chunks[{key}]",
                    payload_offset,
                    code=ErrorCode.DENSITY_TRUNCATED,
                    details={
                        "required_bytes": payload_offset + payload_length,
                        "actual_bytes": len(data),
                    },
                )
            payload_ranges.append(
                (
                    payload_offset,
                    payload_offset + payload_length,
                    f"density_fields[{field_index}].chunks[{key}]",
                )
            )
            raw = np.frombuffer(
                data,
                dtype="<f8",
                count=expected,
                offset=payload_offset,
            ).astype(np.float64, copy=True)
            nx = min(chunk_shape[0], dims[0] - key[0] * chunk_shape[0])
            ny = min(chunk_shape[1], dims[1] - key[1] * chunk_shape[1])
            nz = min(chunk_shape[2], dims[2] - key[2] * chunk_shape[2])
            chunks[key] = raw.reshape((nz, ny, nx))
            chunk_offsets[key] = _index_offset
        density_fields[field_id] = DensityField(
            id=field_id,
            dims=dims,
            chunk_shape=chunk_shape,
            chunks=chunks,
            source_offset=record_offset,
            chunk_offsets=chunk_offsets,
        )
    payload_ranges.sort()
    for previous, current in zip(payload_ranges[:-1], payload_ranges[1:]):
        if current[0] < previous[1]:
            raise SceneError(
                "density chunk payloads overlap",
                current[2],
                current[0],
                code=ErrorCode.CHUNK_INDEX,
                details={"previous_field": previous[2]},
            )

    return Scene(
        width=width,
        height=height,
        camera=camera,
        light=light,
        media=media,
        density_fields=density_fields,
        declared_media_count=media_count,
        declared_density_count=density_count,
        source_size=len(data),
    )


def _medium_bbox_contains(outer: Medium, inner: Medium) -> bool:
    if outer.kind == MEDIUM_BOX:
        lo, hi = outer.bounds
        if inner.kind == MEDIUM_BOX:
            ilo, ihi = inner.bounds
            return bool(np.all(ilo >= lo - 1e-12) and np.all(ihi <= hi + 1e-12))
        center = inner.shape_a
        radius = float(inner.shape_b[0])
        return bool(
            np.all(center - radius >= lo - 1e-12)
            and np.all(center + radius <= hi + 1e-12)
        )
    center = outer.shape_a
    radius = float(outer.shape_b[0])
    if inner.kind == MEDIUM_SPHERE:
        return bool(
            np.linalg.norm(inner.shape_a - center) + float(inner.shape_b[0])
            <= radius + 1e-12
        )
    lo, hi = inner.bounds
    corners = np.array(
        [
            [x, y, z]
            for x in (lo[0], hi[0])
            for y in (lo[1], hi[1])
            for z in (lo[2], hi[2])
        ],
        dtype=np.float64,
    )
    return bool(np.all(np.linalg.norm(corners - center, axis=1) <= radius + 1e-12))


def _bboxes_overlap(a: Medium, b: Medium) -> bool:
    alo, ahi = a.bounds
    blo, bhi = b.bounds
    return bool(np.all(alo <= bhi + 1e-12) and np.all(blo <= ahi + 1e-12))


def validate_scene(scene: Scene) -> None:
    if len(scene.media) != scene.declared_media_count:
        raise SceneError(
            "declared media entry is missing",
            "media",
            -1,
            code=ErrorCode.MEDIA_MISSING,
            details={
                "declared": scene.declared_media_count,
                "actual": len(scene.media),
            },
        )
    if len(scene.density_fields) != scene.declared_density_count:
        raise SceneError(
            "declared density field entry is missing",
            "density_fields",
            -1,
            code=ErrorCode.SCENE_INVALID,
            details={
                "declared": scene.declared_density_count,
                "actual": len(scene.density_fields),
            },
        )

    k = scene.camera.intrinsics
    view = scene.camera.world_to_camera
    if not np.all(np.isfinite(k)) or not np.all(np.isfinite(view)):
        raise SceneError(
            "camera matrix contains a non-finite value",
            "camera.intrinsics/world_to_camera",
            76,
            code=ErrorCode.CAMERA_NONFINITE,
        )
    det_k = float(np.linalg.det(k))
    det_view = float(np.linalg.det(view))
    if abs(det_k) < 1e-12 or abs(det_view) < 1e-12:
        raise SceneError(
            "camera matrix is singular",
            "camera",
            76,
            details={"det_intrinsics": det_k, "det_world_to_camera": det_view},
        )
    try:
        cond_k = float(np.linalg.cond(k))
        cond_view = float(np.linalg.cond(view))
    except np.linalg.LinAlgError as exc:
        raise SceneError("camera matrix cannot be decomposed", "camera", 76) from exc
    if not math.isfinite(cond_k) or not math.isfinite(cond_view):
        raise SceneError("camera matrix is numerically degenerate", "camera", 76)
    if cond_k > 1e14 or cond_view > 1e14:
        raise SceneError(
            "camera matrix is ill-conditioned",
            "camera",
            76,
            details={"cond_intrinsics": cond_k, "cond_world_to_camera": cond_view},
        )
    if not np.allclose(view[3], np.array([0.0, 0.0, 0.0, 1.0]), atol=1e-9):
        raise SceneError(
            "world_to_camera last row must be [0, 0, 0, 1]",
            "camera.world_to_camera",
            148 + 12 * 8,
        )
    if abs(k[2, 2]) < 1e-12:
        raise SceneError("invalid intrinsics homogeneous row", "camera.intrinsics", 76)

    light_dir = scene.light.direction
    radiance = scene.light.radiance
    if not np.all(np.isfinite(light_dir)) or not np.all(np.isfinite(radiance)):
        raise SceneError("light contains a non-finite value", "light", 28)
    if np.linalg.norm(light_dir) <= 1e-12:
        raise SceneError("light direction is degenerate", "light.direction", 28)
    if np.any(radiance < 0.0):
        raise SceneError("light radiance must be non-negative", "light.radiance", 52)

    field_ids: set[int] = set()
    for field_index, field in scene.density_fields.items():
        if field_index != field.id:
            raise SceneError(
                "density field map key does not match field id",
                f"density_fields[{field_index}].id",
                field.source_offset,
            )
        if field.id in field_ids:
            raise SceneError(
                "duplicate density field id",
                f"density_fields[{field.id}]",
                field.source_offset,
            )
        field_ids.add(field.id)
        if any(v <= 0 for v in field.dims) or any(v <= 0 for v in field.chunk_shape):
            raise SceneError(
                "density field dimensions and chunk shape must be positive",
                f"density_fields[{field.id}]",
                field.source_offset,
            )
        expected_keys = set(field.chunk_keys())
        actual_keys = set(field.chunks)
        if actual_keys != expected_keys:
            missing = sorted(expected_keys - actual_keys)
            extra = sorted(actual_keys - expected_keys)
            location = field.source_offset
            if extra:
                location = field.chunk_offsets.get(extra[0], field.source_offset)
            elif missing:
                location = field.chunk_offsets.get(missing[0], field.source_offset)
            raise SceneError(
                "density chunk index does not cover the declared range",
                f"density_fields[{field.id}].chunks",
                location,
                code=ErrorCode.CHUNK_INDEX,
                details={
                    "missing": missing[:8],
                    "extra": extra[:8],
                    "chunk": list(extra[0] if extra else missing[0]),
                },
            )
        for key, chunk in field.chunks.items():
            expected_shape = tuple(reversed(field.chunk_extent(key)))
            if tuple(chunk.shape) != expected_shape:
                raise SceneError(
                    "density chunk has an invalid shape",
                    f"density_fields[{field.id}].chunks[{key}]",
                    field.chunk_offsets.get(key, field.source_offset),
                    code=ErrorCode.CHUNK_INDEX,
                    details={
                        "expected_shape": list(expected_shape),
                        "actual_shape": list(chunk.shape),
                    },
                )
            if not np.all(np.isfinite(chunk)):
                raise SceneError(
                    "density values must be finite",
                    f"density_fields[{field.id}].chunks[{key}]",
                    field.chunk_offsets.get(key, field.source_offset),
                )
            if np.any(chunk < 0.0):
                raise SceneError(
                    "density values must be non-negative",
                    f"density_fields[{field.id}].chunks[{key}]",
                    field.chunk_offsets.get(key, field.source_offset),
                )
        field.materialize()

    medium_ids: set[int] = set()
    for index, medium in enumerate(scene.media):
        path = f"media[{index}]"
        if medium.id in medium_ids:
            raise SceneError(
                "duplicate medium id",
                f"{path}.id",
                medium.source_offset,
                details={"id": medium.id},
            )
        medium_ids.add(medium.id)
        if medium.kind not in (MEDIUM_BOX, MEDIUM_SPHERE):
            raise SceneError(
                "unsupported medium kind",
                f"{path}.kind",
                medium.source_offset + 4,
                details={"kind": medium.kind},
            )
        if medium.density_field_id not in field_ids:
            raise SceneError(
                "medium references a missing density field",
                f"{path}.density_field_id",
                medium.source_offset + 8,
                details={"density_field_id": medium.density_field_id},
            )
        for name, value in (
            ("sigma_s", medium.sigma_s),
            ("sigma_a", medium.sigma_a),
            ("shape_a", medium.shape_a),
            ("shape_b", medium.shape_b),
        ):
            if not np.all(np.isfinite(value)):
                raise SceneError(
                    f"medium {name} contains a non-finite value",
                    f"{path}.{name}",
                    medium.source_offset,
                )
        if np.any(medium.sigma_s < 0.0) or np.any(medium.sigma_a < 0.0):
            raise SceneError(
                "medium coefficients must be non-negative",
                f"{path}.sigma",
                medium.source_offset,
            )
        if not math.isfinite(medium.g) or not -1.0 < medium.g < 1.0:
            raise SceneError(
                "Henyey-Greenstein g must be finite and in (-1, 1)",
                f"{path}.g",
                medium.source_offset + 60,
            )
        if not math.isfinite(medium.density_scale) or medium.density_scale < 0.0:
            raise SceneError(
                "density scale must be finite and non-negative",
                f"{path}.density_scale",
                medium.source_offset + 68,
            )
        if medium.kind == MEDIUM_BOX:
            lo, hi = medium.bounds
            if np.any(hi <= lo):
                raise SceneError(
                    "box medium is degenerate",
                    f"{path}.shape",
                    medium.source_offset + 76,
                    details={"min": lo.tolist(), "max": hi.tolist()},
                )
        else:
            if float(medium.shape_b[0]) <= 0.0:
                raise SceneError(
                    "sphere radius must be positive",
                    f"{path}.shape_b[0]",
                    medium.source_offset + 100,
                )

    for i in range(len(scene.media)):
        for j in range(i + 1, len(scene.media)):
            a = scene.media[i]
            b = scene.media[j]
            if not _bboxes_overlap(a, b):
                continue
            if _medium_bbox_contains(a, b) or _medium_bbox_contains(b, a):
                continue
            raise SceneError(
                "overlapping media must be strictly nested",
                f"media[{i},{j}].nesting",
                min(a.source_offset, b.source_offset),
                details={"medium_ids": [a.id, b.id]},
            )


def load_scene(source: bytes | bytearray | memoryview | str | Path) -> Scene:
    if isinstance(source, (str, Path)):
        data = Path(source).read_bytes()
    else:
        data = bytes(source)
    scene = decode_scene(data)
    validate_scene(scene)
    return scene


def _pack_medium(medium: Medium) -> bytes:
    if medium.kind == MEDIUM_BOX:
        shape_b = np.array(
            [medium.shape_b[0], medium.shape_b[1], medium.shape_b[2], 0.0],
            dtype=np.float64,
        )
    else:
        shape_b = np.array(
            [medium.shape_b[0], 0.0, 0.0, 0.0], dtype=np.float64
        )
    return struct.pack(
        "<IB3xI15d",
        int(medium.id),
        int(medium.kind),
        int(medium.density_field_id),
        *[float(v) for v in medium.sigma_s],
        *[float(v) for v in medium.sigma_a],
        float(medium.g),
        float(medium.density_scale),
        *[float(v) for v in medium.shape_a],
        *[float(v) for v in shape_b],
    )


def build_scene(scene: Scene) -> bytes:
    """Serialize a scene into the validated binary container."""

    validate_scene(scene)
    fields = [scene.density_fields[key] for key in sorted(scene.density_fields)]
    media = list(scene.media)
    metadata_size = (
        HEADER_SIZE
        + len(media) * MEDIA_RECORD_SIZE
        + len(fields) * DENSITY_RECORD_SIZE
        + sum(len(field.chunks) for field in fields) * CHUNK_RECORD_SIZE
    )
    payloads: list[bytes] = []
    chunk_entries: list[tuple[tuple[int, int, int], int, int]] = []
    cursor = metadata_size
    for field in fields:
        for key in sorted(field.chunks, key=lambda value: (value[2], value[1], value[0])):
            chunk = np.ascontiguousarray(field.chunks[key], dtype="<f8")
            payload = chunk.tobytes(order="C")
            chunk_entries.append((key, cursor, len(payload)))
            payloads.append(payload)
            cursor += len(payload)

    output = bytearray()
    output += MAGIC
    output += struct.pack(
        "<IIIII",
        VERSION,
        int(scene.width),
        int(scene.height),
        len(media),
        len(fields),
    )
    output += struct.pack(
        "<6d",
        *[float(v) for v in scene.light.direction],
        *[float(v) for v in scene.light.radiance],
    )
    output += struct.pack(
        "<9d", *[float(v) for v in scene.camera.intrinsics.reshape(-1)]
    )
    output += struct.pack(
        "<16d", *[float(v) for v in scene.camera.world_to_camera.reshape(-1)]
    )
    for medium in media:
        output += _pack_medium(medium)
    for field in fields:
        output += struct.pack(
            "<I7I",
            int(field.id),
            int(field.dims[0]),
            int(field.dims[1]),
            int(field.dims[2]),
            int(field.chunk_shape[0]),
            int(field.chunk_shape[1]),
            int(field.chunk_shape[2]),
            len(field.chunks),
        )
    entry_cursor = 0
    for field in fields:
        for key in sorted(field.chunks, key=lambda value: (value[2], value[1], value[0])):
            entry_key, payload_offset, payload_length = chunk_entries[entry_cursor]
            entry_cursor += 1
            if entry_key != key:
                raise AssertionError("chunk ordering mismatch")
            output += struct.pack(
                "<IIIQQ",
                int(key[0]),
                int(key[1]),
                int(key[2]),
                int(payload_offset),
                int(payload_length),
            )
    for payload in payloads:
        output += payload
    if len(output) != cursor:
        raise AssertionError("binary scene size mismatch")
    return bytes(output)


__all__ = [
    "MAGIC",
    "VERSION",
    "build_scene",
    "decode_scene",
    "load_scene",
    "validate_scene",
    "BusinessError",
]
