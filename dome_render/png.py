"""Minimal deterministic PNG writer, with no timestamps or ancillary chunks."""

from __future__ import annotations

import binascii
import struct
import zlib
from pathlib import Path

import numpy as np


def _chunk(kind: bytes, payload: bytes) -> bytes:
    return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", binascii.crc32(kind + payload) & 0xFFFFFFFF)


def write_rgba16(path: Path, image: np.ndarray) -> None:
    array = np.asarray(image, dtype=np.uint16)
    if array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("RGBA16 image must have shape (h,w,4)")
    height, width, _ = array.shape
    raw = b"".join(b"\x00" + array[y].astype(">u2", copy=False).tobytes() for y in range(height))
    header = struct.pack(">IIBBBBB", width, height, 16, 6, 0, 0, 0)
    data = b"\x89PNG\r\n\x1a\n" + _chunk(b"IHDR", header) + _chunk(b"IDAT", zlib.compress(raw, 9)) + _chunk(b"IEND", b"")
    path.write_bytes(data)


def write_rgba8(path: Path, image: np.ndarray) -> None:
    array = np.asarray(image, dtype=np.uint8)
    if array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("RGBA8 image must have shape (h,w,4)")
    height, width, _ = array.shape
    raw = b"".join(b"\x00" + array[y].tobytes() for y in range(height))
    header = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    data = b"\x89PNG\r\n\x1a\n" + _chunk(b"IHDR", header) + _chunk(b"IDAT", zlib.compress(raw, 9)) + _chunk(b"IEND", b"")
    path.write_bytes(data)
