"""Atomic scene publication, immutable snapshot handles and edit exclusion."""

from __future__ import annotations

import json
import os
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Sequence

from .camera import Camera
from .errors import ErrorCode, TacticalError
from .projection import ProjectionConfig
from .query import BudgetConfig, HoverConfig, HoverResult, QueryEngine, SelectionResult
from .scene import Scene, SceneUnit, load_scene

MANIFEST_FORMAT = "tactical-scene-manifest-v1"


def _atomic_json_write(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{threading.get_ident()}.tmp")
    with temporary.open("wb") as handle:
        handle.write(encoded)
        handle.write(b"\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def read_manifest(path: str | os.PathLike[str]) -> dict[str, Any]:
    manifest_path = Path(path)
    try:
        payload = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene manifest cannot be read",
            {"path": str(manifest_path), "reason": str(exc)},
        ) from exc
    if (
        not isinstance(payload, dict)
        or payload.get("format") != MANIFEST_FORMAT
        or not isinstance(payload.get("entries"), list)
    ):
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene manifest format is invalid",
            {"path": str(manifest_path)},
        )
    for entry in payload["entries"]:
        if not isinstance(entry, dict):
            raise TacticalError(ErrorCode.SCENE_STALE, "scene manifest entry is invalid")
        if not isinstance(entry.get("version"), int) or not isinstance(entry.get("content_hash"), str):
            raise TacticalError(ErrorCode.SCENE_STALE, "scene manifest entry is incomplete")
    return payload


def upsert_manifest_entry(
    manifest_path: str | os.PathLike[str],
    scene: Scene,
    *,
    scene_file: str | None = None,
) -> dict[str, Any]:
    path = Path(manifest_path)
    if path.exists():
        payload = read_manifest(path)
        entries = payload["entries"]
    else:
        payload = {"format": MANIFEST_FORMAT, "entries": []}
        entries = payload["entries"]
    replacement = {
        "version": scene.version,
        "content_hash": scene.content_hash,
        "unit_count": scene.unit_count,
        "scene_file": scene_file,
    }
    updated = [entry for entry in entries if int(entry["version"]) != scene.version]
    updated.append(replacement)
    updated.sort(key=lambda entry: int(entry["version"]))
    payload["entries"] = updated
    payload["latest"] = updated[-1]["version"]
    _atomic_json_write(path, payload)
    return payload


def verify_manifest(
    scene: Scene,
    manifest_path: str | os.PathLike[str],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> dict[str, Any]:
    payload = read_manifest(manifest_path)
    entry = next(
        (item for item in payload["entries"] if int(item["version"]) == scene.version),
        None,
    )
    if entry is None:
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene version is absent from the publication manifest",
            {"version": scene.version, "manifest": str(manifest_path)},
        )
    if entry["content_hash"] != scene.content_hash:
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene hash does not match the publication manifest",
            {
                "version": scene.version,
                "expected_hash": entry["content_hash"],
                "actual_hash": scene.content_hash,
            },
        )
    if expected_version is not None and int(expected_version) != scene.version:
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene version does not match the requested version",
            {"expected_version": int(expected_version), "actual_version": scene.version},
        )
    if expected_hash is not None and expected_hash != scene.content_hash:
        raise TacticalError(
            ErrorCode.SCENE_STALE,
            "scene hash does not match the requested hash",
            {"expected_hash": expected_hash, "actual_hash": scene.content_hash},
        )
    return entry


