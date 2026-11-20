from __future__ import annotations

import gc
import threading
from collections.abc import Iterable
from dataclasses import dataclass

from .engine import BuildState, CompileResult, compile_signals
from .errors import E_BUSY, CompileCancelled, HapticError
from .model import ProjectConfig, WaveSegment
from .trace import TraceLogger
from .versioning import Publisher


@dataclass(frozen=True)
class TaskSnapshot:
    sequence: int
    state: str
    message: str = ""
    published_version: int | None = None
    content_hash: str | None = None
    error: dict | None = None


class CancelToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    def is_cancelled(self) -> bool:
        return self._event.is_set()


class RecomputeTaskManager:
    """Single-flight background compiler with cooperative cancellation."""

    def __init__(
        self,
        publisher: Publisher,
        state: BuildState,
        trace: TraceLogger,
    ) -> None:
        self._publisher = publisher
        self._state = state
        self._trace = trace
        self._lock = threading.RLock()
        self._commit_lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._token: CancelToken | None = None
        self._sequence = 0
        self._snapshot = TaskSnapshot(
            sequence=0,
            state="idle",
            message="ready",
            published_version=state.version.version,
            content_hash=state.version.content_hash,
        )

    @property
    def publisher(self) -> Publisher:
        return self._publisher

    def state(self) -> BuildState:
        with self._lock:
            return self._state

    def status(self) -> TaskSnapshot:
        with self._lock:
            snapshot = self._snapshot
        if snapshot.state in ("running", "cancelling"):
            thread = self._thread
            if thread is not None and not thread.is_alive():
                # The worker updates status in its finally block; this branch
                # only covers an unexpectedly vanished thread.
                return TaskSnapshot(
                    sequence=snapshot.sequence,
                    state="failed",
                    message="worker terminated unexpectedly",
                    published_version=self._publisher.require_current().version,
                    content_hash=self._publisher.require_current().content_hash,
                )
        return snapshot

    def submit(
        self,
        segments: Iterable[WaveSegment],
        config: ProjectConfig,
        *,
        modified_indices: Iterable[int] | None = None,
        force_full: bool = False,
    ) -> int:
        with self._lock:
            if self._thread is not None and self._thread.is_alive():
                raise HapticError(
                    E_BUSY,
                    "a recompute task is already running",
                    details={
                        "sequence": self._sequence,
                        "state": self._snapshot.state,
                    },
                )
            base_state = self._state
            self._sequence += 1
            sequence = self._sequence
            token = CancelToken()
            self._token = token
            self._snapshot = TaskSnapshot(
                sequence=sequence,
                state="running",
                message="recomputing",
                published_version=base_state.version.version,
                content_hash=base_state.version.content_hash,
            )
            thread = threading.Thread(
                target=self._worker,
                args=(
                    sequence,
                    token,
                    base_state,
                    tuple(segments),
                    config,
                    tuple(modified_indices or ()),
                    force_full,
                ),
                name=f"haptic-recompute-{sequence}",
                daemon=True,
            )
            self._thread = thread
            thread.start()
            return sequence

    def cancel(self) -> bool:
        with self._lock:
            token = self._token
            thread = self._thread
            if (
                token is None
                or thread is None
                or not thread.is_alive()
                or self._snapshot.state not in ("running", "cancelling")
            ):
                return False
            token.cancel()
            self._snapshot = TaskSnapshot(
                sequence=self._snapshot.sequence,
                state="cancelling",
                message="cancellation requested",
                published_version=self._snapshot.published_version,
                content_hash=self._snapshot.content_hash,
            )
            return True

    def wait(self, timeout: float | None = None) -> bool:
        with self._lock:
            thread = self._thread
        if thread is None:
            return True
        thread.join(timeout)
        return not thread.is_alive()

    def _publish_success(
        self,
        result: CompileResult,
        base_state: BuildState,
        token: CancelToken,
    ) -> None:
        with self._commit_lock:
            if token.is_cancelled():
                raise CompileCancelled("cancellation observed before atomic publish")
            self._publisher.publish(
                result.version,
                expected_parent_hash=base_state.version.content_hash,
            )
            with self._lock:
                self._state = result.state
            self._trace.record_build(result)

    def _worker(
        self,
        sequence: int,
        token: CancelToken,
        base_state: BuildState,
        segments: tuple[WaveSegment, ...],
        config: ProjectConfig,
        modified_indices: tuple[int, ...],
        force_full: bool,
    ) -> None:
        result: CompileResult | None = None
        try:
            result = compile_signals(
                segments,
                config,
                previous=base_state,
                modified_indices=modified_indices,
                force_full=force_full,
                cancel=token.is_cancelled,
            )
            self._publish_success(result, base_state, token)
            current = self._publisher.require_current()
            with self._lock:
                self._snapshot = TaskSnapshot(
                    sequence=sequence,
                    state="succeeded",
                    message="published",
                    published_version=current.version,
                    content_hash=current.content_hash,
                )
        except CompileCancelled as exc:
            current = self._publisher.require_current()
            with self._lock:
                self._snapshot = TaskSnapshot(
                    sequence=sequence,
                    state="cancelled",
                    message=exc.message,
                    published_version=current.version,
                    content_hash=current.content_hash,
                )
        except HapticError as exc:
            current = self._publisher.require_current()
            with self._lock:
                self._snapshot = TaskSnapshot(
                    sequence=sequence,
                    state="failed",
                    message=str(exc),
                    published_version=current.version,
                    content_hash=current.content_hash,
                    error=exc.to_dict(),
                )
        except Exception as exc:  # noqa: BLE001  # pragma: no cover
            current = self._publisher.require_current()
            with self._lock:
                self._snapshot = TaskSnapshot(
                    sequence=sequence,
                    state="failed",
                    message=f"unexpected recompute failure: {exc}",
                    published_version=current.version,
                    content_hash=current.content_hash,
                    error={
                        "code": "E_INTERNAL",
                        "message": str(exc),
                    },
                )
        finally:
            result = None
            gc.collect()
            with self._lock:
                self._token = None
