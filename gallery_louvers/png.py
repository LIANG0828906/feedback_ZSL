"""Minimal deterministic PNG encoder (RGBA8, no external imaging dependency)."""

from __future__ import annotations

import struct
import zlib
from pathlib import Path

import numpy as np

from .util import atomic_write_bytes


def _chunk(kind: bytes, payload: bytes) -> bytes:
    crc = zlib.crc32(kind)
    crc = zlib.crc32(payload, crc) & 0xFFFFFFFF
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", crc)
    )


def encode_png_rgba(image: np.ndarray) -> bytes:
    array = np.asarray(image)
    if array.dtype != np.uint8 or array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("encode_png_rgba expects HxWx4 uint8")
    height, width = array.shape[:2]
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    rows = b"".join(b"\x00" + array[row].tobytes(order="C") for row in range(height))
    compressed = zlib.compress(rows, level=9)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _chunk(b"IHDR", ihdr)
        + _chunk(b"IDAT", compressed)
        + _chunk(b"IEND", b"")
    )


def write_png_rgba(path: str | Path, image: np.ndarray) -> None:
    atomic_write_bytes(path, encode_png_rgba(image))
