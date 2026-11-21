"""Canonical serialization helpers. No timestamp is ever part of a hash."""

from __future__ import annotations

import hashlib
import json
import math
import os
from pathlib import Path
from typing import Any


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


def json_bytes(value: Any) -> bytes:
    return (canonical_json(value) + "\n").encode("utf-8")


def write_json(path: str | Path, value: Any) -> None:
    atomic_write_bytes(Path(path), json_bytes(value))


def atomic_write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, path)


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def hash_payload(prev_hash: str, payload: Any) -> str:
    body = canonical_json(payload).encode("utf-8")
    return sha256_hex(prev_hash.encode("ascii") + b"\0" + body)


def finite_float(value: Any) -> float:
    number = float(value)
    if not math.isfinite(number):
        raise ValueError("non-finite number")
    return number


def f32(value: float) -> float:
    """Represent a value exactly as it will be stored in the scene file."""
    import struct

    return struct.unpack("<f", struct.pack("<f", float(value)))[0]


def distance3(a: tuple[float, float, float], b: tuple[float, float, float]) -> float:
    return math.sqrt((a[0] - b[0]) ** 2 + (a[1] - b[1]) ** 2 + (a[2] - b[2]) ** 2)

