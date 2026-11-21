from __future__ import annotations

import struct
from dataclasses import dataclass
from typing import Any

import numpy as np

from .hashing import sha256_bytes

ARTIFACT_KINDS = ("mesh", "normals", "density", "preview")
ARTIFACT_ORDER = {name: index for index, name in enumerate(ARTIFACT_KINDS)}

MESH_MAGIC = b"SFMESH01"
NORMAL_MAGIC = b"SFNORM01"
DENSITY_MAGIC = b"SFDENS01"
PREVIEW_MAGIC = b"SFPREV01"


@dataclass(frozen=True)
class Artifact:
    artifact_id: str
    patch_id: str
    kind: str
    data: bytes
    content_hash: str
    parameter_signature: str

    @classmethod
    def create(
        cls,
        patch_id: str,
        kind: str,
        data: bytes,
        parameter_signature: str,
    ) -> "Artifact":
        return cls(
            artifact_id=f"artifact:{patch_id}:{kind}",
            patch_id=patch_id,
            kind=kind,
            data=data,
            content_hash=sha256_bytes(data),
            parameter_signature=parameter_signature,
        )

    def to_manifest_entry(self, dependency_versions: dict[str, str]) -> dict[str, Any]:
        return {
            "artifact_id": self.artifact_id,
            "patch_id": self.patch_id,
            "kind": self.kind,
            "content_hash": self.content_hash,
            "parameter_signature": self.parameter_signature,
            "dependency_versions": dependency_versions,
        }


def encode_mesh(vertices: np.ndarray) -> bytes:
    array = np.ascontiguousarray(vertices, dtype="<f4")
    if array.ndim != 3 or array.shape[2] != 3:
        raise ValueError("mesh vertices must have shape (rows, cols, 3)")
    rows, cols, _ = array.shape
    return MESH_MAGIC + struct.pack("<II", rows, cols) + array.tobytes(order="C")


def decode_mesh(data: bytes) -> tuple[np.ndarray, int, int]:
    if not data.startswith(MESH_MAGIC) or len(data) < 16:
        raise ValueError("invalid mesh artifact")
    rows, cols = struct.unpack_from("<II", data, 8)
    payload = np.frombuffer(data, dtype="<f4", offset=16)
    if payload.size != rows * cols * 3:
        raise ValueError("mesh artifact payload size does not match header")
    return payload.reshape(rows, cols, 3), rows, cols


def encode_normals(normals: np.ndarray) -> bytes:
    array = np.ascontiguousarray(normals, dtype="<f4")
    if array.ndim != 3 or array.shape[2] != 3:
        raise ValueError("normals must have shape (rows, cols, 3)")
    rows, cols, _ = array.shape
    return NORMAL_MAGIC + struct.pack("<II", rows, cols) + array.tobytes(order="C")


def decode_normals(data: bytes) -> tuple[np.ndarray, int, int]:
    if not data.startswith(NORMAL_MAGIC) or len(data) < 16:
        raise ValueError("invalid normals artifact")
    rows, cols = struct.unpack_from("<II", data, 8)
    payload = np.frombuffer(data, dtype="<f4", offset=16)
    if payload.size != rows * cols * 3:
        raise ValueError("normals artifact payload size does not match header")
    return payload.reshape(rows, cols, 3), rows, cols


def encode_density(segments_u: int, segments_v: int) -> bytes:
    return DENSITY_MAGIC + struct.pack("<II", int(segments_u), int(segments_v))


def decode_density(data: bytes) -> tuple[int, int]:
    if not data.startswith(DENSITY_MAGIC) or len(data) != 16:
        raise ValueError("invalid density artifact")
    return struct.unpack_from("<II", data, 8)


def encode_preview(width: int, height: int, rgba: bytes) -> bytes:
    expected = int(width) * int(height) * 4
    if len(rgba) != expected:
        raise ValueError(
            f"preview buffer has {len(rgba)} bytes, expected {expected}"
        )
    return PREVIEW_MAGIC + struct.pack("<II", int(width), int(height)) + rgba


def decode_preview(data: bytes) -> tuple[int, int, bytes]:
    if not data.startswith(PREVIEW_MAGIC) or len(data) < 16:
        raise ValueError("invalid preview artifact")
    width, height = struct.unpack_from("<II", data, 8)
    payload = data[16:]
    if len(payload) != width * height * 4:
        raise ValueError("preview artifact payload size does not match header")
    return width, height, payload


def first_mesh_difference(
    expected: bytes, actual: bytes
) -> tuple[int, int, int, float, float] | None:
    try:
        expected_array, _, _ = decode_mesh(expected)
        actual_array, _, _ = decode_mesh(actual)
    except ValueError:
        return None
    if expected_array.shape != actual_array.shape:
        return None
    different = np.argwhere(expected_array != actual_array)
    if different.size == 0:
        return None
    row, col, component = (int(value) for value in different[0])
    vertex_index = row * expected_array.shape[1] + col
    return (
        vertex_index,
        row,
        col,
        float(expected_array[row, col, component]),
        float(actual_array[row, col, component]),
    )


def compare_artifact_bytes(
    expected: Artifact, actual: Artifact
) -> dict[str, Any] | None:
    if expected.content_hash == actual.content_hash and expected.data == actual.data:
        return None

    details: dict[str, Any] = {
        "artifact_id": expected.artifact_id,
        "expected_hash": expected.content_hash,
        "actual_hash": actual.content_hash,
        "expected_size": len(expected.data),
        "actual_size": len(actual.data),
    }
    if expected.kind in ("mesh", "normals"):
        difference = first_mesh_difference(expected.data, actual.data)
        if difference is not None:
            vertex_index, row, col, expected_value, actual_value = difference
            details.update(
                {
                    "first_difference_vertex": vertex_index,
                    "row": row,
                    "column": col,
                    "expected_component": expected_value,
                    "actual_component": actual_value,
                }
            )
    else:
        limit = min(len(expected.data), len(actual.data))
        byte_index = next(
            (
                index
                for index in range(limit)
                if expected.data[index] != actual.data[index]
            ),
            limit,
        )
        details["first_difference_byte"] = byte_index
    return details
