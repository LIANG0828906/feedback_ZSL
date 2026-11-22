"""Deterministic hashing and atomic filesystem helpers."""

from __future__ import annotations

import hashlib
import json
import os
import pathlib
import tempfile
from typing import Any


def canonical_bytes(payload: Any) -> bytes:
    """Canonical JSON encoding: sorted keys, no whitespace, stable separators."""
    return json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def stable_digest(payload: Any, *, prefix: str = "", length: int | None = None) -> str:
    digest = hashlib.sha256(canonical_bytes(payload)).hexdigest()
    if length is not None:
        digest = digest[:length]
    return f"{prefix}{digest}" if prefix else digest


def file_sha256(path: str | os.PathLike, *, chunk: int = 1 << 20) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        while True:
            block = fh.read(chunk)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def atomic_write_bytes(path: str | os.PathLike, data: bytes, *, fsync: bool = True) -> None:
    """Write via temp file + fsync + os.replace, never leave a partial target."""
    target = pathlib.Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=".tmp-", dir=str(target.parent))
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            if fsync:
                fh.flush()
                os.fsync(fh.fileno())
        os.replace(tmp_name, target)
    except BaseException:
        try:
            os.unlink(tmp_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_json(path: str | os.PathLike, payload: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(payload))


def read_json(path: str | os.PathLike) -> Any:
    with open(path, "rb") as fh:
        return json.loads(fh.read().decode("utf-8"))


def ensure_dir(path: str | os.PathLike) -> pathlib.Path:
    p = pathlib.Path(path)
    p.mkdir(parents=True, exist_ok=True)
    return p
