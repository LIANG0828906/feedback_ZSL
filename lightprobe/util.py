from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any, Iterable

import numpy as np


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json(value))


def sha256_array(array: np.ndarray) -> str:
    contiguous = np.ascontiguousarray(array, dtype="<f8")
    return hashlib.sha256(contiguous.tobytes()).hexdigest()


def finite_float(value: Any, path: str) -> float:
    from .errors import scene_invalid

    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise scene_invalid(path, "expected a finite number")
    result = float(value)
    if not math.isfinite(result):
        raise scene_invalid(path, "expected a finite number")
    return result


def vec3(value: Any, path: str, *, nonnegative: bool = False) -> tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise scene_invalid(path, "expected an array of three numbers")
    result = tuple(finite_float(item, f"{path}[{index}]") for index, item in enumerate(value))
    if nonnegative and any(item < 0.0 for item in result):
        raise scene_invalid(path, "components must be non-negative")
    return result  # type: ignore[return-value]


def clamp(value: float, low: float, high: float) -> float:
    return max(low, min(high, value))


def fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write_bytes(path: Path, data: bytes, *, mode: int = 0o644) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}-{id(data)}")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temporary, mode)
    os.replace(temporary, path)
    fsync_directory(path.parent)


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json(value) + b"\n")


def iter_chunks(items: list[Any], size: int) -> Iterable[list[Any]]:
    for start in range(0, len(items), size):
        yield items[start : start + size]

