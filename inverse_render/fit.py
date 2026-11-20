"""Cancellable grouped fitting with atomic checkpoint publication."""

from __future__ import annotations

import fcntl
import os
import threading
import time
from collections import OrderedDict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

import torch

from .checkpoint import CheckpointManager, ResumeState
from .engine import EvaluationResult, GradientResult, IncrementalEngine
from .errors import ErrorCode, ToolError
from .graph import build_dependency_graph
from .hashing import atomic_write_json, canonical_json_bytes, combine_hashes
from .params import ParameterStore, load_group_specs
from .scene import Scene, load_scene
from .serialization import trace_record
from .snapshot import Snapshot, SnapshotPublisher, make_snapshot


TRACE_FILENAME = "trace.jsonl"


def _lock_directory() -> Path:
    base = Path(os.environ.get("INVERSE_RENDER_LOCK_DIR", os.path.join("/tmp", f"inverse-render-locks-{os.getuid()}")))
    base.mkdir(parents=True, exist_ok=True)
    return base


def fit_lock_path(scene_hash: str) -> Path:
    return _lock_directory() / f"{scene_hash}.fit.lock"


class FitLock:
    """Non-blocking process and thread wide exclusive fit lock."""

    _registry: set[Path] = set()
    _registry_lock = threading.Lock()

    def __init__(self, scene_hash: str) -> None:
        self.path = fit_lock_path(scene_hash)
        self._descriptor: int | None = None
        self._registered = False

    def __enter__(self) -> "FitLock":
        with self._registry_lock:
            if self.path in self._registry:
                raise ToolError(
                    ErrorCode.E_BUSY,
                    "a fit task is already active for this scene",
                    details={"lock": str(self.path)},
                )
            self._registry.add(self.path)
            self._registered = True
        try:
            descriptor = os.open(self.path, os.O_CREAT | os.O_RDWR, 0o600)
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                os.close(descriptor)
                raise ToolError(
                    ErrorCode.E_BUSY,
                    "a fit task is already active for this scene",
                    details={"lock": str(self.path)},
                ) from exc
            self._descriptor = descriptor
            os.ftruncate(descriptor, 0)
            os.write(descriptor, f"{os.getpid()}\n".encode("ascii"))
            os.fsync(descriptor)
            return self
        except Exception:
            if self._descriptor is not None:
                try:
                    fcntl.flock(self._descriptor, fcntl.LOCK_UN)
                finally:
                    os.close(self._descriptor)
                    self._descriptor = None
            self._release_registry()
            raise

    def _release_registry(self) -> None:
        if self._registered:
            with self._registry_lock:
                self._registry.discard(self.path)
            self._registered = False

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        if self._descriptor is not None:
            try:
                fcntl.flock(self._descriptor, fcntl.LOCK_UN)
            finally:
                os.close(self._descriptor)
                self._descriptor = None
        self._release_registry()


class TraceWriter:
    """Append-only canonical JSON Lines; no timestamps or random paths."""

    def __init__(self, path: str | Path, *, append: bool) -> None:
        self.path = Path(path).resolve()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        if not append:
            with open(self.path, "wb") as handle:
                handle.flush()
                os.fsync(handle.fileno())

    def write(self, record: Mapping[str, Any]) -> None:
        payload = canonical_json_bytes(record) + b"\n"
        with open(self.path, "ab") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())


@dataclass
class FitConfig:
    steps: int = 8
    learning_rate: float = 0.02
    schedule: str | Sequence[str] = "all"
    resume: bool = True
    fresh: bool = False
    cancel_event: threading.Event | None = None
    crash_after: int | None = None
    allow_process_exit: bool = False
    deterministic: bool = True
    step_hook: Callable[[int, ParameterStore], None] | None = None

    def validate(self) -> None:
        if isinstance(self.steps, bool) or not isinstance(self.steps, int) or self.steps < 0:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "steps must be a non-negative integer",
                details={"steps": self.steps},
            )
        if self.steps > 0 and (
            not isinstance(self.learning_rate, (int, float))
            or isinstance(self.learning_rate, bool)
            or not float(self.learning_rate) > 0.0
        ):
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "learning_rate must be positive",
                details={"learning_rate": self.learning_rate},
            )
        if self.crash_after is not None and (
            isinstance(self.crash_after, bool)
            or not isinstance(self.crash_after, int)
            or self.crash_after < 0
        ):
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "crash_after must be a non-negative integer",
            )
        if self.crash_after is not None and not self.allow_process_exit:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "crash_after requires allow_process_exit=True",
            )


