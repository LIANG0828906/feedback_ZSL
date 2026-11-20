"""Canonical serialization, deterministic hashing, and atomic local files."""

from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import ErrorCode, ProbeError


def _canonical_value(value: Any) -> Any:
    if isinstance(value, np.ndarray):
        return {
            "dtype": str(value.dtype),
            "shape": list(value.shape),
            "data": value.tolist(),
        }
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ValueError("canonical JSON cannot contain non-finite floats")
        return float(value)
    if isinstance(value, dict):
        return {str(key): _canonical_value(value[key]) for key in sorted(value)}
    if isinstance(value, (list, tuple)):
        return [_canonical_value(item) for item in value]
    if isinstance(value, (set, frozenset)):
        return sorted(_canonical_value(item) for item in value)
    return value


def canonical_json(value: Any) -> str:
    return json.dumps(
        _canonical_value(value),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    )


def sha256_hex(data: bytes | str) -> str:
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


def hash_object(value: Any) -> str:
    return sha256_hex(canonical_json(value))


def hash_arrays(named_arrays: Iterable[tuple[str, np.ndarray]], extra: Any = None) -> str:
    digest = hashlib.sha256()
    if extra is not None:
        digest.update(canonical_json(extra).encode("utf-8"))
        digest.update(b"\0")
    for name, array in named_arrays:
        contiguous = np.ascontiguousarray(array)
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(str(contiguous.dtype).encode("ascii"))
        digest.update(b"\0")
        digest.update(json.dumps(list(contiguous.shape)).encode("ascii"))
        digest.update(b"\0")
        digest.update(contiguous.tobytes(order="C"))
        digest.update(b"\0")
    return digest.hexdigest()


def freeze_array(array: np.ndarray) -> np.ndarray:
    frozen = np.array(array, copy=True, order="C")
    frozen.setflags(write=False)
    return frozen


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    file_descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{target.name}.tmp-", dir=target.parent
    )
    try:
        with os.fdopen(file_descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, target)
        try:
            directory_fd = os.open(target.parent, os.O_RDONLY)
        except OSError:
            directory_fd = -1
        if directory_fd >= 0:
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_text(path: str | Path, text: str) -> None:
    atomic_write_bytes(path, text.encode("utf-8"))


def write_json_atomic(path: str | Path, payload: Any) -> None:
    atomic_write_text(path, canonical_json(payload) + "\n")


def read_json(path: str | Path) -> Any:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise ProbeError(ErrorCode.IO, f"cannot read JSON {path}: {exc}") from exc


def cleanup_temporary_files(directory: str | Path) -> int:
    removed = 0
    for path in Path(directory).glob(".*.tmp-*"):
        try:
            path.unlink()
            removed += 1
        except FileNotFoundError:
            pass
    return removed


class TraceLog:
    """Append-only JSONL trace; safe to share across bake and query threads."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def event(self, event: str, **fields: Any) -> dict[str, Any]:
        payload = {
            "at": datetime.now(timezone.utc).isoformat(),
            "event": event,
            **fields,
        }
        line = json.dumps(payload, sort_keys=True, ensure_ascii=True) + "\n"
        with self._lock:
            with open(self.path, "a", encoding="utf-8") as handle:
                handle.write(line)
                handle.flush()
        return payload

