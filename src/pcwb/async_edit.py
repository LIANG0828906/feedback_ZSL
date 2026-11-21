"""Background editing with whole-snapshot publication and cancellation."""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, replace
from typing import Any

from .cache import OperationCache
from .errors import BusinessError, EditBusy, OperationCancelled
from .model import PointCloud, PointCloudSnapshot
from .operators import OperationRecord, OperatorResult, run_operator
from .repository import CommitResult, PointCloudRepository
from .selection import Selection
from .util import normalize_json_value

OperatorRunner = Callable[..., OperatorResult]
LOGGER = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class EditStatus:
    state: str
    operator: str | None
    base_version: int | None
    base_hash: str | None
    phase: str
    fraction: float
    elapsed_ms: float
    threshold_ms: float
    show_progress: bool
    cache_hit: bool
    result_version: int | None
    error: dict[str, Any] | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "state": self.state,
            "operator": self.operator,
            "base_version": self.base_version,
            "base_hash": self.base_hash,
            "phase": self.phase,
            "fraction": round(self.fraction, 6),
            "elapsed_ms": round(self.elapsed_ms, 6),
            "threshold_ms": self.threshold_ms,
            "show_progress": self.show_progress,
            "cache_hit": self.cache_hit,
            "result_version": self.result_version,
            "error": self.error,
        }


