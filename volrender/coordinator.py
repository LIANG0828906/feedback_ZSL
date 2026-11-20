"""Single-writer render coordination, cancellation and chunk journaling."""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
from pathlib import Path
import threading
from typing import Any, Callable, Mapping

from .budget import BudgetConfig
from .errors import RenderBusyError
from .renderer import RayCaster, RenderResult
from .snapshot import SnapshotManager
from .view import CameraView


def render_key(volume_hash: str, view_hash: str) -> tuple[str, str]:
    return (str(volume_hash), str(view_hash))


class RenderCoordinator:
    """Guarantees at most one in-flight render per volume/view pair."""

    def __init__(self, *, snapshot_manager: SnapshotManager | None = None) -> None:
        self._lock = threading.Lock()
        self._active: set[tuple[str, str]] = set()
        self._cancel_events: dict[tuple[str, str], threading.Event] = {}
        self.snapshots = snapshot_manager or SnapshotManager()
        self.last_result: RenderResult | None = None

    def _acquire(self, key: tuple[str, str]) -> threading.Event:
        with self._lock:
            if key in self._active:
                raise RenderBusyError(
                    "RENDER_IN_FLIGHT",
                    "同一体数据同一视角已有渲染任务",
                    field="render_key",
                    usage={"volume_hash": key[0], "view_hash": key[1]},
                )
            event = threading.Event()
            self._active.add(key)
            self._cancel_events[key] = event
            return event

    def _release(self, key: tuple[str, str]) -> None:
        with self._lock:
            self._active.discard(key)
            self._cancel_events.pop(key, None)

    @property
    def active_keys(self) -> list[tuple[str, str]]:
        with self._lock:
            return sorted(self._active)

    def cancel(self, volume_hash: str, view_hash: str) -> bool:
        with self._lock:
            event = self._cancel_events.get((volume_hash, view_hash))
            if event is None:
                return False
            event.set()
            return True

    def render(
        self,
        raycaster: RayCaster,
        view: CameraView,
        *,
        width: int,
        height: int,
        step_size: float,
        budget: BudgetConfig,
        checkpoint: Callable[[str, Mapping[str, Any]], None] | None = None,
        journal: ChunkJournal | None = None,
        commit_snapshot_path: str | Path | None = None,
    ) -> RenderResult:
        key = render_key(raycaster.volume.volume_sha256, view.hash)
        cancel_event = self._acquire(key)
        try:
            combined_checkpoint = checkpoint
            if journal is not None:
                def combined_checkpoint(stage: str, details: Mapping[str, Any]) -> None:
                    journal.record(stage, details)
                    if checkpoint is not None:
                        checkpoint(stage, details)
            result = raycaster.render(
                view,
                width=width,
                height=height,
                step_size=step_size,
                budget=budget,
                cancel_event=cancel_event,
                checkpoint=combined_checkpoint,
            )
            self.last_result = result
            if commit_snapshot_path is not None:
                self.snapshots.commit(commit_snapshot_path, result)
            return result
        finally:
            # CPU buffers are local.  No file handles are intentionally kept
            # open across a render.  This hook is where a GL backend would
            # release its FBO/offscreen surface.
            self._release(key)


class ChunkJournal:
    """Append-only completed-batch records for post-crash diagnostics."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def record(self, stage: str, details: Mapping[str, Any]) -> None:
        row = {"event": "chunk", "stage": stage, **dict(details)}
        line = json.dumps(row, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n"
        with self._lock:
            with self.path.open("a", encoding="utf-8") as handle:
                handle.write(line)
                handle.flush()
                os.fsync(handle.fileno())

    def __call__(self, stage: str, details: Mapping[str, Any]) -> None:
        self.record(stage, details)

    def last_completed_rays(self) -> int:
        if not self.path.exists():
            return 0
        last = 0
        with self.path.open("r", encoding="utf-8") as handle:
            for line in handle:
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                last = max(last, int(row.get("completed_rays", 0)))
        return last


@dataclass
class RecoveryState:
    snapshot: dict[str, Any] | None
    snapshot_path: Path | None
    last_completed_rays: int


def recover_from_disk(
    *,
    snapshot_path: str | Path,
    journal_path: str | Path | None = None,
) -> RecoveryState:
    """Recover only from the committed snapshot and completed chunk records."""

    snapshot_file = Path(snapshot_path)
    snapshot = load_snapshot(snapshot_file) if snapshot_file.exists() else None
    completed = ChunkJournal(journal_path).last_completed_rays() if journal_path else 0
    return RecoveryState(
        snapshot=snapshot,
        snapshot_path=snapshot_file if snapshot is not None else None,
        last_completed_rays=completed,
    )


__all__ = [
    "ChunkJournal",
    "RecoveryState",
    "RenderCoordinator",
    "recover_from_disk",
    "render_key",
]

