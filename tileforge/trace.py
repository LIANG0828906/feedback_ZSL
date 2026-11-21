"""Deterministic JSONL traces without timestamps or thread interleaving."""

from __future__ import annotations

import json
import os
import threading
from pathlib import Path
from typing import Any

from .binary import atomic_write_bytes


class TraceLog:
    def __init__(self, path: str | Path | None = None) -> None:
        self.path = Path(path) if path is not None else None
        self._lines: list[str] = []
        self._seq = 0
        self._lock = threading.Lock()
        if self.path is not None:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            atomic_write_bytes(self.path, b"")

    def emit(self, event: str, **fields: Any) -> dict[str, Any]:
        with self._lock:
            record = {"seq": self._seq, "event": event, **fields}
            self._seq += 1
            line = json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False)
            self._lines.append(line)
            if self.path is not None:
                with self.path.open("a", encoding="utf-8", newline="\n") as handle:
                    handle.write(line + "\n")
                    handle.flush()
                    os.fsync(handle.fileno())
            return record

    @property
    def records(self) -> list[dict[str, Any]]:
        return [json.loads(line) for line in self._lines]

    def close(self) -> None:
        pass
