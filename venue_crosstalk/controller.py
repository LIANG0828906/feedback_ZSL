"""Background recompute controller with process-local E_BUSY arbitration."""

from __future__ import annotations

import gc
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

from .config import AnalysisConfig
from .errors import E_BUSY, E_CANCELLED, VenueError
from .pipeline import AnalysisPipeline

_ACTIVE_WORKSPACES: set[str] = set()
_ACTIVE_LOCK = threading.Lock()


@dataclass
class JobEvent:
    event: str
    payload: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"event": self.event, **self.payload}


class RecomputeController:
    """Own one asynchronous analysis job at a time for a workspace."""

    def __init__(
        self,
        workspace: str | Path,
        *,
        cache_root: str | Path | None = None,
        trace_path: str | Path | None = None,
        on_event: Callable[[JobEvent], None] | None = None,
    ) -> None:
        self.workspace = str(Path(workspace).resolve())
        self.cache_root = cache_root
        self.trace_path = trace_path
        self.on_event = on_event
        self._lock = threading.Lock()
        self._cancel = threading.Event()
        self._thread: threading.Thread | None = None
        self._active = False
        self._events: list[JobEvent] = []
        self._job_id = 0
        self._last_result: dict[str, Any] | None = None
        self._last_error: dict[str, Any] | None = None

    def _emit(self, event: JobEvent) -> None:
        with self._lock:
            self._events.append(event)
            handler = self.on_event
        if handler is not None:
            handler(event)

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "workspace": self.workspace,
                "active": self._active,
                "job_id": self._job_id,
                "cancelled": self._cancel.is_set(),
                "last_result_run_id": (
                    None if self._last_result is None else self._last_result.get("run_id")
                ),
                "last_error": self._last_error,
            }

    def submit(
        self,
        *,
        operation: str = "recompute",
        target_suppression_db: float | None = None,
        bands: Iterable[dict[str, Any] | tuple[str, float, float]] | None = None,
        config: AnalysisConfig | None = None,
        strict: bool = False,
        reset_trace: bool = False,
    ) -> dict[str, Any]:
        with _ACTIVE_LOCK:
            if self.workspace in _ACTIVE_WORKSPACES:
                error = VenueError(
                    E_BUSY,
                    "a recompute job is already active for this measurement",
                    {"workspace": self.workspace},
                )
                with self._lock:
                    self._last_error = error.to_dict()
                return {"ok": False, **error.to_dict()}
            _ACTIVE_WORKSPACES.add(self.workspace)
        with self._lock:
            self._active = True
            self._cancel = threading.Event()
            self._job_id += 1
            job_id = self._job_id
            cancel_event = self._cancel
        self._emit(
            JobEvent(
                "started",
                {
                    "job_id": job_id,
                    "operation": operation,
                    "target_suppression_db": target_suppression_db,
                },
            )
        )

        def worker() -> None:
            try:
                pipeline = AnalysisPipeline(
                    self.workspace,
                    cache_root=self.cache_root,
                    trace_path=self.trace_path,
                    reset_trace=reset_trace,
                )
                result = pipeline.run(
                    operation=operation,
                    config=config,
                    target_suppression_db=target_suppression_db,
                    bands=bands,
                    strict=strict,
                    cancel_event=cancel_event,
                )
                with self._lock:
                    self._last_result = result
                    self._last_error = None
                self._emit(
                    JobEvent(
                        "completed",
                        {
                            "job_id": job_id,
                            "run_id": result.get("run_id"),
                            "status": result.get("status"),
                            "summary": result.get("summary", {}),
                        },
                    )
                )
            except VenueError as exc:
                if exc.code == E_CANCELLED:
                    event_name = "cancelled"
                else:
                    event_name = "failed"
                with self._lock:
                    self._last_error = exc.to_dict()
                self._emit(
                    JobEvent(
                        event_name,
                        {
                            "job_id": job_id,
                            "code": exc.code,
                            "message": exc.message,
                            "details": exc.details,
                        },
                    )
                )
            except Exception as exc:  # defensive thread boundary
                error = {
                    "code": "E_INTERNAL",
                    "message": f"{type(exc).__name__}: {exc}",
                    "details": {},
                }
                with self._lock:
                    self._last_error = error
                self._emit(JobEvent("failed", {"job_id": job_id, **error}))
            finally:
                with _ACTIVE_LOCK:
                    _ACTIVE_WORKSPACES.discard(self.workspace)
                with self._lock:
                    self._active = False
                    self._thread = None
                gc.collect()

        thread = threading.Thread(
            target=worker,
            name=f"crosstalk-recompute-{job_id}",
            daemon=True,
        )
        with self._lock:
            self._thread = thread
        thread.start()
        return {"ok": True, "job_id": job_id}

    def cancel(self) -> bool:
        with self._lock:
            if not self._active:
                return False
            self._cancel.set()
            job_id = self._job_id
        self._emit(JobEvent("cancel_requested", {"job_id": job_id}))
        return True

    def poll(self) -> list[JobEvent]:
        with self._lock:
            events = list(self._events)
            self._events.clear()
        return events

    def wait(self, timeout: float | None = None) -> list[JobEvent]:
        deadline = None if timeout is None else time.monotonic() + timeout
        collected: list[JobEvent] = []
        while True:
            collected.extend(self.poll())
            with self._lock:
                thread = self._thread
            if thread is None or not thread.is_alive():
                break
            if deadline is not None and time.monotonic() >= deadline:
                break
            time.sleep(0.01)
        collected.extend(self.poll())
        return collected

    @property
    def last_result(self) -> dict[str, Any] | None:
        with self._lock:
            return self._last_result
