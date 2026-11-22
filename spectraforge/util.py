"""Small deterministic utility helpers shared by core and UI."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import WorkbenchError


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


def hash_object(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def hash_arrays(arrays: Iterable[tuple[str, np.ndarray]], extra: Any = None) -> str:
    digest = hashlib.sha256()
    for name, array in arrays:
        contiguous = np.ascontiguousarray(array)
        digest.update(name.encode("utf-8"))
        digest.update(str(contiguous.dtype).encode("ascii"))
        digest.update(repr(tuple(contiguous.shape)).encode("ascii"))
        digest.update(contiguous.tobytes(order="C"))
    if extra is not None:
        digest.update(canonical_json_bytes(extra))
    return "sha256:" + digest.hexdigest()


def read_json(path: Path, *, code: str = "E_META", label: str = "JSON") -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError as exc:
        raise WorkbenchError(code, f"Missing {label} file", {"path": str(path)}) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise WorkbenchError(code, f"Cannot parse {label} file", {"path": str(path), "reason": str(exc)}) from exc
    if not isinstance(value, dict):
        raise WorkbenchError(code, f"{label} root must be an object", {"path": str(path)})
    return value


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        _fsync_directory(path.parent)
    except Exception:
        try:
            temp_path.unlink(missing_ok=True)
        finally:
            raise


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value) + b"\n")


def _fsync_directory(path: Path) -> None:
    if os.name == "nt":
        return
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def relpath_posix(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()


def clamp(value: float, low: float, high: float) -> float:
    return float(min(high, max(low, value)))
