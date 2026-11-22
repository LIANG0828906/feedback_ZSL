from __future__ import annotations

import hashlib
import json
import os
import struct
import tempfile
from pathlib import Path
from typing import Any, Iterator

import numpy as np

from live_transcode.samples import generate_fixtures

ROOT = Path(__file__).resolve().parents[1]


class Workspace:
    def __init__(self, name: str = "lt-test-"):
        self._temp = tempfile.TemporaryDirectory(prefix=name)
        self.root = Path(self._temp.name)

    def __enter__(self) -> "Workspace":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self._temp.cleanup()

    def fixtures(self, *, seed: int = 19093, duration: float = 1.2) -> list[dict[str, Any]]:
        return generate_fixtures(self.root / "samples", seed=seed, duration=duration)

    @property
    def store(self) -> Path:
        return self.root / "store"


def find_chunks(raw: bytes) -> dict[str, tuple[int, int]]:
    offsets: dict[str, tuple[int, int]] = {}
    offset = 12
    while offset + 8 <= len(raw):
        chunk_id = raw[offset : offset + 4]
        size = struct.unpack_from("<I", raw, offset + 4)[0]
        offsets[chunk_id.decode("latin1")] = (offset + 8, size)
        offset += 8 + size + (size & 1)
    return offsets


def json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def file_sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()

