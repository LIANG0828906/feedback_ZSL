from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_text(text: str) -> str:
    return sha256_bytes(text.encode("utf-8"))


def sha256_json(value: Any) -> str:
    return sha256_text(canonical_json(value))


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def array_bytes(array: np.ndarray) -> bytes:
    canonical = np.ascontiguousarray(array, dtype="<f8")
    return canonical.tobytes(order="C")


def sha256_array(array: np.ndarray) -> str:
    normalized = np.ascontiguousarray(array, dtype="<f8")
    digest = hashlib.sha256()
    digest.update(f"{normalized.shape}:{normalized.dtype.str}:".encode("ascii"))
    digest.update(normalized.tobytes(order="C"))
    return digest.hexdigest()


def stable_seed(*parts: Any) -> int:
    return int(sha256_text(canonical_json(list(parts)))[:16], 16)


def rounded(value: Any, digits: int = 12) -> Any:
    """Return a JSON-safe deterministic representation for trace output."""

    if isinstance(value, dict):
        return {str(key): rounded(item, digits) for key, item in sorted(value.items())}
    if isinstance(value, (list, tuple)):
        return [rounded(item, digits) for item in value]
    if isinstance(value, (np.floating, float)):
        number = float(value)
        if not math.isfinite(number):
            return None
        if number == 0.0:
            return 0.0
        return float(f"{number:.{digits}g}")
    if isinstance(value, (np.integer, int)):
        return int(value)
    if isinstance(value, np.bool_):
        return bool(value)
    return value

