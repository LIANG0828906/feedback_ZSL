"""Deterministic hash-chained query traces and version-bound replay."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any, Callable, Iterable, Sequence

from .camera import Camera
from .errors import ErrorCode, TacticalError, trace_invalid
from .projection import ProjectionConfig
from .query import (
    BudgetConfig,
    HoverConfig,
    HoverResult,
    QueryEngine,
    SelectionResult,
)
from .scene import Scene

TRACE_SCHEMA = "tactical-trace-v1"
GENESIS_HASH = "0" * 64


def _canonical_bytes(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def selection_event(
    result: SelectionResult,
    camera: Camera,
    *,
    threshold: float | None = None,
    degraded_stages: Sequence[str] | None = None,
) -> dict[str, Any]:
    return {
        "type": "selection",
        "scene_version": result.scene_version,
        "scene_hash": result.scene_hash,
        "camera": camera.to_dict(),
        "query": {
            "rect": [float(v) for v in result.rect],
            "threshold": (
                round(result.threshold, 12)
                if threshold is None
                else round(float(threshold), 12)
            ),
        },
        "strategy": result.strategy.value,
        "bucket_grid": [result.bucket_columns, result.bucket_rows],
        "refinement_depth": result.refinement_depth,
        "stage_candidate_counts": dict(sorted(result.stage_counts.items())),
        "selected": list(result.selected),
        "selected_coverage": [
            {
                "unit_id": decision.unit_id,
                "coverage": round(decision.coverage, result.coverage_decimals),
            }
            for decision in result.decisions
            if decision.selected
        ],
        "coverage_error_upper_bound": result.coverage_error_upper_bound,
        "degraded_stages": list(result.degraded_stages if degraded_stages is None else degraded_stages),
    }


def hover_event(result: HoverResult, camera: Camera) -> dict[str, Any]:
    payload = result.canonical_dict(include_timings=False)
    payload["point"] = [float(result.point[0]), float(result.point[1])]
    payload["camera"] = camera.to_dict()
    payload["type"] = "hover"
    # canonical_dict uses "kind"; one event discriminator is enough.
    payload.pop("kind", None)
    return payload


class TraceRecorder:
    def __init__(self, path: str | os.PathLike[str], *, append: bool = False) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._seq = 0
        self._prev_hash = GENESIS_HASH
        if append and self.path.exists() and self.path.stat().st_size > 0:
            records = load_trace(self.path)
            if records:
                self._seq = int(records[-1]["seq"]) + 1
                self._prev_hash = str(records[-1]["record_hash"])
            self._handle = self.path.open("ab")
        else:
            self._handle = self.path.open("wb")
        self._closed = False

    def _record(self, event: dict[str, Any]) -> dict[str, Any]:
        version = event.get("scene_version")
        content_hash = event.get("scene_hash")
        if not isinstance(version, int) or version < 1:
            raise trace_invalid("trace event is missing a valid scene version", event=event)
        if not isinstance(content_hash, str) or len(content_hash) != 64:
            raise trace_invalid("trace event is missing a valid scene hash", event=event)
        record = {
            "schema": TRACE_SCHEMA,
            "seq": self._seq,
            "prev_hash": self._prev_hash,
            "event": event,
        }
        digest = hashlib.sha256(_canonical_bytes(record)).hexdigest()
        record["record_hash"] = digest
        encoded = _canonical_bytes(record) + b"\n"
        self._handle.write(encoded)
        self._handle.flush()
        os.fsync(self._handle.fileno())
        self._seq += 1
        self._prev_hash = digest
        return record

    def record_selection(
        self,
        result: SelectionResult,
        camera: Camera,
        *,
        threshold: float | None = None,
    ) -> dict[str, Any]:
        return self._record(selection_event(result, camera, threshold=threshold))

    def record_hover(self, result: HoverResult, camera: Camera) -> dict[str, Any]:
        return self._record(hover_event(result, camera))

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            self._handle.close()

    def __enter__(self) -> "TraceRecorder":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


def load_trace(path: str | os.PathLike[str]) -> list[dict[str, Any]]:
    trace_path = Path(path)
    try:
        lines = trace_path.read_bytes().splitlines()
    except OSError as exc:
        raise trace_invalid("cannot read trace file", path=str(trace_path), reason=str(exc)) from exc
    if not lines:
        return []
    records: list[dict[str, Any]] = []
    previous_hash = GENESIS_HASH
    for line_index, line in enumerate(lines):
        if not line.strip():
            raise trace_invalid("trace contains an empty record", line=line_index + 1)
        try:
            record = json.loads(line)
        except json.JSONDecodeError as exc:
            raise trace_invalid("trace contains invalid JSON", line=line_index + 1) from exc
        if not isinstance(record, dict):
            raise trace_invalid("trace record must be an object", line=line_index + 1)
        if record.get("schema") != TRACE_SCHEMA:
            raise trace_invalid("trace schema is invalid", line=line_index + 1)
        sequence = record.get("seq")
        if type(sequence) is not int or sequence != line_index:
            raise trace_invalid(
                "trace sequence is not contiguous",
                line=line_index + 1,
                sequence=sequence,
            )
        if record.get("prev_hash") != previous_hash:
            raise trace_invalid(
                "trace hash chain is broken",
                line=line_index + 1,
                expected_prev_hash=previous_hash,
                actual_prev_hash=record.get("prev_hash"),
            )
        event = record.get("event")
        if not isinstance(event, dict):
            raise trace_invalid("trace event is missing", line=line_index + 1)
        version = event.get("scene_version")
        if type(version) is not int or version < 1:
            raise trace_invalid("trace event version is missing", line=line_index + 1)
        if not isinstance(event.get("scene_hash"), str):
            raise trace_invalid("trace event scene hash is missing", line=line_index + 1)
        actual_hash = record.get("record_hash")
        if not isinstance(actual_hash, str):
            raise trace_invalid("trace record hash is missing", line=line_index + 1)
        unsigned = dict(record)
        unsigned.pop("record_hash", None)
        expected_hash = hashlib.sha256(_canonical_bytes(unsigned)).hexdigest()
        if actual_hash != expected_hash:
            raise trace_invalid(
                "trace record hash does not match its contents",
                line=line_index + 1,
                expected_hash=expected_hash,
                actual_hash=actual_hash,
            )
        records.append(record)
        previous_hash = expected_hash
    return records


class TraceReplayer:
    """Replays traces by forcing the recorded strategy and bucket grid."""

    def __init__(
        self,
        resolver: Callable[[int, str], Scene],
        *,
        projection_config: ProjectionConfig | None = None,
    ) -> None:
        self.resolver = resolver
        self.projection_config = projection_config or ProjectionConfig()

    def replay(self, path: str | os.PathLike[str]) -> list[dict[str, Any]]:
        records = load_trace(path)
        replayed: list[dict[str, Any]] = []
        for index, record in enumerate(records):
            event = record["event"]
            try:
                scene = self.resolver(int(event["scene_version"]), str(event["scene_hash"]))
                camera = Camera.from_dict(event["camera"])
                assert isinstance(camera, Camera)
            except TacticalError:
                raise
            except Exception as exc:
                raise trace_invalid(
                    "trace record cannot be reconstructed",
                    line=index + 1,
                    reason=str(exc),
                ) from exc
            if event.get("type") == "selection":
                reproduced = self._replay_selection(scene, camera, event)
            elif event.get("type") == "hover":
                reproduced = self._replay_hover(scene, camera, event)
            else:
                raise trace_invalid("trace contains an unknown event type", line=index + 1)
            if _canonical_bytes(reproduced) != _canonical_bytes(event):
                raise trace_invalid(
                    "replayed conclusion differs from the recorded conclusion",
                    line=index + 1,
                    sequence=record.get("seq"),
                    recorded_hash=record.get("record_hash"),
                    reproduced=reproduced,
                )
            replayed.append(reproduced)
        return replayed

    def _replay_selection(
        self,
        scene: Scene,
        camera: Camera,
        event: dict[str, Any],
    ) -> dict[str, Any]:
        query = event["query"]
        rect = query["rect"]
        columns = int(event["bucket_grid"][0])
        budget = BudgetConfig(
            budget_ms=1e15,
            memory_budget_bytes=2**62,
            selection_threshold=float(query["threshold"]),
            bucket_grids=(columns,),
            force_strategy=str(event["strategy"]),
            force_bucket_columns=columns,
            actual_degradation=False,
        )
        result = QueryEngine(
            scene,
            camera,
            projection_config=self.projection_config,
            budget_config=budget,
        ).select_rect(float(rect[0]), float(rect[1]), float(rect[2]), float(rect[3]))
        return selection_event(
            result,
            camera,
            threshold=float(query["threshold"]),
            degraded_stages=event.get("degraded_stages", []),
        )

    def _replay_hover(
        self,
        scene: Scene,
        camera: Camera,
        event: dict[str, Any],
    ) -> dict[str, Any]:
        point = event["point"]
        columns = int(event["bucket_grid"][0])
        budget = BudgetConfig(
            budget_ms=1e15,
            memory_budget_bytes=2**62,
            bucket_grids=(columns,),
            force_bucket_columns=columns,
            actual_degradation=False,
        )
        hover_config = HoverConfig(
            depth_epsilon=float(event["depth_epsilon"]),
            boundary_epsilon_px=float(event["boundary_epsilon_px"]),
            area_samples=int(event["area_samples"]),
        )
        result = QueryEngine(
            scene,
            camera,
            projection_config=self.projection_config,
            budget_config=budget,
            hover_config=hover_config,
        ).hover(
            float(point[0]),
            float(point[1]),
            target_unit_id=event.get("target_unit_id"),
        )
        return hover_event(result, camera)
