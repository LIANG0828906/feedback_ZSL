"""Small deterministic PNG writer (no Pillow dependency)."""

from __future__ import annotations

import struct
import zlib
from pathlib import Path

import numpy as np


def _chunk(tag: bytes, payload: bytes) -> bytes:
    checksum = zlib.crc32(tag)
    checksum = zlib.crc32(payload, checksum) & 0xFFFFFFFF
    return (
        struct.pack(">I", len(payload))
        + tag
        + payload
        + struct.pack(">I", checksum)
    )


def encode_rgb_png(rgb: np.ndarray, compression_level: int = 6) -> bytes:
    if rgb.dtype != np.uint8 or rgb.ndim != 3 or rgb.shape[2] != 3:
        raise ValueError("rgb must be a uint8 array with shape (H, W, 3)")
    height, width, _ = rgb.shape
    signature = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    scanlines = b"".join(
        b"\x00" + np.ascontiguousarray(rgb[row]).tobytes() for row in range(height)
    )
    idat = zlib.compress(scanlines, level=compression_level)
    return signature + _chunk(b"IHDR", ihdr) + _chunk(b"IDAT", idat) + _chunk(b"IEND", b"")


def write_rgb_png(path: str | Path, rgb: np.ndarray) -> None:
    Path(path).write_bytes(encode_rgb_png(rgb))


def owner_visualization(
    owner: np.ndarray, lead_mask: np.ndarray | None = None
) -> np.ndarray:
    """Colourise stable piece serials for quick human inspection."""
    height, width = owner.shape
    output = np.zeros((height, width, 3), dtype=np.uint8)
    serials = np.unique(owner)
    for serial_raw in serials:
        serial = int(serial_raw)
        if serial == 0:
            continue
        hashed = (
            (serial * 0x9E3779B1) ^ 0xA5A5A5A5
        ) & 0xFFFFFFFF
        red = 48 + ((hashed >> 0) & 0xBF)
        green = 48 + ((hashed >> 8) & 0xBF)
        blue = 48 + ((hashed >> 16) & 0xBF)
        output[owner == serial] = (red, green, blue)
    if lead_mask is not None:
        output[lead_mask] = (12, 10, 8)
    return output
