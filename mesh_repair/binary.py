from __future__ import annotations

import hashlib
import math
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO

import numpy as np

from .errors import (
    BusinessError,
    E_MESH_INVALID,
    SUB_DEGENERATE_FACE,
    SUB_FACE_COUNT_MISMATCH,
    SUB_INDEX_OUT_OF_RANGE,
    SUB_MALFORMED_HEADER,
    SUB_NONFINITE_COORD,
)
from .mesh import Mesh


MAGIC = b"MSHRP01\0"
FORMAT_VERSION = 1
HEADER_STRUCT = struct.Struct("<8sHBBQQ")
COORD_TYPES = {0: ("<f4", "f32"), 1: ("<f8", "f64")}
INDEX_TYPES = {2: "<u2", 4: "<u4", 8: "<u8"}
COORD_CODES = {name: code for code, (_, name) in COORD_TYPES.items()}
INDEX_WIDTHS = tuple(INDEX_TYPES)


@dataclass(frozen=True)
class MeshHeader:
    version: int
    vertex_count: int
    face_count: int
    coord_code: int
    coord_dtype: str
    coord_type: str
    index_width: int
    index_dtype: str
    payload_offset: int

    def to_dict(self) -> dict[str, object]:
        return {
            "magic": MAGIC.decode("ascii").rstrip("\0"),
            "version": self.version,
            "vertex_count": self.vertex_count,
            "face_count": self.face_count,
            "coord_type": self.coord_type,
            "index_width": self.index_width,
            "header_bytes": self.payload_offset,
        }


def _invalid(
    message: str,
    subcode: str,
    location: dict[str, object] | None = None,
    **context: object,
) -> BusinessError:
    return BusinessError(
        E_MESH_INVALID,
        message,
        subcode=subcode,
        location=location,
        context=dict(context),
    )


def parse_header(buffer: bytes) -> MeshHeader:
    if len(buffer) < HEADER_STRUCT.size:
        raise _invalid("mesh header is truncated", SUB_MALFORMED_HEADER)
    magic, version, coord_code, index_width, vertex_count, face_count = HEADER_STRUCT.unpack_from(
        buffer, 0
    )
    if magic != MAGIC:
        raise _invalid("unrecognized mesh magic", SUB_MALFORMED_HEADER)
    if version != FORMAT_VERSION:
        raise _invalid(
            f"unsupported mesh format version {version}",
            SUB_MALFORMED_HEADER,
            expected=FORMAT_VERSION,
            actual=version,
        )
    if coord_code not in COORD_TYPES:
        raise _invalid(
            f"unsupported coordinate type code {coord_code}",
            SUB_MALFORMED_HEADER,
            field="coord_type",
        )
    if index_width not in INDEX_TYPES:
        raise _invalid(
            f"unsupported index width {index_width}",
            SUB_MALFORMED_HEADER,
            field="index_width",
        )
    return MeshHeader(
        version=version,
        vertex_count=int(vertex_count),
        face_count=int(face_count),
        coord_code=coord_code,
        coord_dtype=COORD_TYPES[coord_code][0],
        coord_type=COORD_TYPES[coord_code][1],
        index_width=index_width,
        index_dtype=INDEX_TYPES[index_width],
        payload_offset=HEADER_STRUCT.size,
    )


def _expected_file_size(header: MeshHeader) -> int:
    vertex_bytes = header.vertex_count * 3 * np.dtype(header.coord_dtype).itemsize
    face_bytes = header.face_count * 3 * header.index_width
    return header.payload_offset + vertex_bytes + face_bytes


def load_mesh(
    path: str | os.PathLike[str],
    *,
    area_epsilon: float = 1e-12,
    validate: bool = True,
) -> tuple[Mesh, MeshHeader]:
    """Load the local MSHRP01 format.

    The whole payload is checked before geometry is returned. Any malformed
    count, index, coordinate or degenerate face maps to E_MESH_INVALID.
    """

    if not math.isfinite(area_epsilon) or area_epsilon < 0:
        raise _invalid(
            "area_epsilon must be finite and non-negative",
            SUB_MALFORMED_HEADER,
            field="area_epsilon",
        )
    file_path = Path(path)
    file_size = file_path.stat().st_size
    with file_path.open("rb") as stream:
        raw_header = stream.read(HEADER_STRUCT.size)
        header = parse_header(raw_header)
        expected_size = _expected_file_size(header)
        if file_size != expected_size:
            raise _invalid(
                "file size does not match header counts",
                SUB_FACE_COUNT_MISMATCH,
                location={"byte_offset": min(file_size, expected_size)},
                expected_bytes=expected_size,
                actual_bytes=file_size,
                header_vertex_count=header.vertex_count,
                header_face_count=header.face_count,
            )
        vertices = np.fromfile(stream, dtype=header.coord_dtype, count=header.vertex_count * 3)
        faces_raw = np.fromfile(stream, dtype=header.index_dtype, count=header.face_count * 3)

    if vertices.size != header.vertex_count * 3:
        raise _invalid("vertex payload is truncated", SUB_MALFORMED_HEADER)
    if faces_raw.size != header.face_count * 3:
        raise _invalid("face payload is truncated", SUB_FACE_COUNT_MISMATCH)

    vertices = vertices.astype(np.float64, copy=False).reshape(-1, 3)
    faces = faces_raw.astype(np.int64, copy=False).reshape(-1, 3)
    mesh = Mesh(vertices, faces)
    if validate:
        validate_mesh(mesh, area_epsilon=area_epsilon)
    return mesh, header


