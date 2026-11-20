"""Small deterministic, durable filesystem helpers."""

from __future__ import annotations

import hashlib
import json
import os
import re
import tempfile
from pathlib import Path
from typing import Any, BinaryIO

from .errors import E_INVALID_ARGUMENT, GatewayError

_SESSION_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def validate_session_id(session_id: str) -> str:
    if not isinstance(session_id, str) or not _SESSION_RE.fullmatch(session_id):
        raise GatewayError(
            E_INVALID_ARGUMENT,
            "session id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}",
        )
    return session_id


def validate_sha256(value: str | None, name: str = "sha256") -> str | None:
    if value is None:
        return None
    lowered = value.lower()
    if not _SHA256_RE.fullmatch(lowered):
        raise GatewayError(E_INVALID_ARGUMENT, f"{name} must be 64 hexadecimal characters")
    return lowered


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_stream(stream: BinaryIO) -> tuple[str, int]:
    digest = hashlib.sha256()
    length = 0
    while True:
        block = stream.read(1024 * 1024)
        if not block:
            break
        digest.update(block)
        length += len(block)
    return digest.hexdigest(), length


def sha256_file(path: Path) -> tuple[str, int]:
    with path.open("rb") as stream:
        return sha256_stream(stream)


def file_size(path: Path) -> int:
    try:
        return path.stat().st_size
    except FileNotFoundError:
        return 0


def fsync_dir(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write_bytes(path: Path, data: bytes, *, mode: int = 0o644) -> None:
    """Write data to a sibling temporary file, fsync, then atomically replace."""

    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=f".{path.name}.tmp-", dir=path.parent)
    tmp = Path(tmp_name)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
        fsync_dir(path.parent)
    except BaseException:
        try:
            tmp.unlink()
        except FileNotFoundError:
            pass
        raise


def canonical_json_bytes(value: Any) -> bytes:
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode("utf-8")


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value))


def load_json(path: Path) -> Any:
    with path.open("rb") as stream:
        return json.load(stream)


def safe_manifest_name(object_key: str) -> str:
    return hashlib.sha256(object_key.encode("utf-8")).hexdigest() + ".json"


def ranges_from_numbers(numbers: list[int], total_shards: int) -> list[list[int]]:
    missing = [number for number in range(1, total_shards + 1) if number not in set(numbers)]
    if not missing:
        return []
    ranges: list[list[int]] = []
    start = previous = missing[0]
    for number in missing[1:]:
        if number == previous + 1:
            previous = number
            continue
        ranges.append([start, previous])
        start = previous = number
    ranges.append([start, previous])
    return ranges


def unique_sorted(values: list[str]) -> list[str]:
    return sorted(set(values))
