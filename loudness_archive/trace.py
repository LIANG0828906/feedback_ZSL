from __future__ import annotations

import json
import os
import threading
from collections.abc import Iterable
from pathlib import Path
from typing import Any

from .util import canonical_json_bytes, fsync_directory

_EVENT_ORDER = {
    "task_submit": 0,
    "task_start": 1,
    "task_wait_memory": 2,
    "baseline": 2,
    "segment_start": 3,
    "segment_commit": 4,
    "manifest_update": 5,
    "recover_scan": 6,
    "recover_skip": 7,
    "recover_compute": 8,
    "recover_commit": 9,
    "task_cancel": 10,
    "task_complete": 11,
    "task_failed": 11,
    "publish": 12,
}


class TraceLogger:
    """Append-only deterministic JSONL trace.

    Events deliberately exclude wall-clock timestamps and absolute paths. The
    complete log is canonically sorted before each atomic flush, so concurrent
    completion order cannot alter its bytes.
    """

    def __init__(self, path: str | Path):
        self.path = Path(path)
        self._events: list[dict[str, Any]] = []
        self._lock = threading.RLock()
        self._load_existing()

    def _load_existing(self) -> None:
        if not self.path.exists():
            return
        with open(self.path, "r", encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    self._events.append(json.loads(line))

    @staticmethod
    def _sort_key(event: dict[str, Any]) -> tuple[Any, ...]:
        return (
            str(event.get("asset_id", "")),
            int(event.get("submit_seq", -1)),
            int(event.get("segment_index", -1)),
            _EVENT_ORDER.get(str(event.get("event", "")), 99),
            str(event.get("phase", "")),
            str(event.get("hash", "")),
            canonical_json_bytes(event),
        )

    def record(self, event: str, **fields: Any) -> None:
        entry = {"event": event, **fields}
        canonical_json_bytes(entry)
        with self._lock:
            self._events.append(entry)
            self.flush()

    def extend(self, events: Iterable[dict[str, Any]]) -> None:
        with self._lock:
            self._events.extend(events)
            self.flush()

    def flush(self) -> None:
        with self._lock:
            ordered = sorted(self._events, key=self._sort_key)
            payload = b"".join(canonical_json_bytes(item) + b"\n" for item in ordered)
            self.path.parent.mkdir(parents=True, exist_ok=True)
            temp = self.path.with_name(f".{self.path.name}.tmp-{os.getpid()}")
            with open(temp, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp, self.path)
            fsync_directory(self.path.parent)

    def events(self) -> list[dict[str, Any]]:
        with self._lock:
            return sorted((dict(item) for item in self._events), key=self._sort_key)

    def bytes(self) -> bytes:
        return b"".join(
            canonical_json_bytes(item) + b"\n" for item in self.events()
        )

