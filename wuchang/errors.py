"""Structured business errors and stable JSON helpers."""

from __future__ import annotations

import json
import os
from collections.abc import Iterable
from pathlib import Path
from typing import Any


def business_error(
    code: str,
    channel: str | None,
    message: str,
    **details: Any,
) -> dict[str, Any]:
    item: dict[str, Any] = {
        "code": code,
        "channel": channel,
        "message": message,
    }
    if details:
        item["details"] = details
    return item


def stable_json_bytes(value: Any) -> bytes:
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=False,
            separators=(",", ":"),
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def atomic_write_bytes(path: str | os.PathLike[str], payload: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp = target.with_name(f".{target.name}.tmp")
    with open(temp, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temp, target)
    try:
        directory_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        pass


def write_json(path: str | os.PathLike[str], value: Any) -> None:
    atomic_write_bytes(path, stable_json_bytes(value))


def display_path(path: str | os.PathLike[str], root: str | os.PathLike[str]) -> str:
    """Return a stable path without leaking the workspace absolute path."""

    candidate = Path(path)
    root_path = Path(root).resolve()
    try:
        resolved = candidate.resolve()
        relative = resolved.relative_to(root_path)
        return relative.as_posix()
    except (OSError, ValueError):
        return candidate.name


def ordered_warnings(items: Iterable[dict[str, Any]]) -> list[dict[str, Any]]:
    return list(items)

