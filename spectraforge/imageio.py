"""Checksummed, dependency-free raster container.

The container is intentionally simple and deterministic:

    magic (8 bytes) | header_length (u32 LE) | JSON header | raw payload

Images are one-channel rasters. Waveband and exposure semantics live in the
scene metadata, not in the file, so the exact same decoder is used everywhere.
"""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .util import atomic_write_bytes, sha256_bytes

MAGIC = b"SPCTIMG1"


@dataclass(frozen=True)
class Raster:
    data: np.ndarray
    header: dict[str, Any]
    payload_hash: str


def _dtype_name(dtype: np.dtype) -> str:
    return np.dtype(dtype).newbyteorder("<").str


def write_raster(
    path: Path,
    data: np.ndarray,
    *,
    bit_depth: int,
    role: str,
    index: int,
) -> str:
    array = np.asarray(data)
    if array.ndim != 2:
        raise ValueError("Raster payload must be two-dimensional")
    if array.dtype not in (np.dtype("uint8"), np.dtype("uint16")):
        raise ValueError("Raster payload must be uint8 or uint16")
    if bit_depth < 1 or bit_depth > int(array.dtype.itemsize * 8):
        raise ValueError("Invalid bit depth for raster dtype")
    max_code = (1 << bit_depth) - 1
    if int(array.max(initial=0)) > max_code or int(array.min(initial=0)) < 0:
        raise ValueError("Raster values exceed the declared bit depth")
    little = np.ascontiguousarray(array.astype(array.dtype.newbyteorder("<"), copy=False))
    payload = little.tobytes(order="C")
    payload_hash = sha256_bytes(payload)
    header = {
        "format": "spectraforge-raster",
        "version": 1,
        "width": int(array.shape[1]),
        "height": int(array.shape[0]),
        "channels": 1,
        "dtype": _dtype_name(little.dtype),
        "bit_depth": int(bit_depth),
        "role": role,
        "index": int(index),
        "payload_sha256": payload_hash,
    }
    header_bytes = json.dumps(
        header, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
    blob = MAGIC + struct.pack("<I", len(header_bytes)) + header_bytes + payload
    atomic_write_bytes(path, blob)
    return payload_hash


def read_raster(path: Path) -> Raster:
    try:
        blob = path.read_bytes()
    except FileNotFoundError as exc:
        raise WorkbenchError("E_DECODE", "Raster file is missing", {"path": str(path)}) from exc
    except OSError as exc:
        raise WorkbenchError("E_DECODE", "Raster file is unreadable", {"path": str(path), "reason": str(exc)}) from exc
    try:
        if len(blob) < len(MAGIC) + 4 or blob[: len(MAGIC)] != MAGIC:
            raise ValueError("Bad magic")
        header_length = struct.unpack("<I", blob[len(MAGIC) : len(MAGIC) + 4])[0]
        header_start = len(MAGIC) + 4
        if header_length <= 0 or header_start + header_length > len(blob):
            raise ValueError("Bad header length")
        header = json.loads(blob[header_start : header_start + header_length].decode("utf-8"))
        payload = blob[header_start + header_length :]
        if header.get("format") != "spectraforge-raster" or int(header.get("version", -1)) != 1:
            raise ValueError("Unsupported raster format")
        width = int(header["width"])
        height = int(header["height"])
        channels = int(header.get("channels", 1))
        dtype = np.dtype(header["dtype"])
        bit_depth = int(header["bit_depth"])
        if width <= 0 or height <= 0 or channels != 1:
            raise ValueError("Invalid raster dimensions")
        if dtype not in (np.dtype("<u1"), np.dtype("<u2")):
            raise ValueError("Unsupported raster dtype")
        if bit_depth < 1 or bit_depth > int(dtype.itemsize * 8):
            raise ValueError("Invalid raster bit depth")
        expected = width * height * channels * dtype.itemsize
        if len(payload) != expected:
            raise ValueError(f"Payload length {len(payload)} != {expected}")
        payload_hash = sha256_bytes(payload)
        if header.get("payload_sha256") != payload_hash:
            raise ValueError("Payload checksum mismatch")
        array = np.frombuffer(payload, dtype=dtype).reshape(height, width)
        array = array.astype(array.dtype.newbyteorder("="), copy=False)
        if int(array.max(initial=0)) > (1 << bit_depth) - 1:
            raise ValueError("Pixel value exceeds declared bit depth")
    except (KeyError, TypeError, ValueError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise WorkbenchError("E_DECODE", "Raster decode failed", {"path": str(path), "reason": str(exc)}) from exc
    return Raster(data=array, header=header, payload_hash=payload_hash)
