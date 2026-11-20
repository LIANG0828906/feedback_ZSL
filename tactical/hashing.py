"""Canonical serialization and hashing helpers."""

from __future__ import annotations

import hashlib
import json
import struct
from collections.abc import Iterable
from typing import Any

ZERO_HASH = "0" * 64


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=True,
        allow_nan=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_json_bytes(value))


def path_bytes(path: Iterable[tuple[int, int]]) -> bytes:
    return b"".join(struct.pack("<II", int(row), int(col)) for row, col in path)


def path_hash(path: Iterable[tuple[int, int]]) -> str:
    return sha256_bytes(path_bytes(path))
