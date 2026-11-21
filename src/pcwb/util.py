"""Small deterministic serialization and filesystem helpers."""

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np


def normalize_json_value(value: Any) -> Any:
    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("non-finite numeric value")
        return value
    if isinstance(value, np.ndarray):
        return [normalize_json_value(item) for item in value.tolist()]
    if isinstance(value, (list, tuple)):
        return [normalize_json_value(item) for item in value]
    if isinstance(value, dict):
        return {str(key): normalize_json_value(value[key]) for key in sorted(value)}
    return value


def canonical_json_bytes(value: Any) -> bytes:
    value = normalize_json_value(value)
    return (
        json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )


def pretty_json_bytes(value: Any) -> bytes:
    value = normalize_json_value(value)
    return (
        json.dumps(
            value,
            sort_keys=True,
            indent=2,
            ensure_ascii=True,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )


def sha256_hex(data: bytes | bytearray | memoryview) -> str:
    return hashlib.sha256(data).hexdigest()


def fsync_directory(path: Path) -> None:
    """Best-effort directory fsync for durable rename semantics on POSIX."""

    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
