"""Background edit engine isolated from the render thread.

The engine owns one worker thread and an atomic ``current`` snapshot
reference. The render thread only ever reads complete committed
:class:`~pcwb.cloud.PointCloud` objects; publishing a result is a single
reference swap under a lock, so a partial update is impossible. Long
operators receive a :class:`CancelToken` and cooperate on cancellation
checkpoints; a cancelled job releases its staging buffers and leaves both the
store and the live snapshot on the last committed version.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field

from .cloud import PointCloud
from .errors import E_CANCELLED, PointCloudError
from .operators import apply_operator
from .selection import Selection
from .versions import VersionStore


class CancelToken:
    """Cooperative cancellation flag plus progress reporting."""

    def __init__(self, progress_threshold_s: float = 0.5):
        self._cancel = threading.Event()
        self._lock = threading.Lock()
        self.progress = 0.0
        self.message = "queued"
        self.progress_threshold_s = float(progress_threshold_s)

    def cancel(self) -> None:
        self._cancel.set()

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def checkpoint(self) -> None:
        """Raise ``E_CANCELLED`` if cancellation was requested."""
        if self._cancel.is_set():
            raise PointCloudError(E_CANCELLED, "edit cancelled at checkpoint")

    def report(self, progress: float, message: str) -> None:
        with self._lock:
            self.progress = max(0.0, min(1.0, float(progress)))
            self.message = message

    def snapshot_state(self) -> dict:
        with self._lock:
            return {"progress": self.progress, "message": self.message,
                    "cancelled": self._cancel.is_set()}


@dataclass
class EditJob:
    job_id: str
    operator: str
    params: dict
    selection: Selection
    base_version: int
    base_hash: str
    token: CancelToken = field(default_factory=CancelToken)


@dataclass
class JobOutcome:
    job_id: str
    ok: bool
    cloud: PointCloud | None
    error_code: str | None
    record: dict | None


class EditEngine:
    """Single-worker edit engine publishing atomic cloud snapshots."""

    def __init__(self, store: VersionStore,
                 initial: PointCloud | None = None):
        self.store = store
        self._lock = threading.RLock()
        self._current: PointCloud | None = initial
        self._worker: threading.Thread | None = None
        self._active: EditJob | None = None
        self._done = threading.Event()
        self._last_outcome: JobOutcome | None = None
        self._observers: list = []
        self._job_counter = 0

    # ------------------------------------------------------------------ snapshot
    def current(self) -> PointCloud:
        """Return the complete, committed snapshot visible to rendering."""
        with self._lock:
            if self._current is None:
                self._current = self.store.head()
            return self._current

    def publish(self, cloud: PointCloud) -> None:
        """Atomic whole-snapshot replacement (single reference swap)."""
        with self._lock:
            self._current = cloud

    # ------------------------------------------------------------------ jobs
    def submit(self, operator: str, selection: Selection,
               params: dict | None = None,
               progress_threshold_s: float = 0.5) -> EditJob:
        """Queue one edit job; rejects overlapping jobs to keep ordering sane."""
        with self._lock:
            if self._worker is not None and self._worker.is_alive():
                raise RuntimeError("an edit job is already running")
            base = self.current()
            selection.check(base)
            self._job_counter += 1
            job = EditJob(
                job_id=f"job-{self._job_counter:06d}",
                operator=operator,
                params=dict(params or {}),
                selection=selection,
                base_version=base.version,
                base_hash=base.sha256,
                token=CancelToken(progress_threshold_s),
            )
            self._active = job
            self._done = threading.Event()
            self._worker = threading.Thread(
                target=self._run, args=(job,), name="pcwb-edit-worker",
                daemon=True)
            self._worker.start()
            return job

    def _run(self, job: EditJob) -> None:
        outcome: JobOutcome
        try:
            base = self.current()
            if base.version != job.base_version or \
                    base.sha256 != job.base_hash:
                raise PointCloudError(
                    E_CANCELLED,
                    "base version changed before job started")
            job.token.checkpoint()
            job.token.report(0.05, f"running {job.operator}")
            params = dict(job.params)
            if job.operator == "smooth":
                params.setdefault(
                    "cancel_check", job.token.checkpoint)
                params.setdefault(
                    "progress_cb",
                    lambda p, m: job.token.report(0.05 + 0.75 * p, m))
            result = apply_operator(
                job.operator, base, job.selection, params)
            job.token.checkpoint()
            job.token.report(0.8, "committing atomically")
            record = dict(result.record)
            record["job_id"] = job.job_id
            cloud = self.store.commit(
                result.xyz, result.rgb, record=record,
                parent_version=job.base_version)
            job.token.checkpoint()
            self.publish(cloud)
            outcome = JobOutcome(job.job_id, True, cloud, None, record)
        except PointCloudError as exc:
            outcome = JobOutcome(
                job.job_id, False, None, exc.code,
                {"job_id": job.job_id, "error": exc.code,
                 "message": exc.message})
        except Exception as exc:  # pragma: no cover - defensive
            outcome = JobOutcome(
                job.job_id, False, None, "E_COMMIT_FAILED",
                {"job_id": job.job_id, "error": "E_COMMIT_FAILED",
                 "message": str(exc)})
        finally:
            job.token.report(1.0, "done" if outcome.ok else "aborted")
            with self._lock:
                self._last_outcome = outcome
                self._active = None
                self._done.set()
            for observer in list(self._observers):
                observer(outcome)

    # ------------------------------------------------------------------ control
    def cancel(self) -> None:
        """Request cooperative cancellation of the active job."""
        with self._lock:
            if self._active is not None:
                self._active.token.cancel()

    def wait(self, timeout: float | None = None) -> bool:
        """Block until the active job finishes; returns completion flag."""
        return self._done.wait(timeout)

    def shutdown(self) -> None:
        self.cancel()
        if self._worker is not None:
            self._worker.join(timeout=5.0)

    def progress(self) -> dict:
        with self._lock:
            if self._active is None:
                return {"running": False, "progress": 1.0,
                        "message": "idle", "cancelled": False}
            state = self._active.token.snapshot_state()
            state["running"] = self._worker is not None \
                and self._worker.is_alive()
            state["job_id"] = self._active.job_id
            state["operator"] = self._active.operator
            state["threshold_s"] = self._active.token.progress_threshold_s
            return state

    @property
    def last_outcome(self) -> JobOutcome | None:
        return self._last_outcome

    def add_observer(self, callback) -> None:
        self._observers.append(callback)
