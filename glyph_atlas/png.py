"""Small deterministic RGBA PNG writer/reader (no external image library)."""

from __future__ import annotations

import struct
import zlib
from pathlib import Path

import numpy as np

from .errors import E_IO, fail
from .snapshot import atomic_write_bytes

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def encode_rgba_png(image: np.ndarray) -> bytes:
    array = np.asarray(image)
    if array.dtype != np.uint8 or array.ndim != 3 or array.shape[2] != 4:
        fail(E_IO, "PNG image must be an HxWx4 uint8 array")
    height, width, _ = array.shape
    rows = bytearray()
    for row in array:
        rows.append(0)  # deterministic filter type: None
        rows.extend(np.ascontiguousarray(row).tobytes())
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (
        PNG_SIGNATURE
        + _chunk(b"IHDR", ihdr)
        + _chunk(b"IDAT", zlib.compress(bytes(rows), level=9))
        + _chunk(b"IEND", b"")
    )


def encode_pgm(coverage: np.ndarray) -> bytes:
    array = np.asarray(coverage)
    if array.dtype != np.uint8 or array.ndim != 2:
        fail(E_IO, "Coverage image must be a 2D uint8 array")
    height, width = array.shape
    return f"P5\n{width} {height}\n255\n".encode("ascii") + np.ascontiguousarray(array).tobytes()


def write_rgba_png(
    path: str | Path,
    image: np.ndarray,
    temp_dir: str | Path | None = None,
) -> bytes:
    data = encode_rgba_png(image)
    atomic_write_bytes(path, data, temp_dir=temp_dir)
    return data


def read_rgba_png(path: str | Path) -> np.ndarray:
    data = Path(path).read_bytes()
    if not data.startswith(PNG_SIGNATURE):
        fail(E_IO, "Not a PNG file", path=str(path))
    offset = len(PNG_SIGNATURE)
    width = height = None
    compressed = bytearray()
    while offset < len(data):
        if offset + 8 > len(data):
            fail(E_IO, "Truncated PNG chunk", path=str(path))
        length = struct.unpack(">I", data[offset : offset + 4])[0]
        chunk_type = data[offset + 4 : offset + 8]
        payload_start = offset + 8
        payload_end = payload_start + length
        if payload_end + 4 > len(data):
            fail(E_IO, "Truncated PNG payload", path=str(path))
        payload = data[payload_start:payload_end]
        expected_crc = struct.unpack(">I", data[payload_end : payload_end + 4])[0]
        actual_crc = zlib.crc32(chunk_type + payload) & 0xFFFFFFFF
        if expected_crc != actual_crc:
            fail(E_IO, "PNG chunk CRC mismatch", path=str(path))
        if chunk_type == b"IHDR":
            width, height, depth, color_type, _, _, interlace = struct.unpack(">IIBBBBB", payload)
            if depth != 8 or color_type != 6 or interlace != 0:
                fail(E_IO, "Only non-interlaced RGBA8 PNG is supported", path=str(path))
        elif chunk_type == b"IDAT":
            compressed.extend(payload)
        elif chunk_type == b"IEND":
            break
        offset = payload_end + 4
    if width is None or height is None:
        fail(E_IO, "PNG IHDR is missing", path=str(path))
    raw = zlib.decompress(bytes(compressed))
    stride = width * 4
    if len(raw) != height * (stride + 1):
        fail(E_IO, "PNG pixel payload length mismatch", path=str(path))
    image = np.empty((height, width, 4), dtype=np.uint8)
    for row in range(height):
        start = row * (stride + 1)
        if raw[start] != 0:
            fail(E_IO, "Only PNG filter type 0 is supported", path=str(path))
        image[row] = np.frombuffer(raw[start + 1 : start + 1 + stride], dtype=np.uint8).reshape(width, 4)
    return image


def _chunk(chunk_type: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + chunk_type
        + payload
        + struct.pack(">I", zlib.crc32(chunk_type + payload) & 0xFFFFFFFF)
    )

