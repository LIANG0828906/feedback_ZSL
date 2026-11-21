"""Threaded replay controller with E_BUSY and cooperative cancellation."""

from __future__ import annotations

import queue
import threading
import time
from collections.abc import Sequence
from pathlib import Path
from typing import Any, ClassVar

from .errors import ReviewError
from .model import InitialState, RevocationCommand, ShotRecord
from .simulation import Simulation
from .snapshot import SnapshotStore

IDLE = "IDLE"
RUNNING = "RUNNING"
PAUSED = "PAUSED"
CANCELLED = "CANCELLED"
COMPLETED = "COMPLETED"
FAILED = "FAILED"


class WorkbenchRunner:
    """Owns the only active progression task for one replay branch."""

    _active_paths: ClassVar[set[Path]] = set()
    _active_paths_guard = threading.Lock()

    def __init__(
        self,
        initial: InitialState,
        shots: Sequence[ShotRecord],
        revocations: Sequence[RevocationCommand],
        store: SnapshotStore,
        *,
        step_delay: float = 0.02,
        progress_fault_step: int | None = None,
    ) -> None:
        self.initial = initial
        self.shots = list(shots)
        self.revocations = list(revocations)
        self.store = store
        self.step_delay = max(0.0, float(step_delay))
        self.progress_fault_step = progress_fault_step
        self._guard = threading.Lock()
        self._busy = False
        self._thread: threading.Thread | None = None
        self._cancel_event = threading.Event()
        self._pause_requested = threading.Event()
        self._pause_release = threading.Event()
        self._pause_release.set()
        self._wake = threading.Event()
        self._commands: queue.Queue[dict[str, Any]] = queue.Queue()
        self._status = IDLE
        self._last_error: dict[str, Any] | None = None
        self._last_summary: dict[str, Any] | None = None
        self._restored_from_step: int | None = None
        self._next_manual_seq = 10_000_000
        self._manual_command_counter = 0

    @property
    def busy(self) -> bool:
        with self._guard:
            return self._busy

    @property
    def status(self) -> str:
        with self._guard:
            return self._status

    @property
    def last_summary(self) -> dict[str, Any] | None:
        return self._last_summary

    @property
    def last_error(self) -> dict[str, Any] | None:
        return self._last_error

    @property
    def restored_from_step(self) -> int | None:
        return self._restored_from_step

    def status_dict(self) -> dict[str, Any]:
        latest = self.store.list_steps()
        return {
            "status": self.status,
            "busy": self.busy,
            "last_published_step": latest[-1] if latest else 0,
            "last_error": self._last_error,
            "restored_from_step": self._restored_from_step,
            "snapshot_directory": str(self.store.directory),
        }

    def start(self, *, restore: bool = False, end_step: int | None = None) -> None:
        branch_path = self.store.directory.resolve()
        with self._active_paths_guard:
            if branch_path in self._active_paths:
                raise ReviewError(
                    "E_BUSY",
                    "another progression task is active for this replay branch",
                    branch=str(branch_path),
                )
            self._active_paths.add(branch_path)
        with self._guard:
            if self._busy:
                with self._active_paths_guard:
                    self._active_paths.discard(branch_path)
                raise ReviewError(
                    "E_BUSY",
                    "a progression task is already active for this match",
                )
            self._busy = True
            self._cancel_event.clear()
            self._pause_requested.clear()
            self._pause_release.set()
            self._wake.clear()
            self._last_error = None
            self._status = RUNNING
        try:
            if restore:
                recovery = self.store.load_simulation(
                    self.initial, self.shots, self.revocations
                )
                simulation = recovery.simulation
                self._restored_from_step = simulation.state.step
                self._last_summary = recovery.summary
            else:
                self.store.reset()
                simulation = Simulation(
                    self.initial,
                    self.shots,
                    self.revocations,
                    progress_fault_step=self.progress_fault_step,
                )
                self._restored_from_step = None
                self._last_summary = None
        except Exception as exc:
            with self._guard:
                self._busy = False
                self._status = FAILED
                self._last_error = (
                    exc.to_dict()
                    if isinstance(exc, ReviewError)
                    else {"code": "E_RUNNER_START", "message": str(exc)}
                )
            with self._active_paths_guard:
                self._active_paths.discard(branch_path)
            raise
        thread = threading.Thread(
            target=self._run,
            args=(simulation, end_step, branch_path),
            name="cover-review-progression",
            daemon=True,
        )
        self._thread = thread
        thread.start()

    def _run(
        self,
        simulation: Simulation,
        end_step: int | None,
        branch_path: Path,
    ) -> None:
        final_status = COMPLETED
        try:
            end = self.initial.end_step if end_step is None else min(
                int(end_step), self.initial.end_step
            )
            while simulation.state.step < end:
                if self._cancel_event.is_set():
                    final_status = CANCELLED
                    break
                self._wait_at_boundary()
                if self._cancel_event.is_set():
                    final_status = CANCELLED
                    break
                extra = self._drain_commands(simulation.next_step)
                summary = simulation.advance(extra_revocations=extra)
                self.store.write(simulation)
                self._last_summary = summary
                if self.step_delay:
                    self._cancel_event.wait(self.step_delay)
            if self._cancel_event.is_set() and final_status != CANCELLED:
                remaining = simulation.state.step < end
                final_status = CANCELLED if remaining else COMPLETED
        except ReviewError as exc:
            final_status = FAILED
            self._last_error = exc.to_dict()
        except Exception as exc:
            final_status = FAILED
            self._last_error = {"code": "E_RUNNER_FAILURE", "message": str(exc)}
        finally:
            with self._guard:
                self._busy = False
                self._status = final_status
                self._thread = None
            with self._active_paths_guard:
                self._active_paths.discard(branch_path)
            self._wake.set()

    def _wait_at_boundary(self) -> None:
        if not self._pause_requested.is_set():
            return
        with self._guard:
            self._status = PAUSED
        while self._pause_requested.is_set() and not self._cancel_event.is_set():
            self._pause_release.wait(0.05)
        if not self._cancel_event.is_set():
            with self._guard:
                self._status = RUNNING

    def _drain_commands(self, next_step: int) -> list[RevocationCommand]:
        commands: list[RevocationCommand] = []
        while True:
            try:
                item = self._commands.get_nowait()
            except queue.Empty:
                break
            self._next_manual_seq += 1
            commands.append(
                RevocationCommand(
                    revocation_id=str(item["revocation_id"]),
                    player_id=str(item["player_id"]),
                    client_seq=int(item["client_seq"]),
                    arrival_step=next_step,
                    arrival_seq=self._next_manual_seq,
                    reason=str(item.get("reason", "")),
                )
            )
        return commands

    def pause(self) -> None:
        if not self.busy:
            return
        self._pause_requested.set()
        self._pause_release.clear()
        with self._guard:
            if self._status == RUNNING:
                self._status = PAUSED

    def resume(self) -> None:
        self._pause_requested.clear()
        self._pause_release.set()
        with self._guard:
            if self._busy and self._status == PAUSED:
                self._status = RUNNING
        self._wake.set()

    def cancel(self) -> None:
        self._cancel_event.set()
        self._pause_requested.clear()
        self._pause_release.set()
        self._wake.set()

    def wait(self, timeout: float | None = None) -> bool:
        thread = self._thread
        if thread is not None:
            thread.join(timeout)
            return not thread.is_alive()
        return True

    def inject_revocation(
        self,
        player_id: str,
        client_seq: int,
        *,
        reason: str = "interactive revocation injection",
        revocation_id: str | None = None,
    ) -> dict[str, Any]:
        if not player_id or int(client_seq) < 1:
            raise ReviewError(
                "E_INPUT_INVALID", "player and a positive client sequence are required"
            )
        if revocation_id is None:
            with self._guard:
                self._manual_command_counter += 1
                command_counter = self._manual_command_counter
            identifier = (
                f"runtime-revoke-{player_id}-{int(client_seq)}-"
                f"{command_counter:04d}"
            )
        else:
            identifier = revocation_id
        command = {
            "revocation_id": identifier,
            "player_id": player_id,
            "client_seq": int(client_seq),
            "reason": reason,
        }
        self._commands.put(command)
        return command

    def wait_until_idle(self, timeout: float = 30.0) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not self.busy:
                return True
            time.sleep(0.01)
        return False


def latest_published_summary(store: SnapshotStore) -> dict[str, Any] | None:
    steps = store.list_steps()
    if not steps:
        return None
    path = store.snapshot_path(steps[-1])
    try:
        import json

        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, ValueError):
        return None

