from __future__ import annotations

import threading
import weakref
from concurrent.futures import Future
from pathlib import Path

from .camera import Camera
from .cancel import CancellationToken
from .errors import E_BUSY, E_SNAPSHOT_INVALID, TerrainError
from .render import RenderConfig, RenderResult, Renderer
from .snapshot import Snapshot, load_snapshot, save_snapshot
from .terrain import Terrain


_TERRAIN_LOCKS: weakref.WeakKeyDictionary[Terrain, threading.Lock] = (
    weakref.WeakKeyDictionary()
)
_TERRAIN_LOCKS_GUARD = threading.Lock()


def _terrain_lock(terrain: Terrain) -> threading.Lock:
    with _TERRAIN_LOCKS_GUARD:
        lock = _TERRAIN_LOCKS.get(terrain)
        if lock is None:
            lock = threading.Lock()
            _TERRAIN_LOCKS[terrain] = lock
        return lock


class RenderService:
    """Single-render admission control for one terrain instance."""

    def __init__(self, terrain: Terrain, renderer: Renderer | None = None) -> None:
        self.terrain = terrain
        self.renderer = renderer or Renderer()
        self._lock = _terrain_lock(terrain)
        self._worker_thread: threading.Thread | None = None
        self.last_result: RenderResult | None = None
        self.last_snapshot: Snapshot | None = None

    @property
    def active(self) -> bool:
        return self._lock.locked()

    def _acquire(self) -> None:
        if not self._lock.acquire(blocking=False):
            raise TerrainError(
                E_BUSY, "a render task is already active for this terrain"
            )

    def _release(self) -> None:
        self._lock.release()

    def _render_locked(
        self,
        camera: Camera,
        screen_error_threshold: float,
        seam_tolerance: float,
        max_level: int,
        config: RenderConfig,
        snapshot_path: str | Path | None,
        heightfield_name: str | None,
        material_name: str | None,
        cancel_token: CancellationToken | None,
    ) -> RenderResult:
        result: RenderResult | None = None
        try:
            result = self.renderer.render(
                self.terrain,
                camera,
                screen_error_threshold,
                seam_tolerance,
                max_level=max_level,
                config=config,
                cancel_token=cancel_token,
            )
            if snapshot_path is not None:
                snapshot = save_snapshot(
                    snapshot_path,
                    result,
                    self.terrain,
                    heightfield_name=heightfield_name,
                    material_name=material_name,
                )
                self.last_snapshot = snapshot
            self.last_result = result
            return result
        finally:
            result = None

    def render(
        self,
        camera: Camera,
        screen_error_threshold: float,
        seam_tolerance: float,
        max_level: int = 4,
        config: RenderConfig | None = None,
        snapshot_path: str | Path | None = None,
        heightfield_name: str | None = None,
        material_name: str | None = None,
        cancel_token: CancellationToken | None = None,
    ) -> RenderResult:
        self._acquire()
        try:
            return self._render_locked(
                camera,
                screen_error_threshold,
                seam_tolerance,
                max_level,
                config or RenderConfig(),
                snapshot_path,
                heightfield_name,
                material_name,
                cancel_token,
            )
        finally:
            self._release()

    def submit_render(
        self,
        camera: Camera,
        screen_error_threshold: float,
        seam_tolerance: float,
        max_level: int = 4,
        config: RenderConfig | None = None,
        snapshot_path: str | Path | None = None,
        heightfield_name: str | None = None,
        material_name: str | None = None,
        cancel_token: CancellationToken | None = None,
    ) -> tuple[Future[RenderResult], CancellationToken]:
        token = cancel_token or CancellationToken()
        self._acquire()
        future: Future[RenderResult] = Future()

        def worker() -> None:
            try:
                result = self._render_locked(
                    camera,
                    screen_error_threshold,
                    seam_tolerance,
                    max_level,
                    config or RenderConfig(),
                    snapshot_path,
                    heightfield_name,
                    material_name,
                    token,
                )
                future.set_result(result)
            except BaseException as exc:
                future.set_exception(exc)
            finally:
                self._release()

        thread = threading.Thread(target=worker, name="terrainviz-render", daemon=True)
        self._worker_thread = thread
        try:
            thread.start()
        except BaseException:
            self._release()
            raise
        return future, token

    def recover_snapshot(self, snapshot_path: str | Path) -> Snapshot:
        """Recovery uses only the committed snapshot on disk."""
        snapshot = load_snapshot(snapshot_path)
        self.last_snapshot = snapshot
        return snapshot

    @classmethod
    def recover_from_disk(
        cls,
        terrain: Terrain,
        snapshot_path: str | Path,
    ) -> "RenderService":
        snapshot = load_snapshot(snapshot_path)
        expected_digest = snapshot.content["heightfield"].get("height_digest")
        if expected_digest != terrain.heightfield.height_digest:
            raise TerrainError(
                E_SNAPSHOT_INVALID,
                "snapshot and heightfield content do not match",
                snapshot_height_digest=expected_digest,
                heightfield_digest=terrain.heightfield.height_digest,
            )
        service = cls(terrain)
        service.last_snapshot = snapshot
        return service

