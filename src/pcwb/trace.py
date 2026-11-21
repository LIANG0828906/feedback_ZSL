"""Deterministic JSONL trace and separate runtime measurement output."""

from __future__ import annotations

import threading
from collections.abc import Mapping
from pathlib import Path
from typing import Any

from .util import canonical_json_bytes, normalize_json_value


class JsonlWriter:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self.path.write_bytes(b"")

    def append(self, event: Mapping[str, Any]) -> None:
        payload = canonical_json_bytes(normalize_json_value(dict(event)))
        with self._lock, self.path.open("ab") as stream:
            stream.write(payload)
            stream.flush()


class DeterministicTrace(JsonlWriter):
    """A trace with no wall-clock timestamps or nondeterministic measurements."""


class RuntimeMetricsLog(JsonlWriter):
    """Measured durations, intentionally separate from the reproducible trace."""
