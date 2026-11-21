from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Dict, Optional

from .util import canonical_json_bytes


class TraceLogger:
    def __init__(self, path: Optional[Path]) -> None:
        self.path = path
        self._handle = None
        self._digest = hashlib.sha256()
        self._sequence = 0
        self._closed = False
        if path is not None:
            path.parent.mkdir(parents=True, exist_ok=True)
            self._handle = path.open("wb")

    def emit(
        self,
        node: str,
        action: str,
        *,
        pair: Optional[str] = None,
        band: Optional[str] = None,
        cached: Optional[bool] = None,
        reason: Optional[str] = None,
        content_hash: Optional[str] = None,
        details: Optional[Dict[str, Any]] = None,
    ) -> None:
        if self._closed:
            raise RuntimeError("trace logger is closed")
        payload: Dict[str, Any] = {
            "seq": self._sequence,
            "node": node,
            "action": action,
        }
        if pair is not None:
            payload["pair"] = pair
        if band is not None:
            payload["band"] = band
        if cached is not None:
            payload["cached"] = bool(cached)
        if reason is not None:
            payload["reason"] = reason
        if content_hash is not None:
            payload["content_hash"] = content_hash
        if details is not None:
            payload["details"] = details
        line = canonical_json_bytes(payload)
        self._digest.update(line)
        if self._handle is not None:
            self._handle.write(line)
            self._handle.flush()
        self._sequence += 1

    @property
    def sequence(self) -> int:
        return self._sequence

    @property
    def digest_hex(self) -> str:
        return self._digest.hexdigest()

    def close(self) -> None:
        if self._closed:
            return
        if self._handle is not None:
            self._handle.flush()
            self._handle.close()
            self._handle = None
        self._closed = True

    def __enter__(self) -> "TraceLogger":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()
