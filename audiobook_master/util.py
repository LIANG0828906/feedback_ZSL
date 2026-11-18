"""Small deterministic helpers shared by all stages."""

from __future__ import annotations

import hashlib
import json
import math
import os
import tempfile
from pathlib import Path
from typing import Any


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def sha256_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()


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


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=path.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "wb", closefd=True) as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def atomic_write_json(path: Path, value: Any) -> None:
    data = json.dumps(
        value,
        ensure_ascii=False,
        indent=2,
        sort_keys=False,
        allow_nan=False,
    ).encode("utf-8")
    atomic_write_bytes(path, data + b"\n")


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def stable_seed(*parts: object) -> int:
    digest = hashlib.sha256(
        "\x1f".join(str(part) for part in parts).encode("utf-8")
    ).digest()
    return int.from_bytes(digest[:8], "little", signed=False)


def db_from_amplitude(value: float, floor_db: float = -120.0) -> float:
    if not math.isfinite(value) or value <= 0.0:
        return floor_db
    return max(floor_db, 20.0 * math.log10(value))


def linear_from_db(value_db: float) -> float:
    return 10.0 ** (value_db / 20.0)


def display_path(path: Path, base: Path | None = None) -> str:
    """Return a stable, non-absolute path for reports."""

    candidate = path
    if base is not None:
        try:
            candidate = Path(os.path.relpath(path, base))
        except ValueError:
            candidate = Path(path.name)
    if candidate.is_absolute():
        candidate = Path(candidate.name)
    return candidate.as_posix()


def finite_float(value: float, *, digits: int = 9) -> float:
    value = float(value)
    if not math.isfinite(value):
        raise ValueError("report value is not finite")
    return round(value, digits)
