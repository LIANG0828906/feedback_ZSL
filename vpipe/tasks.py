from __future__ import annotations

import hashlib
import json
import threading
from dataclasses import dataclass, field
from typing import Any, Callable

from .boolean import BooleanOptions, BooleanResult, boolean_paths
from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_EXPORT_DEGRADED,
    E_INTERNAL,
    E_PREVIEW_STALE,
    E_PROJECT_MISMATCH,
    PipelineError,
)
from .io import export_path, parse_path
from .metrics import ErrorReport, ErrorThresholds, evaluate_error
from .model import PathData
from .project import ProjectData
from .raster import PreviewService, RasterFrame, RasterSpec, rasterize_path


@dataclass
class CommittedResult:
    result_id: str
    result_hash: str
    path: PathData
    operation: str
    tolerance: float
    thresholds: ErrorThresholds
    metrics: ErrorReport
    boolean_report: dict[str, Any]
    input_a_hash: str
    input_b_hash: str
    committed: bool = True

    @property
    def exportable(self) -> bool:
        return self.committed and self.metrics.exportable

    def to_dict(self) -> dict[str, Any]:
        return {
            "result_id": self.result_id,
            "result_hash": self.result_hash,
            "operation": self.operation,
            "tolerance": self.tolerance,
            "thresholds": self.thresholds.to_dict(),
            "metrics": self.metrics.to_dict(),
            "boolean_report": self.boolean_report,
            "input_a_hash": self.input_a_hash,
            "input_b_hash": self.input_b_hash,
            "committed": self.committed,
            "exportable": self.exportable,
        }


@dataclass
class TaskInfo:
    task_id: str
    pair_key: str
    operation: str
    tolerance: float
    status: str = "running"
    result_hash: str | None = None
    error: dict[str, Any] | None = None
    cancel_requested: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "task_id": self.task_id,
            "pair_key": self.pair_key,
            "operation": self.operation,
            "tolerance": self.tolerance,
            "status": self.status,
            "result_hash": self.result_hash,
            "error": self.error,
            "cancel_requested": self.cancel_requested,
        }


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def check(self) -> None:
        if self._event.is_set():
            raise PipelineError(
                E_CANCELLED,
                "boolean task was cancelled cooperatively",
            )


class ResultStore:
    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._last: CommittedResult | None = None
        self._by_hash: dict[str, CommittedResult] = {}
        self.preview = PreviewService()

    def commit(self, result: CommittedResult, frame: RasterFrame) -> CommittedResult:
        with self._lock:
            self.preview.mark_committed(result.result_hash)
            self.preview.publish(result.result_hash, frame)
            self._by_hash[result.result_hash] = result
            self._last = result
            return result

    def latest(self) -> CommittedResult | None:
        with self._lock:
            return self._last

    def get(self, result_hash: str) -> CommittedResult | None:
        with self._lock:
            return self._by_hash.get(result_hash)

    def committed_hashes(self) -> list[str]:
        with self._lock:
            return list(self._by_hash)

    def snapshot(self) -> tuple[CommittedResult, RasterFrame]:
        with self._lock:
            if self._last is None:
                raise PipelineError(
                    E_PREVIEW_STALE,
                    "no committed result is available for preview",
                )
            frame = self.preview.read(self._last.result_hash)
            return self._last, frame

    def read_preview(self, expected_result_hash: str) -> RasterFrame:
        with self._lock:
            return self.preview.read(expected_result_hash)


