"""Minimal deterministic PNG writer for byte-stable RGBA artifacts."""

from __future__ import annotations

import binascii
import struct
import zlib
from pathlib import Path

import numpy as np

_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def _chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", binascii.crc32(kind + payload) & 0xFFFFFFFF)
    )


def encode_rgba_png(rgba: np.ndarray) -> bytes:
    if rgba.dtype != np.uint8 or rgba.ndim != 3 or rgba.shape[2] != 4:
        raise ValueError("RGBA image must be an HxWx4 uint8 array")
    height, width = rgba.shape[:2]
    scanlines = b"".join(b"\x00" + rgba[row].tobytes(order="C") for row in range(height))
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (
        _SIGNATURE
        + _chunk(b"IHDR", ihdr)
        + _chunk(b"IDAT", zlib.compress(scanlines, level=9))
        + _chunk(b"IEND", b"")
    )


def write_rgba_png(path: str | Path, rgba: np.ndarray) -> None:
    Path(path).write_bytes(encode_rgba_png(rgba))

