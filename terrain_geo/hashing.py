"""Deterministic hashing / canonical-JSON helpers for cache keys & snapshots."""

from __future__ import annotations

import hashlib
import json
from typing import Any

import numpy as np


def canonical_json(obj: Any) -> str:
    """Serialize nested data structures deterministically (sorted keys)."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), default=_default)


def _default(obj: Any) -> Any:
    if isinstance(obj, (np.floating, np.integer)):
        return obj.item()
    if isinstance(obj, np.ndarray):
        return obj.tolist()
    raise TypeError(f"not JSON serializable: {type(obj)!r}")


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def content_hash(obj: Any) -> str:
    """Stable content hash of any canonically-serializable object."""
    return sha256_text(canonical_json(obj))


def image_hash(image: np.ndarray) -> str:
    """Raw-bytes content hash of an image array (dtype/shape included)."""
    h = hashlib.sha256()
    h.update(str(image.shape).encode("utf-8"))
    h.update(b"|")
    h.update(str(image.dtype).encode("utf-8"))
    h.update(b"|")
    h.update(np.ascontiguousarray(image).tobytes())
    return h.hexdigest()


def file_hash(path: str, chunk: int = 1 << 20) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for block in iter(lambda: fh.read(chunk), b""):
            h.update(block)
    return h.hexdigest()