def load_published_scene(
    scene_path: str | os.PathLike[str],
    manifest_path: str | os.PathLike[str],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> Scene:
    scene = load_scene(scene_path)
    verify_manifest(
        scene,
        manifest_path,
        expected_version=expected_version,
        expected_hash=expected_hash,
    )
    return scene


@dataclass(eq=False, slots=True)
class SnapshotHandle:
    handle_id: int
    scene: Scene
    retired: bool = False

    @property
    def version(self) -> int:
        return self.scene.version

    @property
    def content_hash(self) -> str:
        return self.scene.content_hash


class SceneRepository:
    """One mutable publication pointer plus immutable version history."""

    def __init__(
        self,
        scene: Scene,
        *,
        manifest_path: str | os.PathLike[str] | None = None,
        scene_file: str | None = None,
    ) -> None:
        self._lock = threading.RLock()
        self._current = scene
        self._handles: dict[int, SnapshotHandle] = {}
        self._next_handle_id = 1
        self._history: dict[tuple[int, str], Scene] = {
            (scene.version, scene.content_hash): scene
        }
        self.manifest_path = Path(manifest_path) if manifest_path is not None else None
        self.scene_file = scene_file
        self._max_publish_block_ns = 0
        if self.manifest_path is not None:
            if self.manifest_path.exists():
                verify_manifest(scene, self.manifest_path)
            else:
                upsert_manifest_entry(self.manifest_path, scene, scene_file=scene_file)

    @property
    def current_scene(self) -> Scene:
        with self._lock:
            return self._current

    @property
    def max_publish_block_ms(self) -> float:
        with self._lock:
            return self._max_publish_block_ns / 1_000_000.0

    def acquire_handle(self) -> SnapshotHandle:
        with self._lock:
            handle = SnapshotHandle(self._next_handle_id, self._current)
            self._next_handle_id += 1
            self._handles[handle.handle_id] = handle
            return handle

    def resolve_handle(self, handle: SnapshotHandle) -> Scene:
        with self._lock:
            known = self._handles.get(handle.handle_id)
            if known is not handle or handle.retired:
                raise TacticalError(
                    ErrorCode.STALE_HANDLE,
                    "snapshot handle has been retired by a newer publication",
                    {
                        "handle_id": handle.handle_id,
                        "handle_version": handle.scene.version,
                        "current_version": self._current.version,
                    },
                )
            return handle.scene

    def history_snapshot(self, version: int, content_hash: str | None = None) -> Scene:
        with self._lock:
            if content_hash is not None:
                scene = self._history.get((int(version), content_hash))
                if scene is not None:
                    return scene
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "requested historical scene snapshot is unavailable",
                    {"version": int(version), "content_hash": content_hash},
                )
            matches = [
                scene for (stored_version, _hash), scene in self._history.items()
                if stored_version == int(version)
            ]
            if len(matches) == 1:
                return matches[0]
            if not matches:
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "requested historical scene version is unavailable",
                    {"version": int(version)},
                )
            raise TacticalError(
                ErrorCode.SCENE_STALE,
                "requested historical scene version is ambiguous",
                {"version": int(version), "hash_count": len(matches)},
            )

    def publish(self, scene: Scene, *, expected_version: int | None = None) -> Scene:
        publish_started = time.perf_counter_ns()
        with self._lock:
            current = self._current
            if expected_version is not None and int(expected_version) != current.version:
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "publish expected version no longer matches the repository",
                    {
                        "expected_version": int(expected_version),
                        "current_version": current.version,
                    },
                )
            if scene.version <= current.version:
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "published scene version must increase monotonically",
                    {"current_version": current.version, "new_version": scene.version},
                )
            existing = self._history.get((scene.version, scene.content_hash))
            if existing is not None and existing is not scene:
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "scene version/hash pair has already been used",
                    {"version": scene.version, "content_hash": scene.content_hash},
                )
            conflicting_version = [
                key for key in self._history if key[0] == scene.version and key[1] != scene.content_hash
            ]
            if conflicting_version:
                raise TacticalError(
                    ErrorCode.SCENE_STALE,
                    "scene version already refers to a different content hash",
                    {
                        "version": scene.version,
                        "existing_hash": conflicting_version[0][1],
                        "new_hash": scene.content_hash,
                    },
                )
            if self.manifest_path is not None:
                upsert_manifest_entry(self.manifest_path, scene, scene_file=self.scene_file)

            previous = self._current
            for handle in self._handles.values():
                if handle.scene is previous and not handle.retired:
                    handle.retired = True
            self._current = scene
            self._history[(scene.version, scene.content_hash)] = scene
            elapsed_ns = time.perf_counter_ns() - publish_started
            self._max_publish_block_ns = max(self._max_publish_block_ns, elapsed_ns)
            return scene


