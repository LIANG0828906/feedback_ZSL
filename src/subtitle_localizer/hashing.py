from __future__ import annotations

import hashlib
import json
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Iterable, Mapping


def canonical_bytes(value: Any) -> bytes:
    """Canonical JSON bytes used for every content hash.

    No timestamps, process identifiers, locale-dependent formatting or float
    special values are allowed.  Non-finite floats are rejected by json.dumps.
    """

    return json.dumps(
        value,
        ensure_ascii=False,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def content_hash(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value))


def hash_without(value: Mapping[str, Any], *keys: str) -> str:
    return content_hash({k: v for k, v in value.items() if k not in keys})


def pretty_json_bytes(value: Any) -> bytes:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            indent=2,
        )
        + "\n"
    ).encode("utf-8")


def compact_json_line(value: Any) -> bytes:
    return canonical_bytes(value) + b"\n"


def decimal_string(value: Any) -> str:
    if isinstance(value, bool):
        raise ValueError("Boolean is not a valid decimal")
    try:
        number = Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise ValueError(f"invalid decimal: {value!r}") from exc
    if not number.is_finite():
        raise ValueError(f"non-finite decimal: {value!r}")
    text = format(number, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    if text in {"", "-0"}:
        text = "0"
    return text


def write_bytes_atomic(path: Path, payload: bytes) -> None:
    """Write bytes to a sibling temporary file, fsync, then atomically replace."""

    import os
    import tempfile

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        try:
            dir_fd = os.open(path.parent, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if temp_path.exists():
            try:
                temp_path.unlink()
            except OSError:
                pass


def sorted_unique(values: Iterable[str]) -> list[str]:
    return sorted(set(values))

