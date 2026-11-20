"""Single-writer bake jobs with cooperative cancellation."""

from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any

from .bake import BakeEngine, BakeOutcome, BakeState
from .errors import BakeCancelled, ErrorCode, ProbeError
from .publish import IrradianceSnapshot, PublishedStore
from .scene import Scene
from .snapshot import SnapshotStore
from .strategy import BakeConfig


@dataclass(frozen=True)
class JobStatus:
    scene_id: str
    running: bool
    cancelled: bool
    published_version: int | None
    last_content_hash: str | None
    error: dict[str, Any] | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "scene_id": self.scene_id,
            "running": self.running,
            "cancelled": self.cancelled,
            "published_version": self.published_version,
            "last_content_hash": self.last_content_hash,
            "error": self.error,
        }


class BakeJobManager:
    """One scene has at most one writer; failed jobs never replace the publication."""

    _active_lock = threading.Lock()
    _active_scene_ids: set[str] = set()

    def __init__(
        self,
        scene_id: str,
        *,
        engine: BakeEngine,
        publisher: PublishedStore,
        trace: Any | None = None,
        snapshot_store: SnapshotStore | None = None,
    ) -> None:
        self.scene_id = scene_id
        self.engine = engine
        self.publisher = publisher
        self.trace = trace
        self.snapshot_store = snapshot_store
        self._lock = threading.RLock()
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix=f"bake-{scene_id}")
        self._future: Future[BakeOutcome] | None = None
        self._cancel_event: threading.Event | None = None
        self._running = False
        self._cancelled = False
        self._last_state: BakeState | None = None
        self._last_outcome: BakeOutcome | None = None
        self._last_snapshot: IrradianceSnapshot | None = None
        self._error: ProbeError | None = None

    def _active_locked(self) -> bool:
        return self._future is not None and not self._future.done()

    def start(self, scene: Scene, config: BakeConfig) -> Future[BakeOutcome]:
        with self._lock:
            if self._active_locked():
                raise ProbeError(
                    ErrorCode.BUSY,
                    "a rebake task is already active for this scene",
                    details={"scene_id": self.scene_id},
                )
            with self._active_lock:
                if self.scene_id in self._active_scene_ids:
                    raise ProbeError(
                        ErrorCode.BUSY,
                        "another manager already has an active rebake for this scene",
                        details={"scene_id": self.scene_id},
                    )
                self._active_scene_ids.add(self.scene_id)
            config.validate()
            cancel_event = threading.Event()
            self._cancel_event = cancel_event
            self._running = True
            self._cancelled = False
            self._error = None
            try:
                future = self._executor.submit(
                    self._run, scene, config, cancel_event, self._last_state
                )
            except BaseException:
                with self._active_lock:
                    self._active_scene_ids.discard(self.scene_id)
                raise
            self._future = future
            if self.trace is not None:
                self.trace.event(
                    "bake_job_started",
                    scene_id=self.scene_id,
                    strategy=config.strategy,
                    spacing=config.spacing,
                    sh_order=config.sh_order,
                    compensation_strength=config.compensation_strength,
                )
            return future

    def _run(
        self,
        scene: Scene,
        config: BakeConfig,
        cancel_event: threading.Event,
        previous: BakeState | None,
    ) -> BakeOutcome:
        outcome: BakeOutcome | None = None
        try:
            outcome = self.engine.bake(
                scene,
                config,
                previous=previous,
                cancel_event=cancel_event,
            )
            if cancel_event.is_set():
                raise BakeCancelled()
            target_version = self.publisher.next_version
            if self.snapshot_store is not None:
                self.snapshot_store.commit_state(outcome.state, version=target_version)
            snapshot = self.publisher.publish(outcome.state, version=target_version)
            with self._lock:
                self._last_state = outcome.state
                self._last_outcome = outcome
                self._last_snapshot = snapshot
            if self.trace is not None:
                self.trace.event(
                    "bake_job_published",
                    scene_id=self.scene_id,
                    version=snapshot.version,
                    content_hash=snapshot.content_hash,
                )
            return outcome
        except BakeCancelled as exc:
            with self._lock:
                self._cancelled = True
                self._error = exc
            if self.trace is not None:
                self.trace.event(
                    "bake_job_cancelled",
                    scene_id=self.scene_id,
                    published_version=(
                        None if self._last_snapshot is None else self._last_snapshot.version
                    ),
                    temporary_buffers_released=True,
                )
            raise
        except ProbeError as exc:
            with self._lock:
                self._error = exc
            if self.trace is not None:
                self.trace.event(
                    "bake_job_failed",
                    scene_id=self.scene_id,
                    error=exc.to_dict(),
                )
            raise
        except BaseException:
            if self.trace is not None:
                self.trace.event(
                    "bake_job_crashed",
                    scene_id=self.scene_id,
                    error="unexpected exception",
                )
            raise
        finally:
            with self._lock:
                self._running = False
                self._cancel_event = None
                outcome = None
                with self._active_lock:
                    self._active_scene_ids.discard(self.scene_id)

    def cancel(self) -> bool:
        with self._lock:
            if not self._active_locked() or self._cancel_event is None:
                return False
            self._cancel_event.set()
            if self.trace is not None:
                self.trace.event("bake_job_cancel_requested", scene_id=self.scene_id)
            return True

    def wait(self, timeout: float | None = None) -> BakeOutcome | None:
        with self._lock:
            future = self._future
        if future is None:
            return None
        try:
            return future.result(timeout=timeout)
        except BakeCancelled:
            return None

    def wait_until_idle(self, timeout: float | None = None) -> None:
        with self._lock:
            future = self._future
        if future is not None:
            try:
                future.result(timeout=timeout)
            except (BakeCancelled, ProbeError):
                pass

    @property
    def last_outcome(self) -> BakeOutcome | None:
        with self._lock:
            return self._last_outcome

    def seed_committed_state(self, state: BakeState, snapshot: IrradianceSnapshot) -> None:
        with self._lock:
            if self._active_locked():
                raise ProbeError(ErrorCode.BUSY, "cannot seed a state while a bake is active")
            self._last_state = state
            self._last_snapshot = snapshot

    def status(self) -> JobStatus:
        with self._lock:
            return JobStatus(
                scene_id=self.scene_id,
                running=self._active_locked(),
                cancelled=self._cancelled,
                published_version=(
                    None if self._last_snapshot is None else self._last_snapshot.version
                ),
                last_content_hash=(
                    None if self._last_snapshot is None else self._last_snapshot.content_hash
                ),
                error=None if self._error is None else self._error.to_dict(),
            )

    def shutdown(self, wait: bool = True) -> None:
        self.cancel()
        self._executor.shutdown(wait=wait, cancel_futures=True)

