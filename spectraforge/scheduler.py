"""Bounded-concurrency segmented scheduler with pause and fault injection."""

from __future__ import annotations

import copy
import os
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from typing import Any, Callable

from .checkpoint import CheckpointManager
from .errors import WorkbenchError
from .models import Scene, SegmentSpec
from .parameters import parameter_version, validate_parameters
from .pipeline import PreparedScene, prepare_scene, process_segment
from .segmentation import estimate_segment_bytes
from .state import SegmentState


@dataclass(frozen=True)
class ResourceConfig:
    max_workers: int = 4
    per_thread_buffer_mb: int = 64

    def validate(self) -> None:
        if self.max_workers < 1 or self.max_workers > 32:
            raise WorkbenchError(
                "E_PARA",
                "max_workers is outside the allowed range",
                {"field": "max_workers", "value": self.max_workers, "min": 1, "max": 32},
            )
        if self.per_thread_buffer_mb < 1 or self.per_thread_buffer_mb > 4096:
            raise WorkbenchError(
                "E_PARA",
                "per_thread_buffer_mb is outside the allowed range",
                {"field": "per_thread_buffer_mb", "value": self.per_thread_buffer_mb, "min": 1, "max": 4096},
            )

    @property
    def capacity_bytes(self) -> int:
        return int(self.max_workers) * int(self.per_thread_buffer_mb) * 1024 * 1024


class BufferPool:
    """Blocking weighted budget; work waits instead of being dropped."""

    def __init__(self, capacity_bytes: int):
        self.capacity_bytes = int(capacity_bytes)
        self._used = 0
        self._peak = 0
        self._wait_count = 0
        self._condition = threading.Condition()

    def acquire(self, size: int, pause_event: threading.Event) -> bool:
        if size > self.capacity_bytes:
            raise WorkbenchError(
                "E_PARA",
                "Segment buffer exceeds the total resource budget",
                {"field": "per_thread_buffer_mb", "required_bytes": size, "capacity_bytes": self.capacity_bytes},
            )
        with self._condition:
            waited = False
            while self._used + size > self.capacity_bytes:
                if pause_event.is_set():
                    return False
                waited = True
                self._condition.wait(timeout=0.05)
            if waited:
                self._wait_count += 1
            self._used += size
            self._peak = max(self._peak, self._used)
            return True

    def release(self, size: int) -> None:
        with self._condition:
            self._used = max(0, self._used - size)
            self._condition.notify_all()

    def metrics(self) -> dict[str, int]:
        with self._condition:
            return {
                "capacity_bytes": self.capacity_bytes,
                "used_bytes": self._used,
                "peak_used_bytes": self._peak,
                "wait_count": self._wait_count,
            }


