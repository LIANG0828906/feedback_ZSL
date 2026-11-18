from __future__ import annotations

import hashlib
import json
import math
import os
import struct
import tempfile
import zlib
from pathlib import Path
from typing import Any

import numpy as np


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def stable_float(value: float, digits: int = 6) -> float:
    if not math.isfinite(float(value)):
        raise ValueError("non-finite number cannot be serialized")
    rounded = round(float(value), digits)
    return 0.0 if rounded == 0 else rounded


def stable_list(values: np.ndarray | list[float], digits: int = 6) -> list[float]:
    return [stable_float(float(v), digits) for v in np.asarray(values).reshape(-1)]


def json_text(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False) + "\n"


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        try:
            directory_fd = os.open(destination.parent, os.O_RDONLY)
        except OSError:
            directory_fd = None
        if directory_fd is not None:
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    finally:
        if temporary.exists():
            temporary.unlink(missing_ok=True)


def atomic_write_text(path: str | Path, text: str) -> None:
    atomic_write_bytes(path, text.encode("utf-8"))


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_text(path, json_text(value))


def write_npy(path: str | Path, array: np.ndarray) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            np.save(handle, array, allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        if temporary.exists():
            temporary.unlink(missing_ok=True)


def _png_chunk(kind: bytes, payload: bytes) -> bytes:
    return (
        struct.pack(">I", len(payload))
        + kind
        + payload
        + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
    )


def write_png(path: str | Path, array: np.ndarray) -> None:
    """Write uint8 grayscale or RGBA without external imaging dependencies."""

    pixels = np.asarray(array, dtype=np.uint8)
    if pixels.ndim == 2:
        color_type = 0
        channels = 1
        height, width = pixels.shape
        raw = b"".join(b"\x00" + pixels[row].tobytes() for row in range(height))
    elif pixels.ndim == 3 and pixels.shape[2] == 4:
        color_type = 6
        channels = 4
        height, width, _ = pixels.shape
        raw = b"".join(b"\x00" + pixels[row].tobytes() for row in range(height))
    else:
        raise ValueError("PNG writer accepts HxW uint8 or HxWx4 uint8 arrays")

    header = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, color_type, 0, 0, 0)
    compressed = zlib.compress(raw, level=9)
    png = header + _png_chunk(b"IHDR", ihdr) + _png_chunk(b"IDAT", compressed) + _png_chunk(b"IEND", b"")
    atomic_write_bytes(path, png)


def quantize_energy(value: float) -> int:
    return int(round(float(value) * 1_000_000.0))


def format_percentage(numerator: int, denominator: int) -> float:
    if denominator <= 0:
        return 0.0
    return stable_float(100.0 * numerator / denominator, 6)

