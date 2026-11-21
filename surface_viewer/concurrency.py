from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from types import MappingProxyType
from typing import Callable, Mapping, TypeVar

from .artifacts import Artifact
from .errors import ErrorCode, SurfaceViewerError
from .graph import DependencyGraph
from .model import SurfaceAssembly
from .preview import ScreenPreview

T = TypeVar("T")


@dataclass(frozen=True)
class Snapshot:
    generation: int
    commit_seq: int
    assembly: SurfaceAssembly
    graph: DependencyGraph
    artifacts: Mapping[str, Artifact]
    screen_preview: ScreenPreview
    chain_head: str

    def artifact(self, artifact_id: str) -> Artifact:
        try:
            return self.artifacts[artifact_id]
        except KeyError:
            raise SurfaceViewerError(
                ErrorCode.NOT_FOUND,
                "artifact is not present in the snapshot",
                field=artifact_id,
            )


@dataclass(frozen=True)
class SnapshotHandle:
    generation: int
    snapshot: Snapshot
    store: "SnapshotStore"

    def read_artifact(self, artifact_id: str) -> Artifact:
        return self.store.read(self, artifact_id)

    def read_screen(self) -> ScreenPreview:
        return self.store.read_screen(self)


class SnapshotStore:
    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._current: Snapshot | None = None
        self._next_generation = 1

    def publish(
        self,
        *,
        commit_seq: int,
        assembly: SurfaceAssembly,
        graph: DependencyGraph,
        artifacts: Mapping[str, Artifact],
        screen_preview: ScreenPreview,
        chain_head: str,
    ) -> SnapshotHandle:
        with self._lock:
            snapshot = Snapshot(
                generation=self._next_generation,
                commit_seq=commit_seq,
                assembly=assembly,
                graph=graph,
                artifacts=MappingProxyType(dict(artifacts)),
                screen_preview=screen_preview,
                chain_head=chain_head,
            )
            self._next_generation += 1
            self._current = snapshot
            return SnapshotHandle(
                generation=snapshot.generation,
                snapshot=snapshot,
                store=self,
            )

    def current(self) -> SnapshotHandle:
        with self._lock:
            if self._current is None:
                raise SurfaceViewerError(
                    ErrorCode.NOT_FOUND, "no product snapshot has been published"
                )
            return SnapshotHandle(
                generation=self._current.generation,
                snapshot=self._current,
                store=self,
            )

    def _validate_handle(self, handle: SnapshotHandle) -> None:
        with self._lock:
            current = self._current
            if (
                current is None
                or handle.generation != current.generation
                or handle.snapshot is not current
            ):
                raise SurfaceViewerError(
                    ErrorCode.STALE_HANDLE,
                    "snapshot handle has been superseded",
                    field="snapshot_handle",
                    details={
                        "handle_generation": handle.generation,
                        "current_generation": (
                            None if current is None else current.generation
                        ),
                    },
                )

    def read(self, handle: SnapshotHandle, artifact_id: str) -> Artifact:
        self._validate_handle(handle)
        return handle.snapshot.artifact(artifact_id)

    def read_screen(self, handle: SnapshotHandle) -> ScreenPreview:
        self._validate_handle(handle)
        return handle.snapshot.screen_preview


@dataclass
class RecomputeTask:
    assembly_id: str
    _thread: threading.Thread
    _done: threading.Event
    _result: object | None = None
    _error: BaseException | None = None

    @property
    def done(self) -> bool:
        return self._done.is_set()

    def result(self, timeout: float | None = None) -> object:
        if not self._done.wait(timeout):
            raise TimeoutError("recompute task did not finish in time")
        if self._error is not None:
            raise self._error
        return self._result


class RecomputeCoordinator:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._busy: set[str] = set()

    def submit(
        self, assembly_id: str, operation: Callable[[], T]
    ) -> RecomputeTask:
        with self._lock:
            if assembly_id in self._busy:
                raise SurfaceViewerError(
                    ErrorCode.BUSY,
                    "a recompute task is already running for this assembly",
                    field=assembly_id,
                )
            self._busy.add(assembly_id)

        done = threading.Event()
        task = RecomputeTask(
            assembly_id=assembly_id,
            _thread=threading.Thread(
                target=lambda: None, name=f"surface-recompute-{assembly_id}"
            ),
            _done=done,
        )

        def run() -> None:
            try:
                task._result = operation()
            except BaseException as exc:
                task._error = exc
            finally:
                with self._lock:
                    self._busy.discard(assembly_id)
                done.set()

        thread = threading.Thread(
            target=run,
            name=f"surface-recompute-{assembly_id}",
            daemon=True,
        )
        task._thread = thread
        thread.start()
        return task

    def is_busy(self, assembly_id: str) -> bool:
        with self._lock:
            return assembly_id in self._busy


@dataclass(frozen=True)
class RenderLoopStats:
    frames: int
    stale_handles: int
    errors: tuple[str, ...]


def render_fixed_fps(
    store: SnapshotStore,
    draw: Callable[[SnapshotHandle], None],
    *,
    fps: float = 60.0,
    stop_event: threading.Event,
    max_frames: int | None = None,
) -> RenderLoopStats:
    if fps <= 0:
        raise ValueError("fps must be positive")
    interval = 1.0 / fps
    next_deadline = time.monotonic()
    frames = 0
    stale = 0
    errors: list[str] = []
    while not stop_event.is_set():
        try:
            handle = store.current()
            draw(handle)
        except SurfaceViewerError as exc:
            if exc.code is ErrorCode.STALE_HANDLE:
                stale += 1
            else:
                errors.append(exc.code.value)
        except Exception as exc:
            errors.append(type(exc).__name__)
        frames += 1
        if max_frames is not None and frames >= max_frames:
            break
        next_deadline += interval
        delay = next_deadline - time.monotonic()
        if delay > 0:
            stop_event.wait(delay)
        else:
            next_deadline = time.monotonic()
    return RenderLoopStats(
        frames=frames, stale_handles=stale, errors=tuple(errors)
    )
