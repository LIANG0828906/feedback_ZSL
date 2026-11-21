from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

from .hashing import compact_json_line, write_bytes_atomic


class TraceWriter:
    """Append-only deterministic JSONL trace.

    Rows are compact canonical JSON with sorted keys.  No clock values or
    random identifiers are inserted here.
    """

    def __init__(self, path: Path | str | None, *, truncate: bool = False) -> None:
        self.path = Path(path) if path is not None else None
        self._lock = threading.Lock()
        self._closed = False
        if self.path is not None:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            if truncate:
                write_bytes_atomic(self.path, b"")

    def write(self, event: str, **fields: Any) -> dict[str, Any]:
        row = {"event": event, **fields}
        if self.path is not None and not self._closed:
            with self._lock:
                with self.path.open("ab") as handle:
                    handle.write(compact_json_line(row))
                    handle.flush()
        return row

    def close(self) -> None:
        self._closed = True

