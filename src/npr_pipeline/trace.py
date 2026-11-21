from __future__ import annotations

import contextlib
import os
from pathlib import Path
from typing import Any, Iterable

from .canonical import canonical_json_bytes
from .pipeline import BatchTrace, RenderResult
from .strokes import LAYER_NAMES


def result_trace_records(
    result: RenderResult,
    *,
    operation: str,
) -> list[dict[str, Any]]:
    version = result.version
    records: list[dict[str, Any]] = [
        {
            "type": "operation",
            "operation": operation,
            "version": version,
            "committed": result.committed,
            "content_hash": result.version_hash,
            "affected_batches": result.affected_batches,
            "reused_batches": result.reused_batches,
            "rewritten_batches": result.rewritten_batches,
            "resumed_batches": result.resumed_batches,
            "removed_batches": result.removed_batches,
        }
    ]
    for layer in sorted(LAYER_NAMES):
        layer_traces = sorted(
            (item for item in result.batch_traces if item.layer == layer),
            key=lambda item: item.batch,
        )
        layer_stats = result.layer_stats.get(str(layer), {})
        for item in layer_traces:
            zone_stats = layer_stats.get("zones", {}).get(item.zone_id, {})
            records.append(
                {
                    "type": "batch",
                    "version": version,
                    "layer": layer,
                    "layer_name": LAYER_NAMES[layer],
                    "batch": item.batch,
                    "chunk": item.chunk,
                    "zone_id": item.zone_id,
                    "affected": item.affected,
                    "reused": item.reused,
                    "resumed": item.resumed,
                    "removed": item.removed,
                    "affected_batches": 1 if item.affected else 0,
                    "reused_batches": 1 if item.reused else 0,
                    "chunk_count": 0 if item.removed else 1,
                    "stroke_count": item.stroke_count,
                    "excluded_strokes": int(zone_stats.get("excluded_strokes", 0)),
                    "content_hash": item.content_hash,
                    "reasons": list(item.reasons),
                }
            )
        records.append(
            {
                "type": "layer",
                "version": version,
                "layer": layer,
                "layer_name": LAYER_NAMES[layer],
                "batch": "*",
                "chunk": "*",
                "affected": any(item.affected for item in layer_traces),
                "reused": any(item.reused for item in layer_traces),
                "affected_batches": sum(1 for item in layer_traces if item.affected),
                "reused_batches": sum(1 for item in layer_traces if item.reused),
                "rewritten_batches": sum(
                    1 for item in layer_traces if not item.reused and not item.removed
                ),
                "chunk_count": int(layer_stats.get("chunk_count", 0)),
                "excluded_strokes": int(layer_stats.get("excluded_strokes", 0)),
                "content_hash": result.version_hash,
            }
        )
    records.append(
        {
            "type": "version",
            "version": version,
            "layer": -1,
            "layer_name": "all",
            "batch": "*",
            "chunk": "*",
            "affected_batches": result.affected_batches,
            "reused_batches": result.reused_batches,
            "rewritten_batches": result.rewritten_batches,
            "resumed_batches": result.resumed_batches,
            "removed_batches": result.removed_batches,
            "chunk_count": sum(
                int(result.layer_stats.get(str(layer), {}).get("chunk_count", 0))
                for layer in LAYER_NAMES
            ),
            "excluded_strokes": sum(
                int(result.layer_stats.get(str(layer), {}).get("excluded_strokes", 0))
                for layer in LAYER_NAMES
            ),
            "content_hash": result.version_hash,
        }
    )
    return records


class TraceWriter:
    def __init__(self, path: str | Path | None, *, reset: bool = False):
        self.path = None if path is None else Path(path)
        if self.path is not None:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            mode = "wb" if reset else "ab"
            self._handle = open(self.path, mode)
        else:
            self._handle = None

    def write(self, records: Iterable[dict[str, Any]]) -> None:
        if self._handle is None:
            return
        for record in records:
            self._handle.write(canonical_json_bytes(record) + b"\n")
        self._handle.flush()
        os.fsync(self._handle.fileno())

    def close(self) -> None:
        if self._handle is not None:
            with contextlib.suppress(OSError):
                self._handle.close()
            self._handle = None

    def __enter__(self) -> "TraceWriter":
        return self

    def __exit__(self, *_args: Any) -> None:
        self.close()


def write_error_trace(
    path: str | Path,
    *,
    operation: str,
    code: str,
    location: str,
    detail: str,
) -> None:
    with TraceWriter(path) as writer:
        writer.write(
            [
                {
                    "type": "error",
                    "operation": operation,
                    "code": code,
                    "location": location,
                    "detail": detail,
                }
            ]
        )


__all__ = [
    "BatchTrace",
    "TraceWriter",
    "result_trace_records",
    "write_error_trace",
]
