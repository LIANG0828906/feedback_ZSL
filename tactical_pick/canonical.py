"""Canonical encodings used for hashes and byte-stable artifacts."""

from __future__ import annotations

import hashlib
import json
import math
from typing import Any


def _finite_float(value: float) -> float:
    number = float(value)
    if not math.isfinite(number):
        raise ValueError("non-finite value in canonical JSON")
    # Avoid negative zero leaking into byte-stable artifacts.
    if number == 0.0:
        return 0.0
    return number


def normalize_json(value: Any) -> Any:
    """Recursively normalise tuples, bytes and negative zero for JSON."""

    if isinstance(value, float):
        return _finite_float(value)
    if isinstance(value, (tuple, list)):
        return [normalize_json(item) for item in value]
    if isinstance(value, dict):
        return {str(key): normalize_json(value[key]) for key in sorted(value)}
    if isinstance(value, (str, int, bool)) or value is None:
        return value
    raise TypeError(f"unsupported canonical JSON type: {type(value)!r}")


def canonical_json_bytes(value: Any) -> bytes:
    normalized = normalize_json(value)
    text = json.dumps(
        normalized,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    )
    return text.encode("utf-8")


def canonical_json_text(value: Any) -> str:
    return canonical_json_bytes(value).decode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))
