"""Fitting session, group scheduling, snapshot publication and cancellation."""

from __future__ import annotations

import hashlib
import os
import sys
import threading
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping

from .checkpoint import CheckpointManager, CheckpointState, PublishedSnapshot
from .errors import E_BUSY, E_CANCELLED, E_STALE_HANDLE, AppError
from .graph import IncrementalEvaluator
from .params import GROUPS, ParameterSet, canonical_json_bytes
from .reduction import GradientResult
from .scene import Scene


@dataclass(frozen=True)
class FittingConfig:
    steps: int = 6
    learning_rate: float = 0.02
    group_learning_rates: dict[str, float] = field(default_factory=dict)
    active_groups: tuple[str, ...] = GROUPS
    chunk_size: int | tuple[int, int] = 8
    parallel: int = 2
    verify_incremental: bool = True
    checkpoint_dir: str | None = None
    termination_after_step: int | None = None
    hard_exit_after_step: int | None = None

    def __post_init__(self) -> None:
        if self.steps < 0:
            raise ValueError("steps must be non-negative")
        if self.parallel < 1:
            raise ValueError("parallel must be at least one")
        if not self.active_groups:
            raise ValueError("active_groups must not be empty")
        unknown = set(self.active_groups) - set(GROUPS)
        if unknown:
            raise ValueError(f"unknown active groups: {sorted(unknown)}")
        for group in self.active_groups:
            if self.rate(group) < 0.0:
                raise ValueError("learning rates must be non-negative")

    def rate(self, group: str) -> float:
        return float(self.group_learning_rates.get(group, self.learning_rate))


@dataclass(frozen=True)
class FitOutcome:
    status: str
    steps_completed: int
    latest: PublishedSnapshot | None
    history: tuple[dict[str, object], ...]
    error: dict[str, object] | None = None


class SceneLeaseRegistry:
    """Process-local exclusion for fitting one scene at a time."""

    _lock = threading.Lock()
    _active: set[str] = set()

    @classmethod
    def acquire(cls, scene_id: str) -> None:
        with cls._lock:
            if scene_id in cls._active:
                raise AppError(
                    E_BUSY,
                    "a fitting task is already active for this scene",
                    "scene_id",
                    {"scene_id": scene_id},
                )
            cls._active.add(scene_id)

    @classmethod
    def release(cls, scene_id: str) -> None:
        with cls._lock:
            cls._active.discard(scene_id)

    @classmethod
    def active(cls) -> tuple[str, ...]:
        with cls._lock:
            return tuple(sorted(cls._active))


class SimulatedTermination(RuntimeError):
    """Test hook raised immediately after a checkpoint commit."""


