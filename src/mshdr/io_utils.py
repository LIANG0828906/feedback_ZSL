"""Small deterministic I/O helpers shared by the workbench."""

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
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path, chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def value_version(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def read_json(path: Path, field: str) -> Any:
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError as exc:
        raise WorkbenchError("E_DECODE", f"JSON file not found: {path}", field=field) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise WorkbenchError("E_DECODE", f"Cannot decode JSON file {path}: {exc}", field=field) from exc


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False) + "\n"
    atomic_write_text(path, data)


def atomic_write_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    tmp_path = Path(tmp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
        _fsync_dir(path.parent)
    except Exception:
        tmp_path.unlink(missing_ok=True)
        raise


def atomic_save_npy(path: Path, array: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    tmp_path = Path(tmp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            np.save(handle, np.ascontiguousarray(array), allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp_path, path)
        _fsync_dir(path.parent)
    except Exception:
        tmp_path.unlink(missing_ok=True)
        raise


def _fsync_dir(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def canonical_array_digest(arrays: Iterable[tuple[str, np.ndarray]], metadata: Any) -> str:
    """Hash arrays in a fixed field order, including dtype/shape/content."""

    digest = hashlib.sha256()
    digest.update(canonical_json_bytes(metadata))
    for name, array in arrays:
        contiguous = np.ascontiguousarray(array)
        if contiguous.dtype.byteorder == ">":
            contiguous = contiguous.astype(contiguous.dtype.newbyteorder("<"), copy=False)
        digest.update(name.encode("utf-8") + b"\0")
        digest.update(contiguous.dtype.str.encode("ascii") + b"\0")
        digest.update(json.dumps(list(contiguous.shape)).encode("ascii") + b"\0")
        digest.update(memoryview(contiguous).cast("B"))
    return digest.hexdigest()


def ensure_directory(path: Path) -> Path:
    try:
        path.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise WorkbenchError("E_IO", f"Cannot create directory {path}: {exc}", field=str(path)) from exc
    return path