@dataclass(frozen=True)
class FitResult:
    status: str
    resumed_from: int | None
    completed_step: int
    last_loss: float
    cumulative_loss: float
    checkpoint_hash: str
    graph_version: str
    cache_hashes: Mapping[str, str]
    trace_path: str
    state_dir: str
    steps_executed: int

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("status", self.status),
                ("resumed_from", self.resumed_from),
                ("completed_step", self.completed_step),
                ("last_loss", self.last_loss),
                ("cumulative_loss", self.cumulative_loss),
                ("checkpoint_hash", self.checkpoint_hash),
                ("graph_version", self.graph_version),
                ("cache_hashes", OrderedDict(self.cache_hashes)),
                ("trace_path", self.trace_path),
                ("state_dir", self.state_dir),
                ("steps_executed", self.steps_executed),
            )
        )


def _schedule_groups(
    schedule: str | Sequence[str],
    group_order: list[str],
    step: int,
) -> list[str]:
    if isinstance(schedule, str):
        if schedule == "all":
            return list(group_order)
        if schedule == "rotate":
            return [group_order[(step - 1) % len(group_order)]]
        if schedule in group_order:
            return [schedule]
        if "," in schedule:
            requested = [part.strip() for part in schedule.split(",") if part.strip()]
        else:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID,
                "schedule must be 'all', 'rotate', or a group id/list",
                details={"schedule": schedule},
            )
    else:
        requested = list(schedule)
    if not requested:
        raise ToolError(
            ErrorCode.E_CONFIG_INVALID, "schedule may not be empty"
        )
    unknown = [group_id for group_id in requested if group_id not in group_order]
    if unknown:
        raise ToolError(
            ErrorCode.E_CONFIG_INVALID,
            "schedule contains unknown groups",
            details={"groups": unknown},
        )
    deduplicated: list[str] = []
    for group_id in requested:
        if group_id not in deduplicated:
            deduplicated.append(group_id)
    return deduplicated


def _publish_from_state(
    publisher: SnapshotPublisher,
    *,
    step: int,
    groups: Mapping[str, Mapping[str, Any]],
    last_loss: float,
    cumulative_loss: float,
    graph_version: str,
    cache_hashes: Mapping[str, str],
    checkpoint_hash: str,
) -> Snapshot:
    return publisher.publish(
        step=step,
        groups=groups,
        last_loss=last_loss,
        cumulative_loss=cumulative_loss,
        graph_version=graph_version,
        cache_hashes=cache_hashes,
        checkpoint_hash=checkpoint_hash,
    )


def _trace_evaluation(
    writer: TraceWriter,
    *,
    step: int,
    modified_groups: Sequence[str],
    evaluation: EvaluationResult,
    loss: float,
    gradient_norm: float,
) -> None:
    writer.write(
        trace_record(
            step=step,
            modified_groups=list(modified_groups),
            affected_nodes=len(evaluation.affected_nodes),
            reused_nodes=len(evaluation.reused_nodes),
            recomputed_nodes=len(evaluation.recomputed_nodes),
            loss=loss,
            gradient_norm=gradient_norm,
            content_hash=evaluation.content_hash,
            degraded=evaluation.degraded,
            degraded_reason=evaluation.degraded_reason,
            stale_nodes=list(evaluation.stale_nodes),
            graph_version=evaluation.graph_version,
        )
    )


def _clear_fresh_artifacts(state_dir: Path) -> None:
    for filename in ("checkpoint.json", "checkpoint.json.tmp", TRACE_FILENAME):
        try:
            (state_dir / filename).unlink()
        except FileNotFoundError:
            pass


