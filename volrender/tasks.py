"""Single-render-per-volume concurrency with cooperative cancellation."""

from __future__ import annotations

import os
import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from .buffers import BufferStore
from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .renderer import RenderCancelled, RenderResult, render_offscreen
from .snapshot import create_snapshot, save_snapshot
from .volume import VolumeData


class CancellationFlag:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    def is_set(self) -> bool:
        return self._event.is_set()

    @property
    def cancelled(self) -> bool:
        return self.is_set()


@dataclass
class RenderJob:
    volume_key: str
    params: RenderParameters
    future: Future[RenderResult]
    cancellation: CancellationFlag
    started: threading.Event

    @property
    def parameter_hash(self) -> str:
        return self.params.parameter_hash

    def cancel(self) -> None:
        self.cancellation.cancel()


RenderCallable = Callable[..., RenderResult]


class OffscreenRenderService:
    """Owns a bounded executor and a per-volume single-flight gate."""

    def __init__(
        self,
        *,
        renderer: RenderCallable = render_offscreen,
        max_workers: int = 2,
        buffer_store: BufferStore | None = None,
        batch_size: int = 8,
    ) -> None:
        self.renderer = renderer
        self.buffer_store = buffer_store
        self.batch_size = batch_size
        self._executor = ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="volrender")
        self._lock = threading.Lock()
        self._active: dict[str, RenderJob] = {}
        self._closed = False

    def submit(
        self,
        volume_key: str,
        volume: VolumeData,
        params: RenderParameters,
        *,
        snapshot_path: str | os.PathLike[str] | None = None,
        commit_snapshot: bool = True,
    ) -> RenderJob:
        if not volume_key:
            raise VolumeRenderError(ErrorCode.E_BUSY, "volume_key must not be empty", field="volume_key")
        with self._lock:
            if self._closed:
                raise VolumeRenderError(ErrorCode.E_BUSY, "render service is closed", field="service")
            if volume_key in self._active:
                raise VolumeRenderError(
                    ErrorCode.E_BUSY,
                    "an offscreen render is already active for this volume",
                    field="volume_key",
                    details={"volume_key": volume_key},
                )

            cancellation = CancellationFlag()
            started = threading.Event()
            placeholder: Future[RenderResult] = Future()
            job = RenderJob(volume_key, params, placeholder, cancellation, started)
            self._active[volume_key] = job

        try:
            future = self._executor.submit(
                self._run,
                job,
                volume,
                params,
                Path(snapshot_path) if snapshot_path is not None else None,
                bool(commit_snapshot),
            )
        except BaseException:
            with self._lock:
                self._active.pop(volume_key, None)
            raise
        job.future = future
        future.add_done_callback(lambda _future: self._release(volume_key, job))
        return job

    def _release(self, volume_key: str, job: RenderJob) -> None:
        with self._lock:
            if self._active.get(volume_key) is job:
                self._active.pop(volume_key, None)

    def _run(
        self,
        job: RenderJob,
        volume: VolumeData,
        params: RenderParameters,
        snapshot_path: Path | None,
        commit_snapshot: bool,
    ) -> RenderResult:
        job.started.set()
        try:
            if job.cancellation.is_set():
                raise RenderCancelled("render cancelled before work started")
            result = self.renderer(
                volume,
                params,
                cancel_token=job.cancellation,
                batch_size=self.batch_size,
            )
            if job.cancellation.is_set():
                raise RenderCancelled("render cancelled before commit")
            result.assert_bound_to(params)
            if self.buffer_store is not None:
                stored = self.buffer_store.publish(result, params)
                if commit_snapshot and snapshot_path is not None:
                    coverage_hash = str(stored.manifest.get("mask_coverage_hash", ""))
                    snapshot = create_snapshot(
                        params,
                        result,
                        color_buffer_name=params.parameter_hash,
                        mask_coverage_hash=coverage_hash,
                    )
                    save_snapshot(snapshot, snapshot_path)
            return result
        except (RenderCancelled, Exception):
            # No output is committed here.  A prior on-disk snapshot remains the
            # only valid recovery source after cancellation or failure.
            raise

    def active_volume_keys(self) -> tuple[str, ...]:
        with self._lock:
            return tuple(sorted(self._active))

    def cancel(self, volume_key: str) -> bool:
        with self._lock:
            job = self._active.get(volume_key)
        if job is None:
            return False
        job.cancel()
        return True

    def shutdown(self, wait: bool = True) -> None:
        with self._lock:
            self._closed = True
        self._executor.shutdown(wait=wait, cancel_futures=False)


def wait_for_job(job: RenderJob, timeout: float | None = None) -> RenderResult:
    return job.future.result(timeout=timeout)
