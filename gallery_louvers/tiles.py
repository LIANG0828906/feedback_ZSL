"""Deterministic block container used by progress snapshots."""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .util import atomic_write_bytes, canonical_json_bytes, sha256_bytes

TILE_MAGIC = b"GLVBLK1\n"
TILE_VERSION = 1


@dataclass
class TileData:
    header: dict[str, Any]
    rgba: np.ndarray
    owner_id: np.ndarray
    owner_layer: np.ndarray


def tile_bytes(
    rgba: np.ndarray,
    owner_id: np.ndarray,
    owner_layer: np.ndarray,
    metadata: dict[str, Any],
) -> bytes:
    arrays = {
        "rgba": np.ascontiguousarray(rgba.astype("<f4", copy=False)),
        "owner_id": np.ascontiguousarray(owner_id.astype("<i4", copy=False)),
        "owner_layer": np.ascontiguousarray(owner_layer.astype("u1", copy=False)),
    }
    payload_parts: list[bytes] = []
    descriptors: dict[str, Any] = {}
    offset = 0
    for name in ("rgba", "owner_id", "owner_layer"):
        array = arrays[name]
        raw = array.tobytes(order="C")
        descriptors[name] = {
            "dtype": array.dtype.str,
            "shape": list(array.shape),
            "offset": offset,
            "nbytes": len(raw),
            "sha256": sha256_bytes(raw),
        }
        payload_parts.append(raw)
        offset += len(raw)
    header = {
        "tile_version": TILE_VERSION,
        "metadata": metadata,
        "arrays": descriptors,
        "payload_nbytes": offset,
    }
    header_bytes = canonical_json_bytes(header)
    return TILE_MAGIC + struct.pack("<Q", len(header_bytes)) + header_bytes + b"".join(
        payload_parts
    )


def write_tile(
    path: str | Path,
    rgba: np.ndarray,
    owner_id: np.ndarray,
    owner_layer: np.ndarray,
    metadata: dict[str, Any],
) -> str:
    data = tile_bytes(rgba, owner_id, owner_layer, metadata)
    atomic_write_bytes(path, data)
    return sha256_bytes(data)


def parse_tile(data: bytes) -> TileData:
    if not data.startswith(TILE_MAGIC) or len(data) < len(TILE_MAGIC) + 8:
        raise ValueError("invalid tile magic")
    cursor = len(TILE_MAGIC)
    (header_size,) = struct.unpack("<Q", data[cursor : cursor + 8])
    cursor += 8
    header_end = cursor + int(header_size)
    if header_end > len(data):
        raise ValueError("tile header exceeds file")
    header = json.loads(data[cursor:header_end].decode("utf-8"))
    payload = data[header_end:]
    if len(payload) != header.get("payload_nbytes"):
        raise ValueError("tile payload size mismatch")
    arrays: dict[str, np.ndarray] = {}
    for name in ("rgba", "owner_id", "owner_layer"):
        descriptor = header["arrays"][name]
        start = int(descriptor["offset"])
        end = start + int(descriptor["nbytes"])
        chunk = payload[start:end]
        if len(chunk) != descriptor["nbytes"] or sha256_bytes(chunk) != descriptor["sha256"]:
            raise ValueError(f"tile array {name} checksum mismatch")
        dtype = np.dtype(descriptor["dtype"])
        arrays[name] = np.frombuffer(chunk, dtype=dtype).reshape(
            descriptor["shape"]
        ).copy()
    if header["tile_version"] != TILE_VERSION:
        raise ValueError("unsupported tile version")
    return TileData(
        header=header,
        rgba=arrays["rgba"],
        owner_id=arrays["owner_id"],
        owner_layer=arrays["owner_layer"],
    )


def read_tile(path: str | Path) -> TileData:
    data = Path(path).read_bytes()
    return parse_tile(data)
