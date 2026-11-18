from __future__ import annotations

import base64
import hashlib
import json
import math
import os
import tempfile
import zlib
from pathlib import Path
from typing import Any

import numpy as np


def sample_round(value: float, sample_rate: int) -> int:
    """Round a non-negative timestamp to the nearest sample deterministically."""
    if not math.isfinite(value):
        raise ValueError("timestamp must be finite")
    scaled = value * sample_rate
    if scaled >= 0:
        return int(math.floor(scaled + 0.5))
    return -int(math.floor(-scaled + 0.5))


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
        except OSError:
            return
        try:
            try:
                os.fsync(directory_fd)
            except OSError:
                pass
        finally:
            os.close(directory_fd)
    finally:
        if temp_path.exists():
            temp_path.unlink()


def atomic_write_json(path: Path, value: Any) -> None:
    data = json.dumps(
        value,
        sort_keys=True,
        indent=2,
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, data)


def encode_float64_array(array: np.ndarray) -> dict[str, Any]:
    little_endian = np.asarray(array, dtype="<f8", order="C")
    compressed = zlib.compress(little_endian.tobytes(order="C"), level=6)
    return {
        "dtype": "<f8",
        "shape": list(little_endian.shape),
        "encoding": "zlib+base64",
        "data": base64.b64encode(compressed).decode("ascii"),
    }


def decode_float64_array(payload: dict[str, Any]) -> np.ndarray:
    if payload.get("dtype") != "<f8" or payload.get("encoding") != "zlib+base64":
        raise ValueError("unsupported snapshot array encoding")
    raw = zlib.decompress(base64.b64decode(payload["data"], validate=True))
    array = np.frombuffer(raw, dtype="<f8").copy()
    shape = tuple(int(part) for part in payload["shape"])
    return array.reshape(shape)


def db_from_linear(value: float) -> float:
    return 20.0 * math.log10(max(float(value), 1e-12))


def db_from_power(value: float) -> float:
    return 10.0 * math.log10(max(float(value), 1e-24))


def rounded(value: float, digits: int = 9) -> float:
    if not math.isfinite(float(value)):
        return -120.0 if float(value) < 0 else 120.0
    return round(float(value), digits)
