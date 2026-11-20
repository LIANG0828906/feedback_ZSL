"""Deterministic, append-only JSONL trajectory with a hash chain."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any, Mapping

from .models import canonical_json


TRACE_VERSION = 1


class TraceWriter:
    """A logical-time trace.  No wall clock is used, so seeded runs match."""

    def __init__(
        self,
        path: str | Path | None,
        *,
        run_metadata: Mapping[str, Any] | None = None,
    ):
        self.path = Path(path) if path is not None else None
        self.events: list[dict[str, Any]] = []
        self._sequence = 0
        self._previous = "0" * 64
        self._handle = None
        if self.path is not None:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self._handle = open(self.path, "w", encoding="utf-8", newline="\n")
        self.emit(
            "trace_meta",
            {
                "trace_version": TRACE_VERSION,
                "run": dict(run_metadata or {}),
            },
        )

    def _emit(self, event: dict[str, Any]) -> dict[str, Any]:
        payload = {
            "seq": self._sequence,
            "type": event.pop("type"),
            "prev_hash": self._previous,
            "details": event.pop("details"),
        }
        payload.update(event)
        event_hash = hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()
        payload["event_hash"] = event_hash
        self._previous = event_hash
        self._sequence += 1
        self.events.append(payload)
        if self._handle is not None:
            self._handle.write(canonical_json(payload) + "\n")
            self._handle.flush()
        return payload

    def emit(
        self,
        event_type: str,
        details: Mapping[str, Any] | None = None,
        **fields: Any,
    ) -> dict[str, Any]:
        event = {
            "type": event_type,
            "details": dict(details or {}),
        }
        event.update(fields)
        return self._emit(event)

    def close(self, summary: Mapping[str, Any] | None = None) -> None:
        if self._handle is None:
            return
        self.emit("trace_end", dict(summary or {}))
        self._handle.flush()
        os.fsync(self._handle.fileno())
        self._handle.close()
        self._handle = None

    def __enter__(self) -> "TraceWriter":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        summary = {"ok": exc_type is None, "event_count": self._sequence}
        if exc is not None:
            summary["error"] = str(exc)
        self.close(summary)

    def to_jsonl(self) -> str:
        return "".join(canonical_json(event) + "\n" for event in self.events)
