from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any

from .errors import BusinessError, busy, cancelled
from .library import MaterialLibrary, TransitionResult
from .models import PendingJob, TransitionRequest


@dataclass(frozen=True)
class JobSnapshot:
    active: bool
    state: str
    operation: str | None
    result: TransitionResult | None
    error: BusinessError | None
    cancel_requested: bool


class RecomputeManager:
    """Runs one cooperative background recompute at a time per material library."""

    def __init__(self, library: MaterialLibrary) -> None:
        self.library = library
        self._lock = threading.Lock()
        self._cancel_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._active = False
        self._state = "idle"
        self._operation: str | None = None
        self._result: TransitionResult | None = None
        self._error: BusinessError | None = None

    def submit(self, request: TransitionRequest) -> None:
        with self._lock:
            if self._active:
                raise busy()
            self._active = True
            self._state = "running"
            self._operation = request.operation
            self._result = None
            self._error = None
            self._cancel_event = threading.Event()
            cancel_event = self._cancel_event
            thread = threading.Thread(
                target=self._run,
                args=(request, cancel_event),
                name=f"spectral-recompute-{request.operation}",
                daemon=True,
            )
            self._thread = thread
            thread.start()

    def resume_pending(self, pending: PendingJob) -> None:
        self.submit(
            TransitionRequest(
                operation=f"resume:{pending.operation}",
                pipeline=pending.requested_pipeline,
                material_update=pending.material_update,
                rollback=pending.rollback,
            )
        )

    def _run(
        self, request: TransitionRequest, cancel_event: threading.Event
    ) -> None:
        try:
            result = self.library.run_transition_sync(
                request, cancel_event=cancel_event
            )
            with self._lock:
                self._result = result
                self._state = "published"
        except BusinessError as exc:
            with self._lock:
                self._error = exc
                self._state = "cancelled" if exc.code == cancelled().code else "failed"
        except BaseException as exc:  # pragma: no cover - defensive thread boundary
            wrapped = BusinessError(
                "E_RUNTIME",
                f"background recompute failed: {type(exc).__name__}: {exc}",
            )
            with self._lock:
                self._error = wrapped
                self._state = "failed"
        finally:
            with self._lock:
                self._active = False
            self._cancel_event.clear()

    def cancel(self) -> bool:
        with self._lock:
            if not self._active:
                return False
            self._cancel_event.set()
            self._state = "cancelling"
            return True

    def snapshot(self) -> JobSnapshot:
        with self._lock:
            return JobSnapshot(
                active=self._active,
                state=self._state,
                operation=self._operation,
                result=self._result,
                error=self._error,
                cancel_requested=self._cancel_event.is_set(),
            )

    def wait(self, timeout: float | None = None) -> JobSnapshot:
        with self._lock:
            thread = self._thread
        if thread is not None:
            thread.join(timeout)
        return self.snapshot()
