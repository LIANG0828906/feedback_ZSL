"""Canonical JSON and stable hashing helpers."""

from __future__ import annotations

import hashlib
import json
from typing import Any


def canonical_bytes(value: Any, *, newline: bool = True) -> bytes:
    data = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    return data + (b"\n" if newline else b"")


def canonical_text(value: Any, *, pretty: bool = False) -> str:
    if pretty:
        return json.dumps(
            value,
            sort_keys=True,
            indent=2,
            ensure_ascii=False,
            allow_nan=False,
        ) + "\n"
    return canonical_bytes(value).decode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_bytes(canonical_bytes(value, newline=False))


def fnv1a64(text: str) -> int:
    value = 0xCBF29CE484222325
    for byte in text.encode("utf-8"):
        value ^= byte
        value = (value * 0x100000001B3) & 0xFFFFFFFFFFFFFFFF
    return value

