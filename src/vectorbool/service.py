from __future__ import annotations

import hashlib
import json
import threading
import uuid
from collections.abc import Callable
from dataclasses import dataclass

from .boolean import boolean_paths
from .errors import BusinessError, ErrorCode
from .model import Operation, PathSpec
from .preview import PreviewImage, render_preview

SuccessCallback = Callable[["TaskOutcome"], None]
ErrorCallback = Callable[[BusinessError], None]
CancelCallback = Callable[[], None]


@dataclass
class TaskOutcome:
    token: str
    result: object
    preview: PreviewImage | None
    committed: bool


@dataclass
class _Task:
    token: str
    key: str
    cancel_event: threading.Event
    thread: threading.Thread


def _path_pair_key(path_a: PathSpec, path_b: PathSpec) -> str:
    payload = json.dumps(
        [path_a.to_dict(), path_b.to_dict()],
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


class BooleanTaskService:
    """Threaded boolean execution with one active task per path pair."""

    def __init__(self, *, preview_width: int = 512, preview_height: int = 512) -> None:
        self._lock = threading.RLock()
        self._active: dict[str, _Task] = {}
        self._last_preview: PreviewImage | None = None
        self._last_outcome: TaskOutcome | None = None
        self.preview_width = preview_width
        self.preview_height = preview_height

    @property
    def last_preview(self) -> PreviewImage | None:
        with self._lock:
            return self._last_preview

    @property
    def last_outcome(self) -> TaskOutcome | None:
        with self._lock:
            return self._last_outcome

    @property
    def active_count(self) -> int:
        with self._lock:
            return len(self._active)

    def submit(
        self,
        path_a: PathSpec,
        path_b: PathSpec,
        operation: Operation | str,
        *,
        tolerance: float,
        on_success: SuccessCallback | None = None,
        on_error: ErrorCallback | None = None,
        on_cancel: CancelCallback | None = None,
    ) -> str:
        key = _path_pair_key(path_a, path_b)
        with self._lock:
            if key in self._active:
                raise BusinessError(
                    ErrorCode.BUSY,
                    "a boolean task for this path pair is already active",
                )
            token = uuid.uuid4().hex
            cancel_event = threading.Event()
            thread = threading.Thread(
                target=self._run,
                args=(
                    path_a,
                    path_b,
                    operation,
                    tolerance,
                    key,
                    token,
                    cancel_event,
                    on_success,
                    on_error,
                    on_cancel,
                ),
                name=f"vectorbool-{token[:8]}",
                daemon=True,
            )
            task = _Task(token, key, cancel_event, thread)
            self._active[key] = task
        thread.start()
        return token

    def submit_and_wait(
        self,
        path_a: PathSpec,
        path_b: PathSpec,
        operation: Operation | str,
        *,
        tolerance: float,
        timeout: float | None = None,
    ) -> TaskOutcome:
        errors: list[BusinessError] = []
        cancelled: list[bool] = []
        outcomes: list[TaskOutcome] = []
        token = self.submit(
            path_a,
            path_b,
            operation,
            tolerance=tolerance,
            on_success=outcomes.append,
            on_error=errors.append,
            on_cancel=lambda: cancelled.append(True),
        )
        thread = self._thread_for(token)
        if thread is not None:
            thread.join(timeout)
            if thread.is_alive():
                self.cancel(token)
                thread.join(5.0)
        if errors:
            raise errors[0]
        if cancelled:
            raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
        if not outcomes:
            raise BusinessError(ErrorCode.BOOLEAN_FAILED, "boolean task produced no outcome")
        return outcomes[0]

    def _thread_for(self, token: str) -> threading.Thread | None:
        with self._lock:
            for task in self._active.values():
                if task.token == token:
                    return task.thread
        return None

    def cancel(self, token: str) -> bool:
        with self._lock:
            for task in self._active.values():
                if task.token == token:
                    task.cancel_event.set()
                    return True
        return False

    def cancel_all(self) -> None:
        with self._lock:
            for task in self._active.values():
                task.cancel_event.set()

    def _run(
        self,
        path_a: PathSpec,
        path_b: PathSpec,
        operation: Operation | str,
        tolerance: float,
        key: str,
        token: str,
        cancel_event: threading.Event,
        on_success: SuccessCallback | None,
        on_error: ErrorCallback | None,
        on_cancel: CancelCallback | None,
    ) -> None:
        result = None
        preview = None
        try:
            result = boolean_paths(
                path_a,
                path_b,
                operation,
                tolerance=tolerance,
                cancel_check=cancel_event.is_set,
            )
            if cancel_event.is_set():
                raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
            if result.report is not None and result.report.accepted:
                preview = render_preview(
                    path_a,
                    path_b,
                    result,
                    tolerance=tolerance,
                    width=self.preview_width,
                    height=self.preview_height,
                )
            outcome = TaskOutcome(
                token=token,
                result=result,
                preview=preview,
                committed=bool(result.report and result.report.accepted),
            )
            with self._lock:
                if preview is not None:
                    self._last_preview = preview
                self._last_outcome = outcome
            if on_success is not None:
                on_success(outcome)
        except BusinessError as error:
            if error.code is ErrorCode.CANCELLED or error.code == ErrorCode.CANCELLED.value:
                if on_cancel is not None:
                    on_cancel()
            elif on_error is not None:
                on_error(error)
        except Exception as error:  # noqa: BLE001  # pragma: no cover
            wrapped = BusinessError(
                ErrorCode.BOOLEAN_FAILED,
                f"unexpected boolean failure: {error}",
            )
            if on_error is not None:
                on_error(wrapped)
        finally:
            with self._lock:
                self._active.pop(key, None)
            cancel_event.clear()
            result = None
            preview = None

