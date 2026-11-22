"""Canonical hashing helpers used by summaries, caches and tests."""

from __future__ import annotations

import hashlib
import json
from typing import Any

import numpy as np


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def hash_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def hash_array(value: np.ndarray) -> str:
    array = np.ascontiguousarray(value)
    header = f"{array.dtype.str}:{array.shape}:".encode("ascii")
    return sha256_bytes(header + array.tobytes(order="C"))


def hash_arrays(named_arrays: list[tuple[str, np.ndarray]]) -> str:
    digest = hashlib.sha256()
    for name, value in named_arrays:
        array = np.ascontiguousarray(value)
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(f"{array.dtype.str}:{array.shape}:".encode("ascii"))
        digest.update(array.tobytes(order="C"))
        digest.update(b"\0")
    return digest.hexdigest()
