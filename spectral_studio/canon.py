"""Canonical serialization and content hashing.

All hashes and trace logs in the project derive from these helpers so that
the same seed and operation sequence produce byte-identical artifacts.
Floats are rounded to a declared precision (12 significant digits) before
serialization; no timestamps, file sizes or absolute paths are ever mixed
into hashed or logged content.
"""

from __future__ import annotations

import hashlib
import json

PRECISION_DIGITS = 12


def canon(value):
    """Recursively canonicalize a value for deterministic serialization."""
    if isinstance(value, bool) or value is None or isinstance(value, (int, str)):
        return value
    if isinstance(value, float):
        if value != value or value in (float("inf"), float("-inf")):
            return value  # non-finite values are rejected by pipeline validation
        rounded = float(f"{value:.{PRECISION_DIGITS}g}")
        return rounded + 0.0  # normalize -0.0 to 0.0
    if isinstance(value, (list, tuple)):
        return [canon(v) for v in value]
    if isinstance(value, dict):
        return {str(k): canon(value[k]) for k in sorted(value)}
    raise TypeError(f"cannot canonicalize {type(value)!r}")


def canonical_json(value) -> str:
    return json.dumps(canon(value), sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def content_hash(value) -> str:
    """SHA-256 over the canonical JSON form of *value*."""
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()
