from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np

MASK64 = (1 << 64) - 1


def splitmix64(value: int) -> int:
    """A small, explicitly seeded 64-bit mixing function."""
    z = (value + 0x9E3779B97F4A7C15) & MASK64
    z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK64
    return (z ^ (z >> 31)) & MASK64


def seeded_uniform(count: int, seed: int, stream: int) -> np.ndarray:
    """Return deterministic float64 uniforms in [0, 1)."""
    base = ((int(seed) & MASK64) ^ ((int(stream) & MASK64) << 32)) & MASK64
    out = np.empty(count, dtype=np.float64)
    for i in range(count):
        out[i] = splitmix64((base + i) & MASK64) / float(1 << 64)
    return out


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_array(array: np.ndarray) -> str:
    return sha256_bytes(np.ascontiguousarray(array).tobytes(order="C"))


def write_json_atomic(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = (
        json.dumps(
            value,
            sort_keys=False,
            ensure_ascii=False,
            allow_nan=False,
            indent=2,
        )
        + "\n"
    ).encode("utf-8")
    tmp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    with open(tmp, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
    try:
        dir_fd = os.open(path.parent, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)


def finite_float(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
    )


def vec3(value: Any) -> np.ndarray | None:
    if (
        isinstance(value, (list, tuple))
        and len(value) == 3
        and all(finite_float(v) for v in value)
    ):
        return np.asarray(value, dtype=np.float64)
    return None


def is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def stable_round(value: float, digits: int = 7) -> float:
    rounded = round(float(value), digits)
    return 0.0 if rounded == 0.0 else rounded


def lerp(a: float, b: float, t: float) -> float:
    return a + (b - a) * t

