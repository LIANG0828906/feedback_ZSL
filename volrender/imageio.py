"""Dependency-free deterministic RGB PNG encoder."""

from __future__ import annotations

import struct
import zlib

import numpy as np


def _chunk(kind: bytes, data: bytes) -> bytes:
    payload = kind + data
    return (
        struct.pack(">I", len(data))
        + payload
        + struct.pack(">I", zlib.crc32(payload) & 0xFFFFFFFF)
    )


def tonemap_rgb8(
    color: np.ndarray, scale: int = 1, exposure: float = 4.0
) -> np.ndarray:
    mapped = 1.0 - np.exp(-np.maximum(color, 0.0) * exposure)
    mapped = np.power(np.clip(mapped, 0.0, 1.0), 1.0 / 2.2)
    rgb = np.rint(mapped * 255.0).astype(np.uint8)
    if scale > 1:
        rgb = np.repeat(np.repeat(rgb, scale, axis=0), scale, axis=1)
    return rgb


def png_bytes(
    color: np.ndarray, scale: int = 1, exposure: float = 4.0
) -> bytes:
    rgb = tonemap_rgb8(color, scale, exposure)
    height, width, _ = rgb.shape
    raw = b"".join(
        b"\x00" + np.ascontiguousarray(rgb[row]).tobytes()
        for row in range(height)
    )
    header = struct.pack(
        ">IIBBBBB",
        width,
        height,
        8,
        2,
        0,
        0,
        0,
    )
    return (
        b"\x89PNG\r\n\x1a\n"
        + _chunk(b"IHDR", header)
        + _chunk(b"IDAT", zlib.compress(raw, 9))
        + _chunk(b"IEND", b"")
    )
