"""Canonical serialization and filesystem helpers."""

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np


def json_ready(value: Any, *, digits: int = 10) -> Any:
    """Convert numpy/scalar values into deterministic JSON-compatible values."""

    if isinstance(value, np.generic):
        value = value.item()
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError(f"non-finite float cannot be serialized: {value!r}")
        rounded = round(value, digits)
        return 0.0 if rounded == 0 else rounded
    if isinstance(value, np.ndarray):
        return [json_ready(item, digits=digits) for item in value.tolist()]
    if isinstance(value, dict):
        return {str(key): json_ready(item, digits=digits) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_ready(item, digits=digits) for item in value]
    if isinstance(value, Path):
        return str(value)
    return value


def canonical_json(value: Any, *, newline: bool = False) -> str:
    text = json.dumps(
        json_ready(value),
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )
    return text + ("\n" if newline else "")


def sha256_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def sha256_file(path: str | os.PathLike[str], chunk_size: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def sha256_array(values: Any) -> str:
    array = np.ascontiguousarray(np.asarray(values, dtype="<f8"))
    return hashlib.sha256(array.tobytes(order="C")).hexdigest()


def atomic_write_bytes(path: str | os.PathLike[str], payload: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(target.name + ".tmp")
    with open(temporary, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, target)


def atomic_write_json(path: str | os.PathLike[str], value: Any) -> None:
    atomic_write_bytes(path, canonical_json(value, newline=True).encode("utf-8"))


def read_json(path: str | os.PathLike[str]) -> Any:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def ensure_within(workspace: Path, relative_or_absolute: str) -> Path:
    """Resolve a manifest path while preventing accidental escape from workspace."""

    candidate = Path(relative_or_absolute)
    resolved = (candidate if candidate.is_absolute() else workspace / candidate).resolve()
    workspace_resolved = workspace.resolve()
    try:
        resolved.relative_to(workspace_resolved)
    except ValueError as exc:
        raise ValueError(f"path escapes workspace: {relative_or_absolute}") from exc
    return resolved


def safe_slug(text: str) -> str:
    safe = "".join(char if char.isalnum() or char in "-_." else "_" for char in text)
    return safe.strip("._") or "item"