def run_fit(
    scene_path: str | Path,
    group_path: str | Path | None,
    state_dir: str | Path,
    config: FitConfig | None = None,
    *,
    publisher: SnapshotPublisher | None = None,
) -> FitResult:
    config = config or FitConfig()
    config.validate()
    if config.deterministic:
        torch.set_num_threads(1)
        torch.use_deterministic_algorithms(True)

    scene = load_scene(scene_path)
    group_specs = load_group_specs(scene, group_path)
    graph = build_dependency_graph(scene, group_specs)
    scene_hash = scene.scene_hash()
    state_path = Path(state_dir).resolve()
    state_path.mkdir(parents=True, exist_ok=True)
    trace_path = state_path / TRACE_FILENAME
    publisher = publisher or SnapshotPublisher()

    with FitLock(scene_hash):
        manager = CheckpointManager(
            state_path,
            scene_hash=scene_hash,
            expected_graph_version=graph.version,
            expected_group_order=[group.id for group in group_specs],
            expected_group_parameters={
                group.id: group.parameters for group in group_specs
            },
        )
        if config.fresh:
            _clear_fresh_artifacts(state_path)
        resume = manager.load() if config.resume and manager.exists else None
        if manager.exists and resume is None and not config.fresh:
            raise ToolError(
                ErrorCode.E_CKPT_INVALID,
                "an existing checkpoint requires resume=True or fresh=True",
                details={"path": str(manager.path)},
            )
        if resume is not None and resume.step > config.steps:
            result = FitResult(
                status="completed",
                resumed_from=resume.step,
                completed_step=resume.step,
                last_loss=resume.last_loss,
                cumulative_loss=resume.cumulative_loss,
                checkpoint_hash=resume.checkpoint_hash,
                graph_version=resume.graph_version,
                cache_hashes=resume.cache_hashes,
                trace_path=str(trace_path),
                state_dir=str(state_path),
                steps_executed=0,
            )
            _publish_from_state(
                publisher,
                step=resume.step,
                groups=resume.groups,
                last_loss=resume.last_loss,
                cumulative_loss=resume.cumulative_loss,
                graph_version=resume.graph_version,
                cache_hashes=resume.cache_hashes,
                checkpoint_hash=resume.checkpoint_hash,
            )
            return result

        writer = TraceWriter(trace_path, append=resume is not None and trace_path.exists())
        if resume is None:
            store = ParameterStore(scene, group_specs)
            engine = IncrementalEngine(scene, store, graph)
            evaluation = engine.evaluate()
            prepared_gradient = engine.backward(evaluation) if config.steps > 0 else None
            cumulative_loss = 0.0
            last_loss = float(evaluation.loss.detach().cpu().item())
            checkpoint_hash = manager.commit(
                step=0,
                groups=store.checkpoint_groups(),
                cumulative_loss=cumulative_loss,
                last_loss=last_loss,
                graph_version=graph.version,
                cache_hashes=evaluation.cache_hashes,
            )
            _trace_evaluation(
                writer,
                step=0,
                modified_groups=[],
                evaluation=evaluation,
                loss=last_loss,
                gradient_norm=(
                    prepared_gradient.gradient_norm
                    if prepared_gradient is not None
                    else 0.0
                ),
            )
            _publish_from_state(
                publisher,
                step=0,
                groups=store.checkpoint_groups(),
                last_loss=last_loss,
                cumulative_loss=cumulative_loss,
                graph_version=graph.version,
                cache_hashes=evaluation.cache_hashes,
                checkpoint_hash=checkpoint_hash,
            )
            start_step = 0
            resumed_from: int | None = None
        else:
            try:
                store = ParameterStore.from_checkpoint(scene, group_specs, resume.groups)
            except ToolError as exc:
                if exc.code == ErrorCode.E_CKPT_INVALID:
                    raise
                raise ToolError(
                    ErrorCode.E_CKPT_INVALID,
                    "checkpoint parameter values are invalid",
                    details=exc.details,
                    step=resume.step,
                ) from exc
            engine = IncrementalEngine(scene, store, graph)
            evaluation = engine.evaluate()
            prepared_gradient = engine.backward(evaluation) if config.steps > resume.step else None
            cumulative_loss = resume.cumulative_loss
            last_loss = resume.last_loss
            checkpoint_hash = resume.checkpoint_hash
            _publish_from_state(
                publisher,
                step=resume.step,
                groups=resume.groups,
                last_loss=resume.last_loss,
                cumulative_loss=resume.cumulative_loss,
                graph_version=resume.graph_version,
                cache_hashes=resume.cache_hashes,
                checkpoint_hash=resume.checkpoint_hash,
            )
            start_step = resume.step
            resumed_from = resume.step

        canceled = False
        steps_executed = 0
        current_step = start_step
        optimizer_state: Any = None
        try:
            for step in range(start_step + 1, config.steps + 1):
                if config.cancel_event is not None and config.cancel_event.is_set():
                    canceled = True
                    break
                modified_groups = _schedule_groups(
                    config.schedule, store.group_order, step
                )
                if prepared_gradient is None:
                    raise RuntimeError("missing prepared gradient at step boundary")
                gradient_norm = prepared_gradient.gradient_norm
                optimizer_state = {"mode": "stateless-sgd", "step": step}
                for group_id in modified_groups:
                    store.optimizer_step(group_id, float(config.learning_rate))
                if config.step_hook is not None:
                    config.step_hook(step, store)
                evaluation = engine.evaluate()
                prepared_gradient = engine.backward(evaluation)
                step_loss = float(evaluation.loss.detach().cpu().item())
                cumulative_loss += step_loss
                checkpoint_hash = manager.commit(
                    step=step,
                    groups=store.checkpoint_groups(),
                    cumulative_loss=cumulative_loss,
                    last_loss=step_loss,
                    graph_version=graph.version,
                    cache_hashes=evaluation.cache_hashes,
                )
                _trace_evaluation(
                    writer,
                    step=step,
                    modified_groups=modified_groups,
                    evaluation=evaluation,
                    loss=step_loss,
                    gradient_norm=gradient_norm,
                )
                _publish_from_state(
                    publisher,
                    step=step,
                    groups=store.checkpoint_groups(),
                    last_loss=step_loss,
                    cumulative_loss=cumulative_loss,
                    graph_version=graph.version,
                    cache_hashes=evaluation.cache_hashes,
                    checkpoint_hash=checkpoint_hash,
                )
                last_loss = step_loss
                current_step = step
                steps_executed += 1
                optimizer_state = None
                if config.crash_after is not None and step >= config.crash_after:
                    os._exit(137)
                if config.cancel_event is not None and config.cancel_event.is_set():
                    canceled = True
                    break
        finally:
            optimizer_state = None

        status = "cancelled" if canceled else "completed"
        return FitResult(
            status=status,
            resumed_from=resumed_from,
            completed_step=current_step,
            last_loss=last_loss,
            cumulative_loss=cumulative_loss,
            checkpoint_hash=checkpoint_hash,
            graph_version=graph.version,
            cache_hashes=OrderedDict(evaluation.cache_hashes),
            trace_path=str(trace_path),
            state_dir=str(state_path),
            steps_executed=steps_executed,
        )