class SceneEngine:
    """Convenience facade combining publication, queries and background edits."""

    def __init__(
        self,
        scene: Scene,
        *,
        manifest_path: str | os.PathLike[str] | None = None,
        scene_file: str | None = None,
        projection_config: ProjectionConfig | None = None,
        budget_config: BudgetConfig | None = None,
        hover_config: HoverConfig | None = None,
    ) -> None:
        self.repository = SceneRepository(
            scene,
            manifest_path=manifest_path,
            scene_file=scene_file,
        )
        self.projection_config = projection_config or ProjectionConfig()
        self.budget_config = budget_config or BudgetConfig()
        self.hover_config = hover_config or HoverConfig()
        self._edit_lock = threading.Lock()
        self._query_cache_lock = threading.Lock()
        self._query_cache: dict[tuple[Any, ...], QueryEngine] = {}
        self._executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="scene-editor")
        self._closed = False

    def shutdown(self, *, wait: bool = True) -> None:
        if not self._closed:
            self._closed = True
            self._executor.shutdown(wait=wait, cancel_futures=False)

    def __enter__(self) -> "SceneEngine":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.shutdown(wait=True)

    def acquire_handle(self) -> SnapshotHandle:
        return self.repository.acquire_handle()

    def query_engine_for_handle(
        self,
        handle: SnapshotHandle,
        camera: Camera,
    ) -> QueryEngine:
        scene = self.repository.resolve_handle(handle)
        key = (
            scene.version,
            scene.content_hash,
            camera.key(),
            self.projection_config,
            self.budget_config,
            self.hover_config,
        )
        with self._query_cache_lock:
            cached = self._query_cache.get(key)
            if cached is not None:
                return cached
        engine = QueryEngine(
            scene,
            camera,
            projection_config=self.projection_config,
            budget_config=self.budget_config,
            hover_config=self.hover_config,
        )
        with self._query_cache_lock:
            self._query_cache.setdefault(key, engine)
            while len(self._query_cache) > 8:
                oldest_key = next(iter(self._query_cache))
                if oldest_key == key:
                    break
                self._query_cache.pop(oldest_key, None)
        return engine

    def select_rect(
        self,
        camera: Camera,
        x0: float,
        y0: float,
        x1: float,
        y1: float,
        *,
        handle: SnapshotHandle | None = None,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> SelectionResult:
        active_handle = handle or self.repository.acquire_handle()
        engine = self.query_engine_for_handle(active_handle, camera)
        scene_version = engine.scene.version if expected_version is None else expected_version
        scene_hash = engine.scene.content_hash if expected_hash is None else expected_hash
        return engine.select_rect(
            x0,
            y0,
            x1,
            y1,
            expected_version=scene_version,
            expected_hash=scene_hash,
        )

    def hover(
        self,
        camera: Camera,
        x: float,
        y: float,
        *,
        target_unit_id: int | None = None,
        handle: SnapshotHandle | None = None,
        expected_version: int | None = None,
        expected_hash: str | None = None,
    ) -> HoverResult:
        active_handle = handle or self.repository.acquire_handle()
        engine = self.query_engine_for_handle(active_handle, camera)
        scene_version = engine.scene.version if expected_version is None else expected_version
        scene_hash = engine.scene.content_hash if expected_hash is None else expected_hash
        return engine.hover(
            x,
            y,
            target_unit_id=target_unit_id,
            expected_version=scene_version,
            expected_hash=scene_hash,
        )

    def publish(self, scene: Scene, *, expected_version: int | None = None) -> Scene:
        return self.repository.publish(scene, expected_version=expected_version)

    def submit_edit(
        self,
        mutator: Callable[[Scene], Scene | Sequence[SceneUnit]],
        *,
        expected_version: int | None = None,
    ) -> Future[Scene]:
        if self._closed:
            raise TacticalError(ErrorCode.BUSY, "scene engine is closed")
        if not self._edit_lock.acquire(blocking=False):
            raise TacticalError(
                ErrorCode.BUSY,
                "another edit task is already running for this scene",
            )

        def run() -> Scene:
            try:
                base = self.repository.current_scene
                if expected_version is not None and base.version != int(expected_version):
                    raise TacticalError(
                        ErrorCode.SCENE_STALE,
                        "edit expected version no longer matches the current scene",
                        {
                            "expected_version": int(expected_version),
                            "current_version": base.version,
                        },
                    )
                changed = mutator(base)
                if isinstance(changed, Scene):
                    candidate = changed
                    if candidate.version <= base.version:
                        candidate = Scene(
                            version=base.version + 1,
                            units=candidate.units,
                            source=base.source,
                        )
                else:
                    candidate = base.with_updates(tuple(changed))
                return self.repository.publish(candidate, expected_version=base.version)
            finally:
                self._edit_lock.release()

        try:
            return self._executor.submit(run)
        except Exception:
            self._edit_lock.release()
            raise
