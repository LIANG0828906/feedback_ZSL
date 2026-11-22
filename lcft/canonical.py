"""Canonical JSON and hashing helpers.

Every persisted and hashed structure goes through this module.  No wall-clock
time, process id, locale, random state or object identity enters a hash.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any, Iterable


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def canonical_json(value: Any) -> str:
    return canonical_bytes(value).decode("utf-8")


def sha256_value(value: Any) -> str:
    return hashlib.sha256(canonical_bytes(value)).hexdigest()


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def payload_hash(value: dict[str, Any]) -> str:
    return sha256_value({key: item for key, item in value.items() if key != "content_hash"})


def without_keys(value: dict[str, Any], keys: Iterable[str]) -> dict[str, Any]:
    excluded = set(keys)
    return {key: item for key, item in value.items() if key not in excluded}


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    """Write inside the target directory, fsync, then atomically replace."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{target.name}.", suffix=".tmp", dir=str(target.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
        try:
            directory_fd = os.open(target.parent, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(directory_fd)
        except OSError:
            pass
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def atomic_write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(value) + b"\n")


def read_json(path: str | Path) -> Any:
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)

