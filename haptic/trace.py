from __future__ import annotations

import threading
from pathlib import Path
from typing import Any

from .engine import CompileResult
from .util import canonical_json_text


class TraceLogger:
    """Append-only deterministic JSON Lines build trace."""

    def __init__(self, path: str | Path, *, reset: bool = False) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._sequence = 0
        if reset:
            self.path.write_text("", encoding="utf-8")
        elif self.path.exists():
            with self.path.open("r", encoding="utf-8") as handle:
                self._sequence = sum(1 for _ in handle)

    @property
    def sequence(self) -> int:
        with self._lock:
            return self._sequence

    def append(self, payload: dict[str, Any]) -> None:
        with self._lock:
            self._sequence += 1
            event = dict(payload)
            event["seq"] = self._sequence
            line = canonical_json_text(event) + "\n"
            with self.path.open("a", encoding="utf-8") as handle:
                handle.write(line)
                handle.flush()

    def record_build(
        self,
        result: CompileResult,
        *,
        event: str = "publish",
    ) -> None:
        version = result.version
        changed = set(result.changed_indices)
        dependencies = [
            {
                "segment_index": signal.index,
                "sample_interval": [
                    signal.start_sample,
                    signal.end_sample,
                ],
                "sample_hash": signal.sample_hash,
            }
            for signal in result.state.segment_signals
            if signal.index in changed
        ]
        self.append(
            {
                "schema": "haptic-trace-v1",
                "event": event,
                "version": version.version,
                "parent_hash": version.parent_hash,
                "content_hash": version.content_hash,
                "sample_rate": version.sample_rate,
                "sample_count": version.sample_count,
                "full_rebuild": result.full_rebuild,
                "modified_segments": list(result.changed_indices),
                "dependencies": dependencies,
                "affected_intervals": [
                    [start, end] for start, end in result.dirty_ranges
                ],
                "reused_intervals": [
                    [start, end] for start, end in result.reused_ranges
                ],
                "affected_interval_count": result.recomputed_interval_count,
                "reused_interval_count": result.reused_interval_count,
                "recomputed_samples": result.recomputed_samples,
                "reused_samples": result.reused_samples,
                "recomputed_chunks": result.recomputed_chunks,
                "reused_chunks": result.reused_chunks,
                "limited_samples": version.limited_samples,
                "out_of_range_samples": version.out_of_range_samples,
                "segment_hashes": [
                    record.to_dict() for record in version.segment_records
                ],
            }
        )