def pair_fingerprint(path_a: PathData, path_b: PathData) -> str:
    def geometry_key(path: PathData) -> str:
        payload = {
            "fill_rule": path.fill_rule,
            "contours": [
                {
                    "start": [
                        round(contour.segments[0].start.x, 12),
                        round(contour.segments[0].start.y, 12),
                    ],
                    "segments": [
                        {
                            "kind": segment.kind,
                            "end": [
                                round(segment.end.x, 12),
                                round(segment.end.y, 12),
                            ],
                            "c1": (
                                [
                                    round(segment.control1.x, 12),
                                    round(segment.control1.y, 12),
                                ]
                                if segment.control1 is not None
                                else None
                            ),
                            "c2": (
                                [
                                    round(segment.control2.x, 12),
                                    round(segment.control2.y, 12),
                                ]
                                if segment.control2 is not None
                                else None
                            ),
                        }
                        for segment in contour.segments
                    ],
                }
                for contour in path.contours
            ],
        }
        encoded = json.dumps(
            payload,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return hashlib.sha256(encoded).hexdigest()

    keys = sorted([geometry_key(path_a), geometry_key(path_b)])
    return hashlib.sha256(("|".join(keys)).encode("ascii")).hexdigest()


class BooleanService:
    """One in-flight boolean task per input geometry pair."""

    def __init__(self) -> None:
        self.store = ResultStore()
        self._lock = threading.RLock()
        self._active: dict[str, str] = {}
        self._tasks: dict[str, TaskInfo] = {}
        self._tokens: dict[str, CancellationToken] = {}
        self._done: dict[str, threading.Event] = {}
        self._counter = 0
        self.last_error: PipelineError | None = None

    def _claim(
        self,
        path_a: PathData,
        path_b: PathData,
        operation: str,
        tolerance: float,
    ) -> tuple[str, str, CancellationToken]:
        pair_key = pair_fingerprint(path_a, path_b)
        with self._lock:
            if pair_key in self._active:
                raise PipelineError(
                    E_BUSY,
                    "a boolean task for this path pair is already running",
                    pair_key=pair_key,
                    active_task_id=self._active[pair_key],
                )
            self._counter += 1
            task_id = f"task-{self._counter:06d}-{pair_key[:8]}"
            token = CancellationToken()
            self._active[pair_key] = task_id
            self._tokens[task_id] = token
            self._done[task_id] = threading.Event()
            self._tasks[task_id] = TaskInfo(
                task_id=task_id,
                pair_key=pair_key,
                operation=operation,
                tolerance=tolerance,
            )
            return task_id, pair_key, token

    def submit(
        self,
        path_a: PathData,
        path_b: PathData,
        operation: str,
        tolerance: float | None = None,
        thresholds: ErrorThresholds | None = None,
        raster_spec: RasterSpec | None = None,
        commit_gate: Callable[[], None] | None = None,
    ) -> str:
        tolerance = tolerance if tolerance is not None else max(
            path_a.tolerance,
            path_b.tolerance,
        )
        thresholds = thresholds or ErrorThresholds()
        task_id, pair_key, token = self._claim(
            path_a,
            path_b,
            operation,
            tolerance,
        )
        thread = threading.Thread(
            target=self._execute,
            args=(
                task_id,
                pair_key,
                path_a,
                path_b,
                operation,
                tolerance,
                thresholds,
                raster_spec,
                token,
                commit_gate,
                False,
            ),
            name=task_id,
            daemon=True,
        )
        thread.start()
        return task_id

    def run_sync(
        self,
        path_a: PathData,
        path_b: PathData,
        operation: str,
        tolerance: float | None = None,
        thresholds: ErrorThresholds | None = None,
        raster_spec: RasterSpec | None = None,
        commit_gate: Callable[[], None] | None = None,
    ) -> CommittedResult:
        tolerance = tolerance if tolerance is not None else max(
            path_a.tolerance,
            path_b.tolerance,
        )
        thresholds = thresholds or ErrorThresholds()
        task_id, pair_key, token = self._claim(
            path_a,
            path_b,
            operation,
            tolerance,
        )
        return self._execute(
            task_id,
            pair_key,
            path_a,
            path_b,
            operation,
            tolerance,
            thresholds,
            raster_spec,
            token,
            commit_gate,
            True,
        )

    def _execute(
        self,
        task_id: str,
        pair_key: str,
        path_a: PathData,
        path_b: PathData,
        operation: str,
        tolerance: float,
        thresholds: ErrorThresholds,
        raster_spec: RasterSpec | None,
        token: CancellationToken,
        commit_gate: Callable[[], None] | None,
        synchronous: bool,
    ) -> CommittedResult:
        boolean_result: BooleanResult | None = None
        metrics: ErrorReport | None = None
        frame: RasterFrame | None = None
        record: CommittedResult | None = None
        try:
            token.check()
            boolean_result = boolean_paths(
                path_a,
                path_b,
                BooleanOptions(
                    operation=operation,
                    tolerance=tolerance,
                    cancel_check=token.check,
                ),
            )
            token.check()
            metrics, frame, _ = evaluate_error(
                path_a,
                path_b,
                operation,
                boolean_result.path,
                tolerance,
                thresholds,
                raster_spec,
            )
            if commit_gate is not None:
                commit_gate()
            token.check()
            result_hash = boolean_result.path.content_hash()
            record = CommittedResult(
                result_id=f"{operation}:{result_hash.split(':')[-1][:12]}",
                result_hash=result_hash,
                path=boolean_result.path,
                operation=operation,
                tolerance=tolerance,
                thresholds=thresholds,
                metrics=metrics,
                boolean_report=boolean_result.report,
                input_a_hash=path_a.content_hash(),
                input_b_hash=path_b.content_hash(),
                committed=True,
            )
            self.store.commit(record, frame)
            with self._lock:
                info = self._tasks[task_id]
                info.status = "committed"
                info.result_hash = result_hash
            return record
        except PipelineError as exc:
            with self._lock:
                info = self._tasks[task_id]
                info.status = "cancelled" if exc.code == E_CANCELLED else "failed"
                info.error = exc.to_dict()
                info.cancel_requested = token.cancelled
                self.last_error = exc
            if synchronous:
                raise
            return None  # type: ignore[return-value]
        except Exception as exc:
            wrapped = PipelineError(
                E_INTERNAL,
                "unexpected boolean task failure",
                error_type=type(exc).__name__,
                error=str(exc),
            )
            with self._lock:
                info = self._tasks[task_id]
                info.status = "failed"
                info.error = wrapped.to_dict()
                self.last_error = wrapped
            if synchronous:
                raise wrapped from exc
            return None  # type: ignore[return-value]
        finally:
            boolean_result = None
            metrics = None
            frame = None
            record = None
            with self._lock:
                if self._active.get(pair_key) == task_id:
                    self._active.pop(pair_key, None)
                event = self._done.get(task_id)
                if event is not None:
                    event.set()

    def cancel(self, task_id: str) -> bool:
        with self._lock:
            token = self._tokens.get(task_id)
            info = self._tasks.get(task_id)
            if token is None or info is None or info.status != "running":
                return False
            info.cancel_requested = True
            token.cancel()
            return True

    def wait(self, task_id: str, timeout: float | None = None) -> TaskInfo | None:
        with self._lock:
            event = self._done.get(task_id)
        if event is not None:
            event.wait(timeout)
        with self._lock:
            return self._tasks.get(task_id)

    def task(self, task_id: str) -> TaskInfo | None:
        with self._lock:
            return self._tasks.get(task_id)

    def active_tasks(self) -> list[str]:
        with self._lock:
            return list(self._active.values())

    def export_result(
        self,
        result: CommittedResult,
        version: int,
        destination: str | None = None,
    ) -> tuple[dict[str, Any], Any]:
        if not result.committed or not result.metrics.exportable:
            raise PipelineError(
                E_EXPORT_DEGRADED,
                "degraded or uncommitted results cannot be exported",
                result_id=result.result_id,
                committed=result.committed,
                degraded=result.metrics.degraded,
                reasons=result.metrics.reasons,
            )
        return export_path(result.path, version, destination)

    def restore_project(self, project: ProjectData) -> CommittedResult | None:
        payload = project.last_committed
        if payload is None:
            return None
        result_path = parse_path(payload["result"], "project:last_result")
        result_hash = payload.get("result_hash")
        if not isinstance(result_hash, str):
            raise PipelineError(
                E_PROJECT_MISMATCH,
                "last committed result hash is missing",
            )
        metrics = ErrorReport.from_dict(payload["metrics"])
        spec_data = metrics.raster_spec
        bounds_data = dict(spec_data.get("bounds", {}))
        if not bounds_data:
            raise PipelineError(
                E_PROJECT_MISMATCH,
                "last committed raster specification is missing",
            )
        from .model import Bounds

        spec = RasterSpec(
            width=int(spec_data["width"]),
            height=int(spec_data["height"]),
            bounds=Bounds(
                float(bounds_data["min_x"]),
                float(bounds_data["min_y"]),
                float(bounds_data["max_x"]),
                float(bounds_data["max_y"]),
            ),
            background_rgba=tuple(spec_data.get("background_rgba", [246, 247, 249, 255])),
            fill_rgba=tuple(spec_data.get("fill_rgba", [31, 111, 235, 210])),
        )
        frame = rasterize_path(
            result_path,
            spec,
            project.tolerance,
            result_hash,
        )
        thresholds = ErrorThresholds(
            max_area_relative_error=float(
                payload.get("thresholds", {}).get("max_area_relative_error", 0.02)
            ),
            max_boundary_error=float(
                payload.get("thresholds", {}).get("max_boundary_error", 0.5)
            ),
        )
        record = CommittedResult(
            result_id=str(payload.get("result_id", result_hash)),
            result_hash=result_hash,
            path=result_path,
            operation=str(payload.get("operation", project.operation)),
            tolerance=float(payload.get("tolerance", project.tolerance)),
            thresholds=thresholds,
            metrics=metrics,
            boolean_report=dict(payload.get("boolean_report", {})),
            input_a_hash=project.path_a.content_hash(),
            input_b_hash=project.path_b.content_hash(),
            committed=True,
        )
        self.store.commit(record, frame)
        return record

