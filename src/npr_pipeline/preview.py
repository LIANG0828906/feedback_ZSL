from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Any

from .errors import E_BUSY, E_STALE_HANDLE, PipelineError, fail
from .pipeline import RenderPipeline, RenderResult
from .storage import Store


@dataclass(frozen=True)
class PreviewBatch:
    layer: int
    batch: str
    chunk: str
    zone_id: str
    content_hash: str
    factor_versions: dict[str, str]
    strokes: tuple[dict[str, Any], ...]
    bbox: tuple[float, float, float, float]


@dataclass(frozen=True)
class PreviewSnapshot:
    version: int
    content_hash: str
    manifest_hash: str
    scene_id: str
    batches: tuple[PreviewBatch, ...]
    layer_stats: dict[str, Any] = field(default_factory=dict)

    def batches_for_layer(self, layer: int) -> tuple[PreviewBatch, ...]:
        return tuple(item for item in self.batches if item.layer == layer)


class PreviewService:
    """Single-source, version-atomic preview publication."""

    def __init__(self, store: Store):
        self.store = store
        self._lock = threading.RLock()
        self._current: PreviewSnapshot | None = None

    def _snapshot_from_manifest(self, manifest: dict[str, Any]) -> PreviewSnapshot:
        batches: list[PreviewBatch] = []
        for record in sorted(
            manifest.get("batches", []),
            key=lambda item: (int(item["layer"]), str(item["batch"])),
        ):
            document = self.store.load_chunk_document(record)
            batches.append(
                PreviewBatch(
                    layer=int(record["layer"]),
                    batch=str(record["batch"]),
                    chunk=str(record["chunk"]),
                    zone_id=str(record["zone_id"]),
                    content_hash=str(record["hash"]),
                    factor_versions=dict(record["factor_versions"]),
                    strokes=tuple(document.get("strokes", [])),
                    bbox=tuple(float(value) for value in record.get("bbox", (0, 0, 0, 0))),
                )
            )
        return PreviewSnapshot(
            version=int(manifest["version"]),
            content_hash=str(manifest["content_hash"]),
            manifest_hash=str(manifest["manifest_hash"]),
            scene_id=str(manifest["scene_id"]),
            batches=tuple(batches),
            layer_stats=dict(manifest.get("layer_stats", {})),
        )

    def refresh(self, *, attempts: int = 3) -> PreviewSnapshot | None:
        for _attempt in range(attempts):
            pointer_before = self.store.read_current_pointer()
            if pointer_before is None:
                with self._lock:
                    self._current = None
                return None
            manifest = self.store.current_manifest()
            if manifest is None:
                return None
            candidate = self._snapshot_from_manifest(manifest)
            pointer_after = self.store.read_current_pointer()
            if (
                pointer_after is not None
                and pointer_after.get("content_hash") == candidate.content_hash
                and pointer_after.get("manifest_hash") == candidate.manifest_hash
            ):
                with self._lock:
                    if (
                        self._current is None
                        or candidate.version > self._current.version
                        or (
                            candidate.version == self._current.version
                            and candidate.content_hash == self._current.content_hash
                        )
                    ):
                        self._current = candidate
                    return self._current
        return self.current()

    def publish_from_store(self) -> PreviewSnapshot | None:
        return self.refresh()

    def current(self) -> PreviewSnapshot | None:
        with self._lock:
            return self._current

    def resolve_handle(self, snapshot: PreviewSnapshot) -> PreviewSnapshot:
        with self._lock:
            current = self._current
        if current is None or (
            snapshot.version != current.version
            or snapshot.content_hash != current.content_hash
            or snapshot.manifest_hash != current.manifest_hash
        ):
            fail(
                E_STALE_HANDLE,
                f"preview:v{snapshot.version}",
                "snapshot_superseded",
                current_version=None if current is None else current.version,
            )
        return current

    def same_version(self, left: PreviewSnapshot | None, right: PreviewSnapshot | None) -> bool:
        if left is None or right is None:
            return left is right
        return (
            left.version == right.version
            and left.content_hash == right.content_hash
            and left.manifest_hash == right.manifest_hash
        )


@dataclass
class RenderTask:
    scene_id: str
    cancel_event: threading.Event
    done: threading.Event = field(default_factory=threading.Event)
    thread: threading.Thread | None = None
    result: RenderResult | None = None
    error: PipelineError | None = None
    preview: PreviewSnapshot | None = None

    def wait(self, timeout: float | None = None) -> bool:
        return self.done.wait(timeout)


class RenderService:
    def __init__(self, preview: PreviewService):
        self.preview = preview
        self._lock = threading.Lock()
        self._active: dict[str, RenderTask] = {}

    def submit(
        self,
        scene: Any,
        params: Any,
        store: Store,
        *,
        resume: bool = True,
        before_batch: Any | None = None,
        fail_after_batches: int | None = None,
        crash_after_batches: int | None = None,
    ) -> RenderTask:
        scene_id = scene.scene_id
        with self._lock:
            existing = self._active.get(scene_id)
            if existing is not None and not existing.done.is_set():
                fail(E_BUSY, f"scene:{scene_id}", "render_already_running")
            task = RenderTask(scene_id=scene_id, cancel_event=threading.Event())
            self._active[scene_id] = task

        def worker() -> None:
            try:
                pipeline = RenderPipeline(store)
                task.result = pipeline.render(
                    scene,
                    params,
                    resume=resume,
                    cancel_event=task.cancel_event,
                    before_batch=before_batch,
                    fail_after_batches=fail_after_batches,
                    crash_after_batches=crash_after_batches,
                )
                if task.result.committed:
                    task.preview = self.preview.publish_from_store()
            except PipelineError as exc:
                task.error = exc
            finally:
                with self._lock:
                    if self._active.get(scene_id) is task:
                        del self._active[scene_id]
                task.done.set()

        task.thread = threading.Thread(
            target=worker,
            name=f"npr-render-{scene_id[:12]}",
            daemon=True,
        )
        task.thread.start()
        return task

    def cancel(self, scene_id: str) -> bool:
        with self._lock:
            task = self._active.get(scene_id)
        if task is None or task.done.is_set():
            return False
        task.cancel_event.set()
        return True

    def is_busy(self, scene_id: str) -> bool:
        with self._lock:
            task = self._active.get(scene_id)
            return task is not None and not task.done.is_set()


__all__ = [
    "PreviewBatch",
    "PreviewService",
    "PreviewSnapshot",
    "RenderService",
    "RenderTask",
]
