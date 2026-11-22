from __future__ import annotations

import queue
import threading
from dataclasses import dataclass
from typing import Any, Callable

from .engine import MatchEngine
from .errors import E_RESOURCE, KernelError
from .models import AuthoritativeState, MatchContext
from .protocol import decode_frame, encode_frame


@dataclass(frozen=True)
class ConcurrencyLimits:
    max_workers: int = 4
    max_queue: int = 16
    max_buffer_bytes: int = 8 * 1024 * 1024


class TaskHandle:
    def __init__(self, name: str) -> None:
        self.name = name
        self._done = threading.Event()
        self._result: Any = None
        self._error: BaseException | None = None

    def _complete(self, result: Any) -> None:
        self._result = result
        self._done.set()

    def _fail(self, error: BaseException) -> None:
        self._error = error
        self._done.set()

    def result(self, timeout: float | None = None) -> Any:
        if not self._done.wait(timeout):
            raise TimeoutError(f"task {self.name!r} did not complete")
        if self._error is not None:
            raise self._error
        return self._result


class BoundedExecutor:
    """A fixed-size worker pool with queue and reserved-buffer limits."""

    _SENTINEL = object()

    def __init__(self, limits: ConcurrencyLimits) -> None:
        if limits.max_workers <= 0 or limits.max_queue <= 0 or limits.max_buffer_bytes <= 0:
            raise KernelError(
                E_RESOURCE,
                "concurrency limits must all be positive",
                path="$.limits",
            )
        self.limits = limits
        self._queue: queue.Queue[Any] = queue.Queue(maxsize=limits.max_queue)
        self._lock = threading.Lock()
        self._reserved_bytes = 0
        self._pending = 0
        self._closed = False
        self.max_observed_pending = 0
        self.max_observed_buffer = 0
        self.completed = 0
        self._threads = [
            threading.Thread(target=self._worker, name=f"sync-worker-{index}", daemon=True)
            for index in range(limits.max_workers)
        ]
        for thread in self._threads:
            thread.start()

    def submit(
        self,
        function: Callable[..., Any],
        *args: Any,
        name: str = "task",
        estimated_bytes: int = 0,
        **kwargs: Any,
    ) -> TaskHandle:
        if estimated_bytes < 0:
            raise KernelError(E_RESOURCE, "estimated_bytes must be non-negative", path="$")
        handle = TaskHandle(name)
        with self._lock:
            if self._closed:
                raise KernelError(E_RESOURCE, "executor is closed", path="$")
            if self._pending >= self.limits.max_queue:
                raise KernelError(
                    E_RESOURCE,
                    "task queue limit exceeded",
                    path="$",
                    details={
                        "pending": self._pending,
                        "max_queue": self.limits.max_queue,
                    },
                )
            if self._reserved_bytes + estimated_bytes > self.limits.max_buffer_bytes:
                raise KernelError(
                    E_RESOURCE,
                    "reserved memory buffer limit exceeded",
                    path="$",
                    details={
                        "reserved": self._reserved_bytes,
                        "requested": estimated_bytes,
                        "limit": self.limits.max_buffer_bytes,
                    },
                )
            self._reserved_bytes += estimated_bytes
            self._pending += 1
            self.max_observed_pending = max(self.max_observed_pending, self._pending)
            self.max_observed_buffer = max(
                self.max_observed_buffer, self._reserved_bytes
            )
        try:
            self._queue.put_nowait(
                (handle, function, args, kwargs, estimated_bytes)
            )
        except queue.Full:
            with self._lock:
                self._reserved_bytes = max(0, self._reserved_bytes - estimated_bytes)
                self._pending = max(0, self._pending - 1)
            raise KernelError(
                E_RESOURCE,
                "task queue is full",
                path="$",
                details={"max_queue": self.limits.max_queue},
            )
        return handle

    def _worker(self) -> None:
        while True:
            item = self._queue.get()
            if item is self._SENTINEL:
                self._queue.task_done()
                return
            handle, function, args, kwargs, estimated_bytes = item
            try:
                handle._complete(function(*args, **kwargs))
            except BaseException as exc:
                handle._fail(exc)
            finally:
                with self._lock:
                    self._reserved_bytes = max(0, self._reserved_bytes - estimated_bytes)
                    self._pending = max(0, self._pending - 1)
                    self.completed += 1
                self._queue.task_done()

    def close(self, wait: bool = True) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
        for _ in self._threads:
            self._queue.put(self._SENTINEL)
        if wait:
            for thread in self._threads:
                thread.join(timeout=5.0)

    def __enter__(self) -> "BoundedExecutor":
        return self

    def __exit__(self, *_args: Any) -> None:
        self.close()

    def stats(self) -> dict[str, Any]:
        with self._lock:
            return {
                "max_workers": self.limits.max_workers,
                "max_queue": self.limits.max_queue,
                "max_buffer_bytes": self.limits.max_buffer_bytes,
                "max_observed_pending": self.max_observed_pending,
                "max_observed_buffer_bytes": self.max_observed_buffer,
                "completed": self.completed,
            }


