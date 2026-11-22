from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .models import RunResult


class ReplayLogWriter:
    def __init__(self, path: str | Path, header: dict[str, Any]) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._handle = open(self.path, "w", encoding="utf-8")
        self._write({"type": "header", **header})
        self._entries = 0
        self._closed = False

    def _write(self, payload: dict[str, Any]) -> None:
        self._handle.write(
            json.dumps(
                payload,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            )
            + "\n"
        )
        self._handle.flush()
        os.fsync(self._handle.fileno())

    def append(self, event: dict[str, Any]) -> None:
        if self._closed:
            raise RuntimeError("replay log is closed")
        self._entries += 1
        self._write({"type": "event", "entry": self._entries, "event": event})

    def append_result(self, result: RunResult) -> None:
        if self._closed:
            raise RuntimeError("replay log is closed")
        self._write(
            {
                "type": "result",
                "complete": True,
                "frames": [item.to_dict() for item in result.frames],
                "inputs": [item.to_dict() for item in result.inputs],
                "final_phase": result.final_phase,
                "desktop_state": result.desktop_state,
                "stats": result.stats.to_dict(),
            }
        )
        self._write(
            {
                "type": "footer",
                "complete": True,
                "events": self._entries,
                "final_phase": result.final_phase,
            }
        )
        self._closed = True
        self._handle.close()

    def abort(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._handle.close()

    def __enter__(self) -> "ReplayLogWriter":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        if not self._closed:
            self.abort()


def write_run_log(
    result: RunResult,
    path: str | Path,
    metadata: dict[str, Any] | None = None,
) -> Path:
    header = {
        "session_id": result.session_id,
        "content_hash": result.content_hash,
        "capabilities_hash": result.capabilities_hash,
        "mode": result.mode,
        **(metadata or {}),
    }
    with ReplayLogWriter(path, header) as writer:
        for event in result.events:
            writer.append(event)
        writer.append_result(result)
    return Path(path)


def inspect_replay_log(path: str | Path) -> dict[str, Any]:
    records: list[dict[str, Any]] = []
    with Path(path).open("r", encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                records.append(json.loads(line))
    complete = bool(records and records[-1].get("type") == "footer" and records[-1].get("complete") is True)
    return {
        "complete": complete,
        "records": len(records),
        "last_type": records[-1].get("type") if records else None,
        "status": "complete" if complete else "partial",
    }
