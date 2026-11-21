"""Background synchronization jobs and one-at-a-time source admission."""

from __future__ import annotations

import copy
import os
import threading
import time
import uuid
from dataclasses import dataclass, field, replace
from pathlib import Path

from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_TASK_FAILED,
    E_VERSION_STALE,
    SyncError,
    as_error_dict,
)
from .models import SyncConfig
from .pipeline import run_pipeline
from .store import ResultStore, Snapshot


@dataclass(frozen=True)
class SyncRequest:
    wav_path: str
    markers_path: str
    config: SyncConfig = field(default_factory=SyncConfig)
    expected_version: int | None = None


@dataclass(frozen=True)
class JobState:
    job_id: str
    source_key: str
    status: str
    phase: str
    progress: float
    detail: str
    checkpoint: str
    created_at: float
    updated_at: float
    result_version: int | None = None
    result_hash: str | None = None
    error: dict | None = None

    def to_dict(self) -> dict:
        return {
            "job_id": self.job_id,
            "source_key": self.source_key,
            "status": self.status,
            "phase": self.phase,
            "progress": self.progress,
            "detail": self.detail,
            "checkpoint": self.checkpoint,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "result_version": self.result_version,
            "result_hash": self.result_hash,
            "error": copy.deepcopy(self.error),
        }


def _source_key(path: str | Path) -> str:
    resolved = Path(path).expanduser().resolve()
    stat = resolved.stat()
    return f"{stat.st_dev}:{stat.st_ino}:{stat.st_size}:{stat.st_mtime_ns}"


