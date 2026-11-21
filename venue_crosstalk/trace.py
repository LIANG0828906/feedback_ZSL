"""Deterministic JSONL trace logging."""

from __future__ import annotations

import hashlib
import threading
from pathlib import Path
from typing import Any

from .utils import canonical_json, json_ready, read_json


class TraceLogger:
    def __init__(self, path: str | Path, *, reset: bool = False) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._seq = 0
        self._chain_hash = "0" * 64
        if reset:
            self.path.write_bytes(b"")
        elif self.path.exists() and self.path.stat().st_size:
            for line in self.path.read_text(encoding="utf-8").splitlines():
                if not line:
                    continue
                try:
                    entry = read_json_line(line)
                except ValueError:
                    break
                self._seq = int(entry.get("seq", self._seq)) + 1
                self._chain_hash = str(entry.get("chain_hash", self._chain_hash))

    @property
    def chain_hash(self) -> str:
        return self._chain_hash

    def emit(
        self,
        *,
        operation: str,
        node: str,
        pair: str | None = None,
        band: str | None = None,
        cache_hit: bool | None = None,
        reason: str | None = None,
        content_hash: str | None = None,
        data: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        with self._lock:
            entry: dict[str, Any] = {
                "seq": self._seq,
                "operation": operation,
                "node": node,
            }
            if pair is not None:
                entry["pair"] = pair
            if band is not None:
                entry["band"] = band
            if cache_hit is not None:
                entry["cache_hit"] = bool(cache_hit)
            if reason is not None:
                entry["reason"] = reason
            if content_hash is not None:
                entry["content_hash"] = content_hash
            if data is not None:
                entry["data"] = json_ready(data)
            digest = hashlib.sha256(
                (self._chain_hash + canonical_json(entry)).encode("utf-8")
            ).hexdigest()
            entry["chain_hash"] = digest
            with open(self.path, "a", encoding="utf-8", newline="\n") as handle:
                handle.write(canonical_json(entry) + "\n")
                handle.flush()
            self._seq += 1
            self._chain_hash = digest
            return entry


def read_json_line(line: str) -> dict[str, Any]:
    import json

    value = json.loads(line)
    if not isinstance(value, dict):
        raise ValueError("trace line is not an object")
    return value
