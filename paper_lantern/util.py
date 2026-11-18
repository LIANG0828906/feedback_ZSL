from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any

import numpy as np


def canonical_json(data: Any) -> bytes:
    """Canonical, platform-independent JSON bytes used for scene digests."""
    return json.dumps(
        data,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_array(array: np.ndarray) -> str:
    array = np.ascontiguousarray(array)
    return sha256_bytes(array.tobytes(order="C"))


def fsync_directory(path: Path) -> None:
    """Best-effort directory fsync after an atomic rename."""
    try:
        fd = os.open(str(path), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp-{os.getpid()}")
    try:
        with open(tmp, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
        fsync_directory(path.parent)
    finally:
        try:
            if tmp.exists():
                tmp.unlink()
        except OSError:
            pass


def stable_float(value: float) -> float:
    """Round report floats to a stable, readable precision."""
    rounded = round(float(value), 10)
    return 0.0 if rounded == 0.0 else rounded


def as_float_list(vector: np.ndarray) -> list[float]:
    return [stable_float(item) for item in np.asarray(vector, dtype=np.float64).tolist()]


def normalize(vector: np.ndarray) -> np.ndarray:
    vector = np.asarray(vector, dtype=np.float64)
    length = float(np.linalg.norm(vector))
    if length <= 1e-15:
        return vector.copy()
    return vector / length