class SyncService:
    """Coordinate jobs while keeping all UI-facing calls non-blocking."""

    def __init__(self, output_root: str | Path):
        self.output_root = Path(output_root).expanduser().resolve()
        self.output_root.mkdir(parents=True, exist_ok=True)
        self.store = ResultStore(self.output_root)
        self._lock = threading.RLock()
        self._condition = threading.Condition(self._lock)
        self._jobs: dict[str, JobState] = {}
        self._cancel_events: dict[str, threading.Event] = {}
        self._active_by_source: dict[str, str] = {}
        self._threads: dict[str, threading.Thread] = {}

    def submit(self, request: SyncRequest) -> JobState:
        request.config.validate()
        try:
            source_key = _source_key(request.wav_path)
        except OSError as exc:
            raise SyncError(E_TASK_FAILED, f"cannot stat WAV file: {exc}") from exc
        with self._lock:
            if request.expected_version is not None:
                current = self.store.current
                if current is None or current.version != request.expected_version:
                    raise SyncError(
                        E_VERSION_STALE,
                        "requested base version is not the committed version",
                        {
                            "requested_version": request.expected_version,
                            "current_version": current.version if current else None,
                        },
                    )
            active = self._active_by_source.get(source_key)
            if active is not None:
                raise SyncError(
                    E_BUSY,
                    "the same source already has a synchronization job",
                    {"job_id": active, "source_key": source_key},
                )
            now = time.time()
            job_id = uuid.uuid4().hex
            state = JobState(
                job_id=job_id,
                source_key=source_key,
                status="queued",
                phase="queued",
                progress=0.0,
                detail="waiting for worker thread",
                checkpoint="queued",
                created_at=now,
                updated_at=now,
            )
            event = threading.Event()
            self._jobs[job_id] = state
            self._cancel_events[job_id] = event
            self._active_by_source[source_key] = job_id
            thread = threading.Thread(
                target=self._run_job,
                args=(job_id, request, event),
                name=f"track-sync-{job_id[:8]}",
                daemon=True,
            )
            self._threads[job_id] = thread
            thread.start()
            self._condition.notify_all()
            return state

    def _update(self, job_id: str, **changes) -> None:
        with self._lock:
            state = self._jobs.get(job_id)
            if state is None:
                return
            self._jobs[job_id] = replace(
                state,
                updated_at=time.time(),
                **changes,
            )
            self._condition.notify_all()

    def _run_job(
        self,
        job_id: str,
        request: SyncRequest,
        cancel_event: threading.Event,
    ) -> None:
        source_key = self._jobs[job_id].source_key
        try:
            self._update(
                job_id,
                status="running",
                phase="starting",
                detail="starting synchronization worker",
                checkpoint="start",
            )
            previous_snapshot = self.store.current
            previous_payload = previous_snapshot.payload if previous_snapshot else None

            def progress(phase: str, fraction: float, detail: str) -> None:
                if cancel_event.is_set():
                    raise SyncError(E_CANCELLED, "synchronization was cancelled")
                self._update(
                    job_id,
                    phase=phase,
                    progress=float(max(0.0, min(1.0, fraction))),
                    detail=detail,
                )

            def checkpoint(label: str) -> None:
                if cancel_event.is_set():
                    raise SyncError(E_CANCELLED, f"cancelled at checkpoint {label}")
                self._update(job_id, checkpoint=label)

            report = run_pipeline(
                request.wav_path,
                request.markers_path,
                self.output_root,
                request.config,
                previous_payload=previous_payload,
                progress=progress,
                checkpoint=checkpoint,
            )
            if cancel_event.is_set():
                raise SyncError(E_CANCELLED, "cancelled before publication")
            with self._lock:
                current = self.store.current
                if request.expected_version is not None and (
                    current is None or current.version != request.expected_version
                ):
                    raise SyncError(
                        E_VERSION_STALE,
                        "result changed while the job was running",
                        {
                            "requested_version": request.expected_version,
                            "current_version": current.version if current else None,
                        },
                    )
            snapshot = self.store.publish(report)
            self._update(
                job_id,
                status="succeeded",
                phase="complete",
                progress=1.0,
                detail="result atomically published",
                checkpoint="published",
                result_version=snapshot.version,
                result_hash=snapshot.content_hash,
            )
        except SyncError as exc:
            status = "cancelled" if exc.code == E_CANCELLED else "failed"
            self._update(
                job_id,
                status=status,
                phase=status,
                detail=str(exc),
                error=exc.to_dict(),
            )
        except BaseException as exc:
            error = as_error_dict(exc)
            self._update(
                job_id,
                status="failed",
                phase="failed",
                detail=str(exc),
                error=error,
            )
        finally:
            with self._lock:
                self._cancel_events.pop(job_id, None)
                self._threads.pop(job_id, None)
                if self._active_by_source.get(source_key) == job_id:
                    self._active_by_source.pop(source_key, None)
                self._condition.notify_all()

    def cancel(self, job_id: str) -> JobState:
        with self._lock:
            state = self._jobs.get(job_id)
            if state is None:
                raise SyncError(E_TASK_FAILED, f"unknown job: {job_id}")
            if state.status in {"succeeded", "failed", "cancelled"}:
                return state
            event = self._cancel_events.get(job_id)
            if event is not None:
                event.set()
            updated = replace(
                state,
                status="cancelling",
                phase="cancelling",
                detail="cancel requested; waiting for the next segment checkpoint",
                updated_at=time.time(),
            )
            self._jobs[job_id] = updated
            self._condition.notify_all()
            return updated

    def get_job(self, job_id: str) -> JobState:
        with self._lock:
            state = self._jobs.get(job_id)
            if state is None:
                raise SyncError(E_TASK_FAILED, f"unknown job: {job_id}")
            return state

    def list_jobs(self) -> list[JobState]:
        with self._lock:
            return sorted(self._jobs.values(), key=lambda item: item.created_at, reverse=True)

    def wait(self, job_id: str, timeout: float | None = None) -> JobState:
        deadline = None if timeout is None else time.monotonic() + timeout
        with self._condition:
            while True:
                state = self._jobs.get(job_id)
                if state is None:
                    raise SyncError(E_TASK_FAILED, f"unknown job: {job_id}")
                if (
                    state.status in {"succeeded", "failed", "cancelled"}
                    and self._active_by_source.get(state.source_key) != job_id
                ):
                    return state
                remaining = None if deadline is None else deadline - time.monotonic()
                if remaining is not None and remaining <= 0:
                    return state
                self._condition.wait(remaining)

    def is_busy(self, wav_path: str | Path) -> bool:
        try:
            key = _source_key(wav_path)
        except OSError:
            return False
        with self._lock:
            return key in self._active_by_source

    def get_result(
        self,
        version: int | None = None,
        content_hash: str | None = None,
    ) -> Snapshot:
        return self.store.get_result(version=version, content_hash_value=content_hash)

    def recover_from_disk(self) -> Snapshot | None:
        return self.store.recover()