class ProcessingJob:
    def __init__(
        self,
        scene: Scene,
        manager: CheckpointManager,
        *,
        resource: ResourceConfig | None = None,
        params: dict[str, Any] | None = None,
        event_callback: Callable[[dict[str, Any]], None] | None = None,
    ):
        self.scene = scene
        self.manager = manager
        self.resource = resource or ResourceConfig()
        self.resource.validate()
        self._params = validate_parameters(
            params if params is not None else scene.metadata.default_parameters,
            scene.exposure_count,
        )
        self._structural_interpolation = self._params["interpolation"]
        self._param_lock = threading.RLock()
        self._pause_event = threading.Event()
        self._event_callback = event_callback
        self._prepared: PreparedScene | None = None
        self._last_result: dict[str, Any] | None = None

    def _emit(self, event: dict[str, Any]) -> None:
        if self._event_callback is not None:
            try:
                self._event_callback(event)
            except Exception:
                pass

    def set_parameters(self, params: dict[str, Any]) -> None:
        normalized = validate_parameters(params, self.scene.exposure_count)
        if normalized["interpolation"] != self._structural_interpolation:
            raise WorkbenchError(
                "E_PARA",
                "Registration interpolation is a structural setting for a running snapshot",
                {"field": "interpolation", "value": normalized["interpolation"]},
            )
        with self._param_lock:
            self._params = normalized

    def get_parameters(self) -> dict[str, Any]:
        with self._param_lock:
            return copy.deepcopy(self._params)

    def pause(self) -> None:
        self._pause_event.set()

    def cancel(self) -> None:
        self._pause_event.set()

    def _runnable_segments(self, retry_failed: bool) -> list[SegmentSpec]:
        runnable = []
        for spec in self.manager.segments:
            record = self.manager.state.records[spec.index]
            if record.state == SegmentState.PENDING:
                runnable.append(spec)
            elif record.state == SegmentState.FAILED and retry_failed:
                runnable.append(spec)
        return runnable

    def _validate_buffers(self) -> dict[int, int]:
        costs = {
            spec.index: estimate_segment_bytes(spec, self.scene.band_count)
            for spec in self.manager.segments
        }
        per_thread = int(self.resource.per_thread_buffer_mb) * 1024 * 1024
        for index, cost in costs.items():
            if cost > per_thread:
                raise WorkbenchError(
                    "E_PARA",
                    "Segment working buffer exceeds the per-thread limit",
                    {
                        "field": "per_thread_buffer_mb",
                        "segment_index": index,
                        "required_bytes": cost,
                        "limit_bytes": per_thread,
                    },
                )
        return costs

    def run(
        self,
        *,
        retry_failed: bool = True,
        pause_after: int | None = None,
        fail_segment: int | None = None,
        crash_after: int | None = None,
    ) -> dict[str, Any]:
        self._pause_event.clear()
        costs = self._validate_buffers()
        runnable = self._runnable_segments(retry_failed)
        if self._prepared is None:
            params = self.get_parameters()
            self._prepared = prepare_scene(self.scene, str(params["interpolation"]))
        prepared = self._prepared
        buffer_pool = BufferPool(self.resource.capacity_bytes)
        baseline_committed = self.manager.progress()["committed"]
        processed_since_start = 0
        failures: dict[int, dict[str, Any]] = {}
        self._emit(
            {
                "type": "run_started",
                "total": len(self.manager.segments),
                "runnable": len(runnable),
                "progress": self.manager.progress(),
                "alignment": prepared.alignment_report,
            }
        )
        if not runnable:
            self.manager.set_run_status("complete" if self.manager.is_complete() else "incomplete")
            self._last_result = {
                "ok": self.manager.is_complete(),
                "progress": self.manager.progress(),
                "failures": failures,
                "resource_metrics": buffer_pool.metrics(),
            }
            return self._last_result
        with ThreadPoolExecutor(max_workers=self.resource.max_workers, thread_name_prefix="sf-segment") as executor:
            futures = {executor.submit(self._run_one, spec, prepared, buffer_pool, costs[spec.index], fail_segment): spec for spec in runnable}
            for future in as_completed(futures):
                spec = futures[future]
                try:
                    outcome = future.result()
                except WorkbenchError as exc:
                    outcome = {"committed": False, "segment_index": spec.index, "error": exc.to_dict()}
                except Exception as exc:
                    outcome = {
                        "committed": False,
                        "segment_index": spec.index,
                        "error": WorkbenchError(
                            "E_INTERNAL",
                            "Unhandled segment failure",
                            {"reason": str(exc), "segment_index": spec.index},
                        ).to_dict(),
                    }
                if outcome.get("committed"):
                    processed_since_start += 1
                    if crash_after is not None and (
                        baseline_committed + processed_since_start >= crash_after
                    ):
                        os._exit(86)
                    if pause_after is not None and processed_since_start >= pause_after:
                        self._pause_event.set()
                elif outcome.get("error") is not None:
                    failures[spec.index] = outcome["error"]
                self._emit(
                    {
                        "type": "progress",
                        "segment_index": spec.index,
                        "result": outcome,
                        "progress": self.manager.progress(),
                    }
                )
        if self.manager.is_complete():
            status = "complete"
        elif self._pause_event.is_set():
            status = "paused"
        else:
            status = "incomplete"
        self.manager.set_run_status(status)
        self._last_result = {
            "ok": self.manager.is_complete(),
            "paused": status == "paused",
            "progress": self.manager.progress(),
            "failures": failures,
            "resource_metrics": buffer_pool.metrics(),
            "alignment": prepared.alignment_report,
        }
        self._emit({"type": "run_finished", **self._last_result})
        return self._last_result

    def _run_one(
        self,
        spec: SegmentSpec,
        prepared: PreparedScene,
        buffer_pool: BufferPool,
        cost: int,
        fail_segment: int | None,
    ) -> dict[str, Any]:
        if self._pause_event.is_set():
            return {"committed": False, "segment_index": spec.index, "skipped": True}
        acquired = buffer_pool.acquire(cost, self._pause_event)
        if not acquired:
            return {"committed": False, "segment_index": spec.index, "skipped": True}
        params: dict[str, Any] | None = None
        param_version_value: str | None = None
        transitioned = False
        try:
            if self._pause_event.is_set():
                return {"committed": False, "segment_index": spec.index, "skipped": True}
            params = self.get_parameters()
            param_version_value = parameter_version(params, self.scene.exposure_count)
            self.manager.state.transition(spec.index, SegmentState.PROCESSING)
            transitioned = True
            self._emit(
                {
                    "type": "segment_state",
                    "segment_index": spec.index,
                    "state": SegmentState.PROCESSING.value,
                    "param_version": param_version_value,
                }
            )
            if fail_segment is not None and spec.index == fail_segment:
                raise WorkbenchError(
                    "E_INTERNAL",
                    "Injected single-segment failure",
                    {"segment_index": spec.index, "injected": True},
                )
            result = process_segment(self.scene, prepared, spec, params)
            self.manager.commit_segment(result)
            self._emit(
                {
                    "type": "segment_state",
                    "segment_index": spec.index,
                    "state": SegmentState.COMMITTED.value,
                    "stats": result.stats,
                    "param_version": result.param_version,
                }
            )
            return {
                "committed": True,
                "segment_index": spec.index,
                "stats": result.stats,
                "param_version": result.param_version,
            }
        except WorkbenchError as exc:
            if transitioned:
                try:
                    self.manager.fail_segment(
                        spec.index,
                        exc,
                        params=params,
                        param_version_value=param_version_value,
                    )
                except WorkbenchError:
                    pass
            self._emit(
                {
                    "type": "segment_state",
                    "segment_index": spec.index,
                    "state": SegmentState.FAILED.value,
                    "error": exc.to_dict(),
                }
            )
            return {"committed": False, "segment_index": spec.index, "error": exc.to_dict()}
        except Exception as exc:
            wrapped = WorkbenchError(
                "E_INTERNAL",
                "Unhandled segment failure",
                {"reason": str(exc), "segment_index": spec.index},
            )
            if transitioned:
                try:
                    self.manager.fail_segment(
                        spec.index,
                        wrapped,
                        params=params,
                        param_version_value=param_version_value,
                    )
                except WorkbenchError:
                    pass
            self._emit(
                {
                    "type": "segment_state",
                    "segment_index": spec.index,
                    "state": SegmentState.FAILED.value,
                    "error": wrapped.to_dict(),
                }
            )
            return {"committed": False, "segment_index": spec.index, "error": wrapped.to_dict()}
        finally:
            buffer_pool.release(cost)
