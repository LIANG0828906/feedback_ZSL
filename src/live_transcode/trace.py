from __future__ import annotations

import json
import os
import threading
from pathlib import Path
from typing import Any, Callable

from .models import canonical_json_bytes

TRACE_SCHEMA = "live-transcode-trace/v1"


class TraceWriter:
    def __init__(
        self,
        path: str | os.PathLike[str],
        *,
        callback: Callable[[dict[str, Any]], None] | None = None,
        reset: bool = False,
    ):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.callback = callback
        self.sequence = 0
        self._lock = threading.Lock()
        if reset or not self.path.exists():
            self.path.write_bytes(b"")
        else:
            with self.path.open("r", encoding="utf-8") as handle:
                for line in handle:
                    if line.strip():
                        self.sequence += 1

    def emit(self, action: str, **fields: Any) -> dict[str, Any]:
        with self._lock:
            record = {
                "schema": TRACE_SCHEMA,
                "seq": self.sequence,
                "action": action,
                **fields,
            }
            self.sequence += 1
            line = canonical_json_bytes(record) + b"\n"
            with self.path.open("ab") as handle:
                handle.write(line)
                handle.flush()
                os.fsync(handle.fileno())
        if self.callback:
            self.callback(record)
        return record


def read_trace(path: str | os.PathLike[str]) -> list[dict[str, Any]]:
    target = Path(path)
    if not target.exists():
        return []
    records: list[dict[str, Any]] = []
    with target.open("r", encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                records.append(json.loads(line))
    return records


def trace_digest(path: str | os.PathLike[str]) -> str:
    import hashlib

    return hashlib.sha256(Path(path).read_bytes()).hexdigest()