@dataclass
class VersionWorkResult:
    version: int
    final_hash: str
    final_tick: int
    encoded_frames: int
    decoded_frames: int
    warning_count: int
    first_divergence: dict[str, Any] | None
    frame_bytes_hashes: list[str]


def _version_worker(
    version: int,
    context: MatchContext,
    initial_state: AuthoritativeState,
    inputs_by_tick: dict[int, list[dict[str, Any]]],
    target_tick: int,
    expected_hashes: dict[int, str],
) -> VersionWorkResult:
    engine = MatchEngine(context, initial_state, inputs_by_tick)
    work_states, _ = engine.run_to(target_tick)
    base: AuthoritativeState | None = None
    warning_count = 0
    byte_hashes: list[str] = []
    decoded_frames = 0
    first_divergence: dict[str, Any] | None = None
    for state in work_states:
        previous = work_states[state.tick - 1] if state.tick > 0 else None
        encoded = encode_frame(state, version, previous=previous, context=context)
        warning_count += len(encoded.warnings)
        import hashlib

        byte_hashes.append(hashlib.sha256(encoded.payload).hexdigest())
        decoded = decode_frame(encoded.payload, base=base, context=context)
        decoded_frames += 1
        if first_divergence is None and decoded.state.snapshot_hash != expected_hashes.get(
            state.tick
        ):
            first_divergence = {
                "tick": state.tick,
                "field": "snapshot_hash",
                "decoded": decoded.state.snapshot_hash,
                "expected": expected_hashes.get(state.tick),
            }
        base = decoded.state
    assert base is not None
    return VersionWorkResult(
        version=version,
        final_hash=base.snapshot_hash,
        final_tick=base.tick,
        encoded_frames=len(work_states),
        decoded_frames=decoded_frames,
        warning_count=warning_count,
        first_divergence=first_divergence,
        frame_bytes_hashes=byte_hashes,
    )


def run_parallel_version_validation(
    *,
    context: MatchContext,
    initial_state: AuthoritativeState,
    inputs_by_tick: dict[int, list[dict[str, Any]]],
    expected_states: list[AuthoritativeState],
    versions: list[int],
    limits: ConcurrencyLimits,
) -> tuple[dict[int, VersionWorkResult], dict[str, Any]]:
    expected_hashes = {state.tick: state.snapshot_hash for state in expected_states}
    target_tick = expected_states[-1].tick
    with BoundedExecutor(limits) as executor:
        handles = {
            version: executor.submit(
                _version_worker,
                version,
                context,
                initial_state,
                inputs_by_tick,
                target_tick,
                expected_hashes,
                name=f"version-{version}",
                estimated_bytes=512 * 1024,
            )
            for version in versions
        }
        results = {
            version: handles[version].result(timeout=30.0)
            for version in sorted(handles)
        }
        stats = executor.stats()
    return results, stats

