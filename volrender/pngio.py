"""Small deterministic PNG writer used when no GUI/OpenGL surface exists."""

from __future__ import annotations

import struct
import zlib

import numpy as np


def _chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def encode_png(image: np.ndarray, *, compression: int = 9) -> bytes:
    """Encode an RGB/RGBA/grayscale uint8 image without timestamps."""

    array = np.asarray(image)
    if array.dtype != np.uint8:
        raise ValueError("PNG image must be uint8")
    if array.ndim == 2:
        height, width = array.shape
        channels = 1
        color_type = 0
    elif array.ndim == 3 and array.shape[2] in (3, 4):
        height, width, channels = array.shape
        color_type = 2 if channels == 3 else 6
    else:
        raise ValueError("PNG image must be HxW, HxWx3 or HxWx4")
    rows = array.reshape(height, width * channels)
    raw = b"".join(b"\x00" + row.tobytes() for row in rows)
    header = struct.pack(">IIBBBBB", width, height, 8, color_type, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _chunk(b"IHDR", header)
        + _chunk(b"IDAT", zlib.compress(raw, compression))
        + _chunk(b"IEND", b"")
    )


def write_png(path: str, image: np.ndarray, *, compression: int = 9) -> None:
    with open(path, "wb") as handle:
        handle.write(encode_png(image, compression=compression))


def colorize_error(error: np.ndarray, *, max_error: float = 255.0) -> np.ndarray:
    """Map a non-negative scalar error field to a deterministic RGB heatmap."""

    values = np.clip(np.asarray(error, dtype=np.float64) / max(1.0, max_error), 0.0, 1.0)
    red = np.clip(values * 2.0, 0.0, 1.0)
    green = np.clip(values * 2.0 - 0.5, 0.0, 1.0)
    blue = np.clip(values * 2.0 - 1.0, 0.0, 1.0)
    return np.rint(np.stack([red, green, blue], axis=-1) * 255.0).astype(np.uint8)


__all__ = ["colorize_error", "encode_png", "write_png"]
