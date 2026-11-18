"""Small deterministic PNG encoder, avoiding optional image dependencies."""

from __future__ import annotations

import os
import struct
import tempfile
import zlib
from pathlib import Path

import numpy as np


def _chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def png_rgba_bytes(image: np.ndarray) -> bytes:
    array = np.ascontiguousarray(image, dtype=np.uint8)
    if array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("PNG RGBA input must have shape (height, width, 4)")
    height, width = array.shape[:2]
    raw = b"".join(b"\x00" + array[row].tobytes() for row in range(height))
    header = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _chunk(b"IHDR", header)
        + _chunk(b"IDAT", zlib.compress(raw, 9))
        + _chunk(b"IEND", b"")
    )


def png_gray_bytes(image: np.ndarray) -> bytes:
    gray = np.ascontiguousarray(image, dtype=np.uint8)
    if gray.ndim != 2:
        raise ValueError("grayscale input must have shape (height, width)")
    rgba = np.empty((gray.shape[0], gray.shape[1], 4), dtype=np.uint8)
    rgba[:, :, 0] = gray
    rgba[:, :, 1] = gray
    rgba[:, :, 2] = gray
    rgba[:, :, 3] = 255
    return png_rgba_bytes(rgba)


def decode_png_rgba(data: bytes) -> np.ndarray:
    """Decode the 8-bit RGBA PNGs emitted by this module.

    Filter types 0-4 are supported even though the encoder writes filter 0.
    """

    if not data.startswith(b"\x89PNG\r\n\x1a\n"):
        raise ValueError("not a PNG file")
    offset = 8
    width = height = 0
    compressed = bytearray()
    while offset < len(data):
        length = struct.unpack(">I", data[offset : offset + 4])[0]
        kind = data[offset + 4 : offset + 8]
        payload = data[offset + 8 : offset + 8 + length]
        offset += 12 + length
        if kind == b"IHDR":
            width, height, bit_depth, color_type, compression, filtering, interlace = struct.unpack(
                ">IIBBBBB", payload
            )
            if bit_depth != 8 or color_type != 6 or compression != 0 or filtering != 0 or interlace != 0:
                raise ValueError("unsupported PNG layout")
        elif kind == b"IDAT":
            compressed.extend(payload)
        elif kind == b"IEND":
            break
    raw = zlib.decompress(bytes(compressed))
    stride = width * 4
    if len(raw) != (stride + 1) * height:
        raise ValueError("PNG data length mismatch")
    rows: list[np.ndarray] = []
    previous = np.zeros(stride, dtype=np.uint8)
    cursor = 0
    for _ in range(height):
        filter_type = raw[cursor]
        cursor += 1
        row = np.frombuffer(raw[cursor : cursor + stride], dtype=np.uint8).copy()
        cursor += stride
        if filter_type == 0:
            pass
        elif filter_type == 1:
            for index in range(4, stride):
                row[index] = (int(row[index]) + int(row[index - 4])) & 0xFF
        elif filter_type == 2:
            row = (row.astype(np.uint16) + previous.astype(np.uint16)).astype(np.uint8)
        elif filter_type == 3:
            for index in range(stride):
                left = int(row[index - 4]) if index >= 4 else 0
                row[index] = (int(row[index]) + ((left + int(previous[index])) // 2)) & 0xFF
        elif filter_type == 4:
            for index in range(stride):
                left = int(row[index - 4]) if index >= 4 else 0
                up = int(previous[index])
                upper_left = int(previous[index - 4]) if index >= 4 else 0
                p = left + up - upper_left
                pa = abs(p - left)
                pb = abs(p - up)
                pc = abs(p - upper_left)
                predictor = left if pa <= pb and pa <= pc else (up if pb <= pc else upper_left)
                row[index] = (int(row[index]) + predictor) & 0xFF
        else:
            raise ValueError(f"unsupported PNG filter {filter_type}")
        previous = row
        rows.append(row)
    return np.stack(rows, axis=0).reshape(height, width, 4)


def write_bytes_atomic(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
        try:
            directory_fd = os.open(target.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            pass
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def write_png_rgba(path: str | Path, image: np.ndarray) -> None:
    write_bytes_atomic(path, png_rgba_bytes(image))


def write_png_gray(path: str | Path, image: np.ndarray) -> None:
    write_bytes_atomic(path, png_gray_bytes(image))

