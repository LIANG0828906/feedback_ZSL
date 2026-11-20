"""Canonical JSON, tensor hashing, and atomic file replacement helpers."""

from __future__ import annotations

import hashlib
import json
import math
import os
import struct
from pathlib import Path
from typing import Any, Iterable, Mapping

import numpy as np
import torch


GENESIS_HASH = "0" * 64


def _normalize_json(value: Any) -> Any:
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        converted = value.tolist()
        if not isinstance(converted, list):
            return _normalize_json(converted)
        return [_normalize_json(item) for item in converted]
    if isinstance(value, torch.Tensor):
        converted = value.detach().cpu().tolist()
        if not isinstance(converted, list):
            return _normalize_json(converted)
        return [_normalize_json(item) for item in converted]
    if isinstance(value, Mapping):
        return {str(key): _normalize_json(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_normalize_json(item) for item in value]
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("canonical JSON does not accept non-finite floats")
        return 0.0 if value == 0.0 else value
    return value


def canonical_json_bytes(value: Any) -> bytes:
    """Serialize in insertion order with no insignificant whitespace.

    Callers deliberately construct ordered dictionaries, therefore key order
    is part of the on-disk contract rather than being alphabetically sorted.
    """

    normalized = _normalize_json(value)
    return json.dumps(
        normalized,
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def tensor_bytes(tensor: torch.Tensor) -> bytes:
    array = tensor.detach().cpu().contiguous().numpy()
    if array.dtype == np.dtype("bool"):
        array = array.astype(np.uint8, copy=False)
    return np.ascontiguousarray(array).tobytes(order="C")


def tensor_content_hash(tensor: torch.Tensor) -> str:
    array = tensor.detach().cpu().contiguous().numpy()
    digest = hashlib.sha256()
    digest.update(str(tuple(array.shape)).encode("ascii"))
    digest.update(b"|")
    digest.update(array.dtype.str.encode("ascii"))
    digest.update(b"|")
    digest.update(tensor_bytes(tensor))
    return digest.hexdigest()


def tensor_map_hash(values: Mapping[str, torch.Tensor]) -> str:
    """Hash an ordered tensor map, including field names and dtypes."""

    digest = hashlib.sha256()
    for name, tensor in values.items():
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(tensor_content_hash(tensor).encode("ascii"))
        digest.update(b"\n")
    return digest.hexdigest()


def combine_hashes(values: Iterable[str]) -> str:
    digest = hashlib.sha256()
    for value in values:
        digest.update(str(value).encode("utf-8"))
        digest.update(b"\n")
    return digest.hexdigest()


def parameter_content_hash(tensor: torch.Tensor) -> str:
    return tensor_content_hash(tensor)


def float_bytes(value: float) -> bytes:
    """Unused by checkpoints, retained as a deterministic primitive for tests."""

    return struct.pack(">d", float(value))


def fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    """Write through a pre-agreed sibling temporary file and atomically rename."""

    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with open(tmp, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    fsync_directory(path.parent)


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value))


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