class AsyncEditController:
    """Owns the atomically published display snapshot.

    Workers only compute and commit.  They never mutate arrays reachable from
    the renderer.  A successful commit is published with one reference swap.
    """

    def __init__(
        self,
        repository: PointCloudRepository,
        *,
        cache: OperationCache | None = None,
        progress_threshold_ms: float = 250.0,
        operator_runner: OperatorRunner = run_operator,
    ) -> None:
        self.repository = repository
        self.cache = cache
        self.progress_threshold_ms = float(progress_threshold_ms)
        self._operator_runner = operator_runner
        self._published_lock = threading.Lock()
        self._published = repository.current_snapshot
        self._state_lock = threading.RLock()
        self._thread: threading.Thread | None = None
        self._cancel_event: threading.Event | None = None
        self._base: PointCloudSnapshot | None = None
        self._start_ns = 0
        self._status = EditStatus(
            state="idle",
            operator=None,
            base_version=None,
            base_hash=None,
            phase="idle",
            fraction=0.0,
            elapsed_ms=0.0,
            threshold_ms=self.progress_threshold_ms,
            show_progress=False,
            cache_hit=False,
            result_version=None,
            error=None,
        )
        self._completion = threading.Event()
        self._completion.set()
        self._last_commit: CommitResult | None = None
        self._last_duration_ms: float | None = None

    def display_snapshot(self) -> PointCloudSnapshot:
        with self._published_lock:
            return self._published

    def _publish(self, snapshot: PointCloudSnapshot) -> None:
        with self._published_lock:
            self._published = snapshot

    def _set_status(self, **changes: Any) -> None:
        with self._state_lock:
            self._status = replace(self._status, **changes)

    def _progress(self, phase: str, fraction: float) -> None:
        elapsed_ms = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
        with self._state_lock:
            if self._status.state not in {"running", "committing"}:
                return
            self._status = replace(
                self._status,
                phase=phase,
                fraction=max(self._status.fraction, min(max(fraction, 0.0), 1.0)),
                elapsed_ms=elapsed_ms,
                show_progress=elapsed_ms >= self.progress_threshold_ms,
            )

    def submit(
        self,
        operator: str,
        params: Mapping[str, Any] | None,
        selection: Selection,
    ) -> EditStatus:
        with self._state_lock:
            if self._thread is not None and self._thread.is_alive():
                raise EditBusy("an edit is already running")
            base = self.repository.current_snapshot
            normalized_params = normalize_json_value(dict(params or {}))
            cancel_event = threading.Event()
            completion = threading.Event()
            self._base = base
            self._cancel_event = cancel_event
            self._completion = completion
            self._start_ns = time.perf_counter_ns()
            self._last_commit = None
            self._last_duration_ms = None
            self._status = EditStatus(
                state="running",
                operator=operator,
                base_version=base.version,
                base_hash=base.content_hash,
                phase="starting",
                fraction=0.0,
                elapsed_ms=0.0,
                threshold_ms=self.progress_threshold_ms,
                show_progress=False,
                cache_hit=False,
                result_version=None,
                error=None,
            )
            thread = threading.Thread(
                target=self._run,
                name=f"pcwb-edit-{operator}",
                args=(
                    base,
                    selection,
                    operator,
                    normalized_params,
                    cancel_event,
                    completion,
                ),
                daemon=True,
            )
            self._thread = thread
            thread.start()
        return self.status()

    def _run(
        self,
        base: PointCloudSnapshot,
        selection: Selection,
        operator: str,
        params: dict[str, Any],
        cancel_event: threading.Event,
        completion: threading.Event,
    ) -> None:
        cloud: PointCloud | None = None
        try:
            if selection.version_hash != base.content_hash:
                from .errors import SelectionInvalid

                raise SelectionInvalid(
                    "selection is stale for the edit base",
                    selection_hash=selection.version_hash,
                    base_hash=base.content_hash,
                )
            cache_hit = False
            record: OperationRecord | None = None
            if self.cache is not None:
                cached = self.cache.get(base, operator, params)
                if cached is not None:
                    cache_hit = True
                    cloud = cached
                    record = OperationRecord(
                        operator=operator,
                        params=params,
                        input_version=base.version,
                        input_hash=base.content_hash,
                        output_hash=cloud.content_hash,
                        input_count=base.count,
                        output_count=cloud.count,
                        selection_count=selection.count,
                    )
            if cloud is None:
                result = self._operator_runner(
                    base,
                    selection,
                    operator,
                    params,
                    cancel_event=cancel_event,
                    progress=self._progress,
                )
                cloud = result.cloud
                record = result.record
            self._set_status(cache_hit=cache_hit)
            if cancel_event.is_set():
                raise OperationCancelled("edit cancelled before commit")
            self._progress("commit", 0.98)
            self._set_status(state="committing")
            assert record is not None
            commit = self.repository.commit(
                cloud, base_version=base.version, record=record
            )
            self._publish(commit.snapshot)
            if self.cache is not None and not cache_hit:
                try:
                    self.cache.put(base, operator, params, cloud)
                except Exception:
                    # Caching is an optimization and must never turn a
                    # successful durable commit into a failed edit.
                    LOGGER.exception("cache write failed after successful commit")
            self._last_commit = commit
            elapsed = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
            self._last_duration_ms = elapsed
            self._set_status(
                state="succeeded",
                phase="committed",
                fraction=1.0,
                elapsed_ms=elapsed,
                show_progress=elapsed >= self.progress_threshold_ms,
                result_version=commit.version,
            )
        except OperationCancelled as exc:
            elapsed = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
            self._set_status(
                state="cancelled",
                phase="cancelled",
                elapsed_ms=elapsed,
                show_progress=elapsed >= self.progress_threshold_ms,
                error=exc.to_dict(),
            )
        except BusinessError as exc:
            elapsed = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
            self._set_status(
                state="failed",
                phase="failed",
                elapsed_ms=elapsed,
                show_progress=elapsed >= self.progress_threshold_ms,
                error=exc.to_dict(),
            )
        except Exception as exc:  # noqa: BLE001 - worker failures must not kill UI
            elapsed = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
            self._set_status(
                state="failed",
                phase="failed",
                elapsed_ms=elapsed,
                error={
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "type": type(exc).__name__,
                },
            )
        finally:
            cloud = None
            with self._state_lock:
                self._thread = None
                self._cancel_event = None
                self._base = None
                completion.set()

    def cancel(self) -> bool:
        with self._state_lock:
            event = self._cancel_event
            if event is None or event.is_set():
                return False
            event.set()
            if self._status.state == "running":
                self._status = replace(
                    self._status, phase="cancelling", state="cancelling"
                )
            return True

    def status(self) -> EditStatus:
        with self._state_lock:
            status = self._status
            if status.state in {"running", "committing", "cancelling"}:
                elapsed = (time.perf_counter_ns() - self._start_ns) / 1_000_000.0
                status = replace(
                    status,
                    elapsed_ms=elapsed,
                    show_progress=elapsed >= self.progress_threshold_ms,
                )
            return status

    def wait(self, timeout: float | None = None) -> bool:
        with self._state_lock:
            completion = self._completion
        return completion.wait(timeout)

    @property
    def last_commit(self) -> CommitResult | None:
        with self._state_lock:
            return self._last_commit

    @property
    def last_duration_ms(self) -> float | None:
        with self._state_lock:
            return self._last_duration_ms

    def sync_current(self) -> PointCloudSnapshot:
        with self._state_lock:
            if self._thread is not None and self._thread.is_alive():
                raise EditBusy("cannot change versions while an edit is running")
        snapshot = self.repository.current_snapshot
        if self.cache is not None:
            self.cache.invalidate_after_version(snapshot.version)
        self._publish(snapshot)
        with self._state_lock:
            self._status = EditStatus(
                state="idle",
                operator=None,
                base_version=None,
                base_hash=None,
                phase="idle",
                fraction=0.0,
                elapsed_ms=0.0,
                threshold_ms=self.progress_threshold_ms,
                show_progress=False,
                cache_hit=False,
                result_version=snapshot.version,
                error=None,
            )
        return snapshot

    def close(self, timeout: float = 5.0) -> None:
        self.cancel()
        with self._state_lock:
            thread = self._thread
        if thread is not None:
            thread.join(timeout)
