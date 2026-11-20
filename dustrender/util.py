"""Stable serialization and atomic file helpers."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any

import numpy as np


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def canonical_json(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def array_sha256(array: np.ndarray) -> str:
    return sha256_bytes(np.ascontiguousarray(array, dtype="<f4").tobytes(order="C"))


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value) + b"\n")


def atomic_write_npy(path: str | Path, array: np.ndarray) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".npy", dir=target.parent
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            np.save(
                handle,
                np.ascontiguousarray(array, dtype="<f4"),
                allow_pickle=False,
            )
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise


def encode_array(array: np.ndarray) -> str:
    raw = np.ascontiguousarray(array, dtype="<f4").tobytes(order="C")
    return base64.b64encode(raw).decode("ascii")


def decode_array(text: str, shape: tuple[int, ...], field_path: str) -> np.ndarray:
    try:
        raw = base64.b64decode(text.encode("ascii"), validate=True)
    except Exception as exc:
        from .errors import E_RECOVERY_INVALID, DustRenderError

        raise DustRenderError(
            E_RECOVERY_INVALID, "invalid base64 array payload", field_path
        ) from exc
    expected = int(np.prod(shape, dtype=np.int64)) * 4
    if len(raw) != expected:
        from .errors import E_RECOVERY_INVALID, DustRenderError

        raise DustRenderError(
            E_RECOVERY_INVALID,
            f"array byte length {len(raw)} does not match {expected}",
            field_path,
        )
    return np.frombuffer(raw, dtype="<f4").reshape(shape).copy()


def write_ppm(path: str | Path, image: np.ndarray) -> None:
    rgb = np.clip(np.asarray(image, dtype=np.float64), 0.0, 1.0)
    encoded = np.rint(rgb * 255.0).astype(np.uint8)
    height, width, _ = encoded.shape
    header = f"P6\n{width} {height}\n255\n".encode("ascii")
    atomic_write_bytes(path, header + encoded.tobytes(order="C"))
