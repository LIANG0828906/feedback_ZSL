from __future__ import annotations

import base64
import hashlib
import json
import os
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


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = canonical_json_bytes(value) + b"\n"
    path.write_bytes(data)


def atomic_write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = canonical_json_bytes(value) + b"\n"
    tmp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    try:
        with tmp.open("wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
        _fsync_directory(path.parent)
    finally:
        if tmp.exists():
            tmp.unlink()


def _fsync_directory(path: Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


def encode_array(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array)
    payload = zlib.compress(contiguous.tobytes(order="C"), level=6)
    return base64.b64encode(payload).decode("ascii")


def decode_array(payload: str, shape: tuple[int, ...], dtype: str) -> np.ndarray:
    try:
        compressed = base64.b64decode(payload.encode("ascii"), validate=True)
        raw = zlib.decompress(compressed)
    except Exception as exc:
        raise ValueError("invalid compressed array payload") from exc
    dtype_obj = np.dtype(dtype)
    expected = int(np.prod(shape)) * dtype_obj.itemsize
    if len(raw) != expected:
        raise ValueError(f"array byte length {len(raw)} does not match {expected}")
    return np.frombuffer(raw, dtype=dtype_obj).reshape(shape).copy()


def stable_float(value: float, digits: int = 8) -> float:
    rounded = round(float(value), digits)
    if rounded == 0.0:
        return 0.0
    return rounded


def relative_name(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()
