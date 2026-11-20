"""Deterministic JSONL trace recording."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .util import atomic_write_bytes, canonical_json_bytes


class TraceRecorder:
    def __init__(self) -> None:
        self.events: list[dict[str, Any]] = []

    def emit(self, event: str, **fields: Any) -> dict[str, Any]:
        item = dict(fields)
        item["event"] = event
        item["seq"] = len(self.events)
        self.events.append(item)
        return item

    def to_bytes(self) -> bytes:
        return b"".join(canonical_json_bytes(event) + b"\n" for event in self.events)

    def write(self, path: str | Path) -> None:
        atomic_write_bytes(path, self.to_bytes())
