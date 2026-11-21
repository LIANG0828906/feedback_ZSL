"""Canonical, byte-stable trajectory logging."""

from __future__ import annotations

import fcntl
import os
from pathlib import Path
from typing import Any, Dict, Optional

from .util import canonical_json_bytes, hash_obj, sha256_file


class TrajectoryLog:
    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)

    def _next_sequence(self) -> int:
        if not self.path.exists():
            return 0
        count = 0
        with self.path.open("rb") as handle:
            for line in handle:
                if line.strip():
                    count += 1
        return count

    def append(self, event: Dict[str, Any]) -> Dict[str, Any]:
        lock_path = self.path.with_suffix(self.path.suffix + ".lock")
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        with lock_path.open("a+b") as lock_handle:
            fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX)
            try:
                payload = dict(event)
                payload.pop("entry_hash", None)
                payload["seq"] = self._next_sequence()
                payload["entry_hash"] = hash_obj(payload)
                line = canonical_json_bytes(payload) + b"\n"
                with self.path.open("ab") as handle:
                    handle.write(line)
                    handle.flush()
                    os.fsync(handle.fileno())
            finally:
                fcntl.flock(lock_handle.fileno(), fcntl.LOCK_UN)
        return payload

    def snapshot_hash(self) -> Optional[str]:
        if not self.path.exists():
            return None
        return sha256_file(self.path)