class FittingSession:
    def __init__(self, scene: Scene, config: FittingConfig) -> None:
        if not config.checkpoint_dir:
            raise ValueError("checkpoint_dir is required")
        self.scene = scene
        self.config = config
        self._lock = threading.RLock()
        self._cancel = threading.Event()
        self._thread: threading.Thread | None = None
        self._running = False
        self._status = "idle"
        self._latest: PublishedSnapshot | None = None
        self._history: tuple[dict[str, object], ...] = ()
        self._degradations: list[dict[str, object]] = []
        self._queue: list[dict[str, object]] = []
        self._last_error: AppError | None = None
        self._outcome: FitOutcome | None = None
        self.manager = CheckpointManager(config.checkpoint_dir, scene.scene_id)

    @property
    def status(self) -> str:
        with self._lock:
            return self._status

    @property
    def latest_snapshot(self) -> PublishedSnapshot | None:
        with self._lock:
            return self._latest

    @property
    def degradations(self) -> tuple[dict[str, object], ...]:
        with self._lock:
            return tuple(dict(item) for item in self._degradations)

    def set_queue(self, queue: Iterable[Mapping[str, object]]) -> None:
        with self._lock:
            self._queue = [dict(item) for item in queue]

    def queue_snapshot(self) -> tuple[dict[str, object], ...]:
        with self._lock:
            return tuple(dict(item) for item in self._queue)

    def update_group(self, group: str, updates: Mapping[str, float]) -> None:
        with self._lock:
            if self._running:
                raise AppError(
                    E_BUSY,
                    "cannot edit parameters while fitting is running",
                    "session",
                )
            self.scene.params.update(group, updates)

    def start(self) -> None:
        with self._lock:
            if self._running:
                raise AppError(E_BUSY, "fitting session is already running", "session")
            SceneLeaseRegistry.acquire(self.scene.scene_id)
            self._running = True
            self._status = "starting"
            self._outcome = None
            self._cancel.clear()
            self._thread = threading.Thread(
                target=self._thread_main,
                name=f"fit-{self.scene.scene_id[:8]}",
                daemon=True,
            )
            self._thread.start()

    def run_sync(self) -> FitOutcome:
        with self._lock:
            if self._running:
                raise AppError(E_BUSY, "fitting session is already running", "session")
            SceneLeaseRegistry.acquire(self.scene.scene_id)
            self._running = True
            self._status = "starting"
            self._outcome = None
            self._cancel.clear()
        return self._run_guarded()

    def join(self, timeout: float | None = None) -> FitOutcome | None:
        with self._lock:
            thread = self._thread
        if thread is not None:
            thread.join(timeout)
        with self._lock:
            return self._outcome

    def cancel(self) -> None:
        self._cancel.set()
        with self._lock:
            if self._running and self._status not in ("cancelling", "terminating"):
                self._status = "cancelling"

    def query(self, handle: str | None = None) -> PublishedSnapshot:
        with self._lock:
            latest = self._latest
            if latest is None:
                raise AppError(
                    E_STALE_HANDLE,
                    "no checkpoint has been published",
                    "handle",
                    {"requested": handle},
                )
            if handle is not None and handle != latest.handle:
                raise AppError(
                    E_STALE_HANDLE,
                    "snapshot handle is no longer current",
                    "handle",
                    {"requested": handle, "current": latest.handle},
                )
            return PublishedSnapshot(
                handle=latest.handle,
                step=latest.step,
                loss=latest.loss,
                cumulative_loss=latest.cumulative_loss,
                params=_deep_copy(latest.params),
                gradient_norms=dict(latest.gradient_norms),
                history=tuple(_deep_copy(item) for item in latest.history),
                reduce_hash=latest.reduce_hash,
                checkpoint_hash=latest.checkpoint_hash,
            )

    def _thread_main(self) -> None:
        self._run_guarded()

    def _run_guarded(self) -> FitOutcome:
        try:
            outcome = self._run()
            with self._lock:
                self._outcome = outcome
                self._status = outcome.status
            return outcome
        except SimulatedTermination as exc:
            outcome = FitOutcome(
                status="terminated",
                steps_completed=self._latest.step + 1 if self._latest else 0,
                latest=self._latest,
                history=self._history,
                error={"code": "SIMULATED_TERMINATION", "message": str(exc)},
            )
            with self._lock:
                self._outcome = outcome
                self._status = "terminated"
            return outcome
        except AppError as exc:
            outcome = FitOutcome(
                status="failed",
                steps_completed=self._latest.step + 1 if self._latest else 0,
                latest=self._latest,
                history=self._history,
                error=exc.to_dict(),
            )
            with self._lock:
                self._last_error = exc
                self._outcome = outcome
                self._status = "failed"
            return outcome
        except BaseException as exc:
            outcome = FitOutcome(
                status="failed",
                steps_completed=self._latest.step + 1 if self._latest else 0,
                latest=self._latest,
                history=self._history,
                error={"code": "E_INTERNAL", "message": repr(exc)},
            )
            with self._lock:
                self._outcome = outcome
                self._status = "failed"
            return outcome
        finally:
            SceneLeaseRegistry.release(self.scene.scene_id)
            with self._lock:
                self._running = False

    def _run(self) -> FitOutcome:
        checkpoint = self.manager.load_latest(strict=True)
        if checkpoint is None:
            params = self.scene.params.clone()
            history: list[dict[str, object]] = []
            cumulative_loss = 0.0
            next_step = 0
            evaluator = IncrementalEvaluator(self.scene)
            evaluator.warm_cache(params, chunk_size=self.config.chunk_size)
            with self._lock:
                self._history = ()
        else:
            params = checkpoint.params.clone()
            history = list(checkpoint.history)
            cumulative_loss = checkpoint.cumulative_loss
            next_step = checkpoint.step + 1
            evaluator = IncrementalEvaluator(self.scene)
            evaluator.warm_cache(params, chunk_size=self.config.chunk_size)
            self._publish(checkpoint.snapshot)
        with self._lock:
            self._history = tuple(history)
            self._status = "running"

        while next_step < self.config.steps:
            if self._cancel.is_set():
                with self._lock:
                    self._status = "cancelled"
                return FitOutcome(
                    status="cancelled",
                    steps_completed=next_step,
                    latest=self._latest,
                    history=tuple(history),
                )
            group = self.config.active_groups[next_step % len(self.config.active_groups)]
            result = evaluator.evaluate(
                params,
                modified_groups=(group,),
                chunk_size=self.config.chunk_size,
                parallel=self.config.parallel,
                verify_incremental=self.config.verify_incremental,
            )
            if result.degraded:
                degradation = {
                    "step": next_step,
                    "group": group,
                    "reason": result.downgrade_reason,
                    "recomputed": list(result.recomputed_nodes),
                }
                with self._lock:
                    self._degradations.append(degradation)
            gradient_norms = result.gradient_norms()
            learning_rate = self.config.rate(group)
            params.add_vector(group, result.gradients[group].tolist(), -learning_rate)
            cumulative_loss += result.loss
            event = self._make_event(
                step=next_step,
                group=group,
                result=result,
                gradient_norms=gradient_norms,
                cumulative_loss=cumulative_loss,
                learning_rate=learning_rate,
                params=params,
            )
            history.append(event)
            state = self.manager.commit(
                step=next_step,
                params=params,
                cumulative_loss=cumulative_loss,
                last_loss=result.loss,
                gradient_norms=gradient_norms,
                reduce_hash=result.gradients_sha256,
                chunk_hashes=result.chunk_hashes,
                optimizer_state={
                    "algorithm": "sgd",
                    "active_group": group,
                    "learning_rate": learning_rate,
                },
                history=history,
            )
            self._publish(state.snapshot)
            with self._lock:
                self._history = tuple(history)
            if self.config.termination_after_step == next_step:
                raise SimulatedTermination(
                    f"terminated after committed step {next_step}"
                )
            if self.config.hard_exit_after_step == next_step:
                sys.stdout.flush()
                sys.stderr.flush()
                os._exit(17)
            if next_step + 1 < self.config.steps and not self._cancel.is_set():
                evaluator.warm_cache(params, chunk_size=self.config.chunk_size)
            next_step += 1

        with self._lock:
            status = "cancelled" if self._cancel.is_set() else "completed"
            self._status = status
            return FitOutcome(
                status=status,
                steps_completed=next_step,
                latest=self._latest,
                history=tuple(history),
            )

    def _make_event(
        self,
        *,
        step: int,
        group: str,
        result: GradientResult,
        gradient_norms: Mapping[str, float],
        cumulative_loss: float,
        learning_rate: float,
        params: ParameterSet,
    ) -> dict[str, object]:
        base: dict[str, object] = {
            "event": "fit_step",
            "step": step,
            "scene_id": self.scene.scene_id,
            "modified_group": group,
            "affected_node_count": len(result.affected_nodes),
            "affected_nodes": list(result.affected_nodes),
            "reused_node_count": len(result.reused_nodes),
            "reused_nodes": list(result.reused_nodes),
            "recomputed_node_count": len(result.recomputed_nodes),
            "recomputed_nodes": list(result.recomputed_nodes),
            "loss": result.loss,
            "cumulative_loss": cumulative_loss,
            "gradient_norms": {
                name: float(gradient_norms[name]) for name in GROUPS
            },
            "parallel": self.config.parallel,
            "chunk_size": (
                list(self.config.chunk_size)
                if isinstance(self.config.chunk_size, tuple)
                else self.config.chunk_size
            ),
            "reduce_hash": result.gradients_sha256,
            "reduction_order": [list(key) for key in result.reduction_order],
            "chunk_hashes": list(result.chunk_hashes),
            "gradient_sha256": result.gradients_sha256,
            "degraded": result.degraded,
            "downgrade_reason": result.downgrade_reason,
            "verification": result.verification,
            "learning_rate": learning_rate,
            "param_versions": dict(params.versions),
            "param_content_hashes": {
                name: params.content_hash(name) for name in GROUPS
            },
        }
        base["content_hash"] = hashlib.sha256(
            canonical_json_bytes(base)
        ).hexdigest()
        return base

    def _publish(self, snapshot: PublishedSnapshot) -> None:
        with self._lock:
            self._latest = snapshot


def _deep_copy(value: Any) -> Any:
    if isinstance(value, dict):
        return {key: _deep_copy(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_deep_copy(item) for item in value]
    return value