def validate_mesh(mesh: Mesh, *, area_epsilon: float = 1e-12) -> None:
    if mesh.vertex_count == 0:
        raise _invalid("mesh has no vertices", SUB_MALFORMED_HEADER)
    nonfinite = ~np.isfinite(mesh.vertices).all(axis=1)
    if np.any(nonfinite):
        vertex_id = int(np.flatnonzero(nonfinite)[0])
        raise _invalid(
            "mesh contains a non-finite coordinate",
            SUB_NONFINITE_COORD,
            location={"vertex": vertex_id, "component": int(np.flatnonzero(~np.isfinite(mesh.vertices[vertex_id]))[0])},
        )
    if mesh.face_count == 0:
        raise _invalid("mesh has no faces", SUB_MALFORMED_HEADER)

    if np.any(mesh.faces < 0) or np.any(mesh.faces >= mesh.vertex_count):
        bad_rows = np.flatnonzero(
            np.any((mesh.faces < 0) | (mesh.faces >= mesh.vertex_count), axis=1)
        )
        face_id = int(bad_rows[0])
        vertex_column = int(
            np.flatnonzero(
                (mesh.faces[face_id] < 0) | (mesh.faces[face_id] >= mesh.vertex_count)
            )[0]
        )
        raise _invalid(
            "face index is outside the vertex range",
            SUB_INDEX_OUT_OF_RANGE,
            location={"face": face_id, "corner": vertex_column},
            index=int(mesh.faces[face_id, vertex_column]),
            vertex_count=mesh.vertex_count,
        )

    repeated = (
        (mesh.faces[:, 0] == mesh.faces[:, 1])
        | (mesh.faces[:, 1] == mesh.faces[:, 2])
        | (mesh.faces[:, 2] == mesh.faces[:, 0])
    )
    if np.any(repeated):
        face_id = int(np.flatnonzero(repeated)[0])
        raise _invalid(
            "face has a repeated vertex",
            SUB_DEGENERATE_FACE,
            location={"face": face_id},
        )

    if area_epsilon > 0:
        tri = mesh.vertices[mesh.faces]
        cross = np.cross(tri[:, 1] - tri[:, 0], tri[:, 2] - tri[:, 0])
        areas = np.linalg.norm(cross, axis=1)
        degenerate = areas <= area_epsilon
        if np.any(degenerate):
            face_id = int(np.flatnonzero(degenerate)[0])
            raise _invalid(
                "face area is below the degeneracy threshold",
                SUB_DEGENERATE_FACE,
                location={"face": face_id},
                area=float(areas[face_id]),
                area_epsilon=area_epsilon,
            )


def encode_mesh(
    mesh: Mesh,
    *,
    coord_type: str = "f64",
    index_width: int = 4,
    area_epsilon: float = 1e-12,
) -> bytes:
    validate_mesh(mesh, area_epsilon=area_epsilon)
    if coord_type not in COORD_CODES:
        raise ValueError(f"coord_type must be one of {sorted(COORD_CODES)}")
    if index_width not in INDEX_TYPES:
        raise ValueError(f"index_width must be one of {INDEX_WIDTHS}")
    if mesh.vertex_count > (1 << (8 * index_width)):
        raise ValueError("index width cannot represent vertex count")
    coord_code = COORD_CODES[coord_type]
    header = HEADER_STRUCT.pack(
        MAGIC,
        FORMAT_VERSION,
        coord_code,
        index_width,
        mesh.vertex_count,
        mesh.face_count,
    )
    vertex_dtype = COORD_TYPES[coord_code][0]
    index_dtype = INDEX_TYPES[index_width]
    return (
        header
        + np.asarray(mesh.vertices, dtype=vertex_dtype).tobytes(order="C")
        + np.asarray(mesh.faces, dtype=index_dtype).tobytes(order="C")
    )


def atomic_write_bytes(path: str | os.PathLike[str], data: bytes) -> str:
    """Write bytes through a same-directory temp file and atomic rename."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    sha = hashlib.sha256(data).hexdigest()
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.tmp.", dir=str(target.parent)
    )
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, target)
        _fsync_directory(target.parent)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise
    return sha


def write_mesh(
    path: str | os.PathLike[str],
    mesh: Mesh,
    *,
    coord_type: str = "f64",
    index_width: int = 4,
    area_epsilon: float = 1e-12,
    atomic: bool = True,
) -> str:
    payload = encode_mesh(
        mesh,
        coord_type=coord_type,
        index_width=index_width,
        area_epsilon=area_epsilon,
    )
    if atomic:
        return atomic_write_bytes(path, payload)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("wb") as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())
    return hashlib.sha256(payload).hexdigest()


def file_sha256(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        while chunk := stream.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def _fsync_directory(path: Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
