from __future__ import annotations

import hashlib
import json
import os
import tempfile
from collections.abc import Iterable
from pathlib import Path
from typing import Any


def _normalize_floats(value: Any) -> Any:
    if isinstance(value, float):
        if value == 0.0:
            return 0.0
        return value
    if isinstance(value, dict):
        return {str(k): _normalize_floats(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_normalize_floats(v) for v in value]
    return value


def canonical_bytes(value: Any) -> bytes:
    normalized = _normalize_floats(value)
    return (
        json.dumps(
            normalized,
            ensure_ascii=False,
            allow_nan=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        + b"\n"
    )


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_file(path: Path, chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while chunk := handle.read(chunk_size):
            digest.update(chunk)
    return digest.hexdigest()


def fsync_dir(path: Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write_bytes(path: Path, data: bytes) -> None:
    """Write bytes via a sibling temp file, fsync, and atomic replace."""

    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        fsync_dir(path.parent)
    except BaseException:  # noqa: BLE001
        try:
            temp_path.unlink(missing_ok=True)
        finally:
            raise


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_bytes(value))


def append_json_line(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    line = canonical_bytes(value)
    with path.open("ab") as handle:
        handle.write(line)
        handle.flush()
        os.fsync(handle.fileno())


def intervals_from_indices(indices: Iterable[int]) -> list[list[int]]:
    ordered = sorted({int(index) for index in indices})
    if not ordered:
        return []
    result: list[list[int]] = []
    start = previous = ordered[0]
    for index in ordered[1:]:
        if index == previous + 1:
            previous = index
            continue
        result.append([start, previous])
        start = previous = index
    result.append([start, previous])
    return result


def indices_from_intervals(intervals: Iterable[Iterable[int]]) -> list[int]:
    result: set[int] = set()
    for start, end in intervals:
        result.update(range(int(start), int(end) + 1))
    return sorted(result)


def relative_to_workspace(path: Path, workspace: Path) -> str:
    resolved = path.resolve()
    try:
        return resolved.relative_to(workspace.resolve()).as_posix()
    except ValueError:
        return resolved.as_posix()

