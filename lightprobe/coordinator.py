from __future__ import annotations

import threading
import time
import uuid
from pathlib import Path
from typing import Any

from .bake import Baker
from .bake_types import BakeConfig, BakeReport
from .errors import ProbeError
from .scene import Scene
from .snapshot import load_snapshot
from .version import VersionedStore

_GLOBAL_LOCK = threading.Lock()
_ACTIVE_SCENE_RUNS: dict[str, str] = {}


class SceneRuntime:
    """Single-writer bake coordinator with a non-blocking versioned reader."""

    def __init__(
        self,
        scene: Scene,
        *,
        snapshot_root: str | Path = "artifacts/runs",
        store: VersionedStore | None = None,
    ) -> None:
        self.scene = scene
        self.snapshot_root = Path(snapshot_root)
        self.store = store or VersionedStore()
        self.baker = Baker()
        self._lock = threading.RLock()
        self._busy = False
        self._cancel_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._active_run_id: str | None = None
        self._started_at: float | None = None
        self._last_report: BakeReport | None = None
        self._last_error: dict[str, Any] | None = None
        self._phase: dict[str, Any] = {"phase": "idle"}

    def status(self) -> dict[str, Any]:
        with self._lock:
            current = self.store.current
            return {
                "ok": True,
                "busy": self._busy,
                "run_id": self._active_run_id,
                "started_at": self._started_at,
                "phase": dict(self._phase),
                "published": None if current is None else current.summary(),
                "last_report": None if self._last_report is None else self._last_report.to_dict(),
                "last_error": self._last_error,
                "scene": self.scene.to_dict(),
            }

    def start_bake(
        self,
        updates: dict[str, Any] | None = None,
        *,
        config_overrides: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        updates = updates or {}
        run_id = uuid.uuid4().hex
        with self._lock:
            if self._busy:
                raise ProbeError(
                    "E_BUSY",
                    "a bake task is already running for this scene",
                    {"scene_id": self.scene.scene_id, "run_id": self._active_run_id},
                )
            try:
                next_scene = self.scene.with_updates(updates) if updates else self.scene
                config = BakeConfig.from_scene(
                    next_scene, **(config_overrides or {})
                ).validated()
            except Exception:
                raise
            with _GLOBAL_LOCK:
                active = _ACTIVE_SCENE_RUNS.get(next_scene.scene_id)
                if active is not None:
                    raise ProbeError(
                        "E_BUSY",
                        "a bake task is already running for this scene",
                        {"scene_id": next_scene.scene_id, "run_id": active},
                    )
                _ACTIVE_SCENE_RUNS[next_scene.scene_id] = run_id
            self._busy = True
            self._cancel_event = threading.Event()
            self._active_run_id = run_id
            self._started_at = time.time()
            self._last_error = None
            self._phase = {"phase": "queued", "run_id": run_id}
            current_version = self.store.current.version if self.store.current else 0
            target_version = current_version + 1
            snapshot_dir = (
                self.snapshot_root
                / _safe_name(next_scene.scene_id)
                / run_id
            )
            thread = threading.Thread(
                target=self._worker,
                args=(next_scene, config, target_version, snapshot_dir, run_id),
                name=f"lightprobe-bake-{run_id[:8]}",
                daemon=True,
            )
            self._thread = thread
            try:
                thread.start()
            except Exception:
                self._busy = False
                self._thread = None
                self._active_run_id = None
                with _GLOBAL_LOCK:
                    if _ACTIVE_SCENE_RUNS.get(next_scene.scene_id) == run_id:
                        _ACTIVE_SCENE_RUNS.pop(next_scene.scene_id, None)
                raise
        return {"ok": True, "run_id": run_id, "target_version": target_version}

    def _worker(
        self,
        scene: Scene,
        config: BakeConfig,
        target_version: int,
        snapshot_dir: Path,
        run_id: str,
    ) -> None:
        try:
            base_state = self.store.current.state if self.store.current is not None else None
            result = self.baker.bake(
                scene,
                config,
                base_state=base_state,
                snapshot_dir=snapshot_dir,
                target_version=target_version,
                run_id=run_id,
                cancel_event=self._cancel_event,
                progress=self._progress,
            )
            published = self.store.publish(result.state, version=target_version)
            report = result.report
            report.version = published.version
            with self._lock:
                self.scene = scene
                self._last_report = report
                self._phase = {
                    "phase": "published",
                    "run_id": run_id,
                    "version": published.version,
                }
        except ProbeError as exc:
            with self._lock:
                self._last_error = exc.to_dict()
                self._phase = {
                    "phase": "cancelled" if exc.code == "E_CANCELLED" else "failed",
                    "run_id": run_id,
                    "code": exc.code,
                }
        except Exception as exc:  # pragma: no cover - defensive boundary
            with self._lock:
                self._last_error = {
                    "ok": False,
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "details": {"type": type(exc).__name__},
                }
                self._phase = {"phase": "failed", "run_id": run_id, "code": "E_INTERNAL"}
        finally:
            with self._lock:
                self._busy = False
                self._thread = None
                self._active_run_id = None
            with _GLOBAL_LOCK:
                if _ACTIVE_SCENE_RUNS.get(scene.scene_id) == run_id:
                    _ACTIVE_SCENE_RUNS.pop(scene.scene_id, None)

    def _progress(self, update: dict[str, Any]) -> None:
        with self._lock:
            self._phase = dict(update)

    def cancel(self) -> dict[str, Any]:
        with self._lock:
            if not self._busy:
                return {"ok": True, "cancelled": False, "message": "no bake is running"}
            self._cancel_event.set()
            self._phase = {**self._phase, "phase": "cancelling"}
            return {
                "ok": True,
                "cancelled": True,
                "run_id": self._active_run_id,
            }

    def wait(self, timeout: float | None = None) -> bool:
        with self._lock:
            thread = self._thread
        if thread is not None:
            thread.join(timeout)
            return not thread.is_alive()
        return True

    def query(
        self,
        position: list[float] | tuple[float, float, float],
        normal: list[float] | None = None,
        *,
        expected_version: int | None = None,
        expected_content_hash: str | None = None,
    ) -> dict[str, Any]:
        return self.store.query(
            position,
            normal,
            expected_version=expected_version,
            expected_content_hash=expected_content_hash,
        )

    def resume_snapshot(self, snapshot_dir: str | Path) -> dict[str, Any]:
        with self._lock:
            if self._busy:
                raise ProbeError(
                    "E_BUSY",
                    "a bake task is already running for this scene",
                    {"scene_id": self.scene.scene_id, "run_id": self._active_run_id},
                )
        run_token = f"resume-{uuid.uuid4().hex}"
        with _GLOBAL_LOCK:
            active = _ACTIVE_SCENE_RUNS.get(self.scene.scene_id)
            if active is not None:
                raise ProbeError(
                    "E_BUSY",
                    "a bake task is already running for this scene",
                    {"scene_id": self.scene.scene_id, "run_id": active},
                )
            _ACTIVE_SCENE_RUNS[self.scene.scene_id] = run_token
        try:
            recovered = load_snapshot(snapshot_dir)
            config_payload = dict(recovered.metadata["config"])
            config = BakeConfig(**config_payload).validated()
            result = self.baker.bake(
                self.scene,
                config,
                recovered=recovered,
                snapshot_dir=snapshot_dir,
                target_version=int(recovered.metadata["target_version"]),
                run_id=str(recovered.metadata["run_id"]),
            )
            published = self.store.publish(
                result.state, version=int(recovered.metadata["target_version"])
            )
            result.report.version = published.version
            with self._lock:
                self._last_report = result.report
                self._phase = {
                    "phase": "published",
                    "run_id": recovered.metadata["run_id"],
                    "version": published.version,
                }
            return result.report.to_dict()
        finally:
            with _GLOBAL_LOCK:
                if _ACTIVE_SCENE_RUNS.get(self.scene.scene_id) == run_token:
                    _ACTIVE_SCENE_RUNS.pop(self.scene.scene_id, None)


def _safe_name(value: str) -> str:
    return "".join(character if character.isalnum() or character in "-_" else "_" for character in value)

