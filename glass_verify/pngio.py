from __future__ import annotations

import binascii
import struct
import zlib
from pathlib import Path

import numpy as np


def _chunk(kind: bytes, payload: bytes) -> bytes:
    crc = binascii.crc32(kind + payload) & 0xFFFFFFFF
    return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", crc)


def _png(width: int, height: int, bit_depth: int, color_type: int, rows: bytes) -> bytes:
    signature = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, bit_depth, color_type, 0, 0, 0)
    compressed = zlib.compress(rows, level=9)
    return (
        signature
        + _chunk(b"IHDR", ihdr)
        + _chunk(b"IDAT", compressed)
        + _chunk(b"IEND", b"")
    )


def encode_rgb8(image: np.ndarray) -> bytes:
    """Encode uint8 HxWx3 pixels as a metadata-free deterministic PNG."""
    array = np.asarray(image)
    if array.dtype != np.uint8 or array.ndim != 3 or array.shape[2] != 3:
        raise ValueError("encode_rgb8 expects uint8 HxWx3")
    height, width, _ = array.shape
    rows = b"".join(b"\x00" + array[y].tobytes(order="C") for y in range(height))
    return _png(width, height, 8, 2, rows)


def encode_gray16(image: np.ndarray) -> bytes:
    """Encode uint16 HxW pixels as a metadata-free deterministic PNG."""
    array = np.asarray(image)
    if array.dtype != np.uint16 or array.ndim != 2:
        raise ValueError("encode_gray16 expects uint16 HxW")
    height, width = array.shape
    big_endian = array.astype(">u2", copy=False)
    rows = b"".join(b"\x00" + big_endian[y].tobytes(order="C") for y in range(height))
    return _png(width, height, 16, 0, rows)


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{id(payload)}.tmp")
    try:
        with temporary.open("wb") as handle:
            handle.write(payload)
            handle.flush()
            import os

            os.fsync(handle.fileno())
        temporary.replace(path)
    finally:
        if temporary.exists():
            temporary.unlink()
