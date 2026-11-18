"""Small dependency-free deterministic file writers."""

from __future__ import annotations

import os
import struct
import tempfile
import zlib
from pathlib import Path
from typing import Any

import numpy as np

from .scene import canonical_bytes


def _atomic_bytes(path: str | Path, data: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    handle = tempfile.NamedTemporaryFile(
        mode="wb",
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
        delete=False,
    )
    temporary = handle.name
    try:
        with handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def write_json_atomic(path: str | Path, value: Any) -> None:
    _atomic_bytes(path, canonical_bytes(value))


def write_npy_atomic(path: str | Path, array: np.ndarray) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    handle = tempfile.NamedTemporaryFile(
        mode="wb",
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
        delete=False,
    )
    temporary = handle.name
    try:
        with handle:
            np.save(handle, np.ascontiguousarray(array), allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _png_chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def png_rgba_bytes(rgba: np.ndarray) -> bytes:
    array = np.asarray(rgba, dtype=np.uint8)
    if array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("RGBA PNG input must be HxWx4")
    height, width = array.shape[:2]
    rows = b"".join(b"\x00" + array[y].tobytes(order="C") for y in range(height))
    header = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", header)
        + _png_chunk(b"IDAT", zlib.compress(rows, level=9))
        + _png_chunk(b"IEND", b"")
    )


def png_gray16_bytes(values: np.ndarray) -> bytes:
    array = np.asarray(values, dtype=np.uint16)
    if array.ndim != 2:
        raise ValueError("gray16 PNG input must be HxW")
    height, width = array.shape
    big_endian = array.astype(">u2", copy=False)
    rows = b"".join(b"\x00" + big_endian[y].tobytes(order="C") for y in range(height))
    header = struct.pack(">IIBBBBB", width, height, 16, 0, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + _png_chunk(b"IHDR", header)
        + _png_chunk(b"IDAT", zlib.compress(rows, level=9))
        + _png_chunk(b"IEND", b"")
    )


def write_png_atomic(path: str | Path, data: bytes) -> None:
    _atomic_bytes(path, data)