class FitService:
    """Background fit plus lock-free snapshot queries."""

    _active: dict[Path, "FitService"] = {}
    _active_lock = threading.Lock()

    def __init__(
        self,
        scene_path: str | Path,
        group_path: str | Path | None,
        state_dir: str | Path,
    ) -> None:
        self.scene_path = Path(scene_path).resolve()
        self.group_path = Path(group_path).resolve() if group_path is not None else None
        self.state_dir = Path(state_dir).resolve()
        self.publisher = SnapshotPublisher()
        self._cancel = threading.Event()
        self._thread: threading.Thread | None = None
        self._result: FitResult | None = None
        self._error: BaseException | None = None
        self._lock_path: Path | None = None

    def start(self, config: FitConfig | None = None) -> "FitService":
        config = config or FitConfig()
        scene = load_scene(self.scene_path)
        scene_hash = scene.scene_hash()
        lock_path = fit_lock_path(scene_hash)
        with self._active_lock:
            if lock_path in self._active:
                raise ToolError(
                    ErrorCode.E_BUSY,
                    "a fit task is already active for this scene",
                    details={"lock": str(lock_path)},
                )
            self._active[lock_path] = self
        self._lock_path = lock_path
        config.cancel_event = self._cancel

        def target() -> None:
            try:
                self._result = run_fit(
                    self.scene_path,
                    self.group_path,
                    self.state_dir,
                    config,
                    publisher=self.publisher,
                )
            except BaseException as exc:
                self._error = exc
            finally:
                with self._active_lock:
                    self._active.pop(lock_path, None)

        self._thread = threading.Thread(target=target, name="inverse-render-fit")
        self._thread.start()
        return self

    def cancel(self) -> None:
        self._cancel.set()

    def query(self, handle: str | int | None = None) -> Snapshot:
        return self.publisher.query(handle)

    def wait(self, timeout: float | None = None) -> FitResult:
        if self._thread is None:
            raise ToolError(ErrorCode.E_CONFIG_INVALID, "fit service has not been started")
        self._thread.join(timeout)
        if self._thread.is_alive():
            raise TimeoutError("fit task is still running")
        if self._error is not None:
            raise self._error
        if self._result is None:
            raise RuntimeError("fit thread ended without result")
        return self._result

