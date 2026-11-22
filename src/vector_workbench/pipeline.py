from __future__ import annotations

import io
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

import numpy as np

from . import ALGORITHM_VERSION
from .checkpoint import CheckpointStore, SnapshotLoad
from .errors import ErrorCode, WorkbenchError
from .geometry import (
    find_self_intersection,
    flatten_path,
    polygon_area,
)
from .model import (
    CellStatus,
    Injection,
    RenderParams,
    RunResult,
    Scene,
    Stage,
    VectorPath,
)
from .raster import (
    canvas_size,
    composite_coverage,
    empty_canvas,
    path_coverage,
)
from .state import PathStateMachine

_INJECTION_ALIASES = {
    "self": "self_intersection",
    "self_intersection": "self_intersection",
    "degenerate": "degenerate",
    "zero_area": "degenerate",
    "clip": "clip",
    "stroke_overflow": "clip",
    "stage": "stage_failure",
    "stage_failure": "stage_failure",
}


@dataclass(frozen=True)
class PipelineEvent:
    event: str
    path_id: str
    stage: str
    status: str
    committed: int
    total: int
    error: dict[str, Any] | None = None


class RunControl:
    """Cooperative boundary control for pause, recoverable cancel, and hard stop."""

    def __init__(
        self,
        *,
        pause: bool = False,
        cancel: bool = False,
        terminate: bool = False,
        cancel_after_commits: int | None = None,
        terminate_after_commits: int | None = None,
    ) -> None:
        self._lock = threading.Lock()
        self._pause = pause
        self._cancel = cancel
        self._terminate = terminate
        self._cancel_after = cancel_after_commits
        self._terminate_after = terminate_after_commits

    def request_pause(self) -> None:
        with self._lock:
            self._pause = True

    def request_cancel(self) -> None:
        with self._lock:
            self._cancel = True

    def request_terminate(self) -> None:
        with self._lock:
            self._terminate = True

    def clear_pause(self) -> None:
        with self._lock:
            self._pause = False

    def clear_stop(self) -> None:
        with self._lock:
            self._cancel = False
            self._terminate = False
            self._cancel_after = None
            self._terminate_after = None

    def action(self, committed: int) -> str | None:
        with self._lock:
            if self._terminate or (
                self._terminate_after is not None and committed >= self._terminate_after
            ):
                return "terminate"
            if self._cancel or (
                self._cancel_after is not None and committed >= self._cancel_after
            ):
                return "cancel"
            if self._pause:
                return "pause"
        return None


@dataclass
class BaseComputation:
    flattened: np.ndarray | None = None
    coverage: np.ndarray | None = None
    errors: dict[Stage, WorkbenchError] = field(default_factory=dict)


def _array_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    return buffer.getvalue()


def _bytes_array(payload: bytes) -> np.ndarray:
    return np.load(io.BytesIO(payload), allow_pickle=False)


def _normalize_injection(value: Injection | Mapping[str, Any] | None) -> Injection | None:
    if value is None:
        return None
    if isinstance(value, Injection):
        kind = value.kind
        stage = value.stage
    elif isinstance(value, Mapping):
        kind = str(value.get("kind", ""))
        stage_raw = value.get("stage")
        stage = Stage(str(stage_raw)) if stage_raw is not None else None
    else:
        raise WorkbenchError(ErrorCode.E_PARA, "invalid injection specification")
    normalized = _INJECTION_ALIASES.get(kind)
    if normalized is None:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"unknown injection kind {kind!r}",
            field="injection",
        )
    if normalized == "stage_failure" and stage is None:
        stage = Stage.COVERAGE
    return Injection(kind=normalized, stage=stage)


class Pipeline:
    """Deterministic path-major flatten -> coverage -> compose executor."""

    def __init__(
        self,
        scene: Scene,
        params: RenderParams,
        checkpoint_root: str | Path,
        *,
        injections: Mapping[str, Injection | Mapping[str, Any] | None] | None = None,
        control: RunControl | None = None,
        event_callback: Callable[[PipelineEvent], None] | None = None,
    ) -> None:
        params.validate()
        self.scene = scene
        self.params = params
        self.control = control or RunControl()
        self.event_callback = event_callback
        self.states = PathStateMachine(scene.paths)
        self._injections: dict[str, Injection | None] = {
            path_id: _normalize_injection(value)
            for path_id, value in (injections or {}).items()
        }
        for path_id in self._injections:
            if path_id not in scene.path_map():
                raise WorkbenchError(
                    ErrorCode.E_PARA,
                    "injection target path does not exist",
                    path_id=path_id,
                    field="injections",
                )
        self.expected_order = [
            (path.id, stage.value) for path in scene.paths for stage in Stage.ordered()
        ]
        self.store = CheckpointStore(
            checkpoint_root,
            scene_id=scene.scene_id,
            scene_hash=scene.canonical_hash(),
            param_hash=params.fingerprint(),
            algorithm_version=ALGORITHM_VERSION,
            expected_order=[(path_id, Stage(stage)) for path_id, stage in self.expected_order],
            parameter_details=params.version_dict(),
        )
        self.canvas = empty_canvas(scene, params)
        self.flatten_cache: dict[str, np.ndarray] = {}
        self.coverage_cache: dict[str, np.ndarray] = {}
        self.compose_stages: dict[str, np.ndarray] = {}
        self.commit_sequence: list[tuple[str, str]] = []
        self.invalidated_snapshot = False
        self.invalidation_reason: str | None = None
        self._lock = threading.RLock()
        self._load_snapshot()

    @property
    def progress_total(self) -> int:
        return self.states.total_cells

    @property
    def progress_committed(self) -> int:
        return self.states.committed_cells

    @property
    def image_size(self) -> tuple[int, int]:
        return canvas_size(self.scene, self.params)

    def set_injections(
        self, injections: Mapping[str, Injection | Mapping[str, Any] | None]
    ) -> None:
        normalized = {
            path_id: _normalize_injection(value)
            for path_id, value in injections.items()
        }
        for path_id in normalized:
            if path_id not in self.scene.path_map():
                raise WorkbenchError(
                    ErrorCode.E_PARA,
                    "injection target path does not exist",
                    path_id=path_id,
                    field="injections",
                )
        self._injections = normalized

    def effective_injection(self, path: VectorPath) -> Injection | None:
        if path.id in self._injections:
            return self._injections[path.id]
        return path.injection

    def _decode_entry(self, path_id: str, stage: Stage, payload: bytes) -> np.ndarray:
        try:
            array = _bytes_array(payload)
        except (OSError, ValueError) as exc:
            raise WorkbenchError(
                ErrorCode.E_STAGE,
                f"checkpoint payload is not a valid NumPy array: {exc}",
                path_id=path_id,
                stage=stage.value,
            ) from exc
        width, height = self.image_size
        if stage is Stage.FLATTEN:
            if array.ndim != 2 or array.shape[1] != 2 or array.dtype != np.float64:
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    "flatten checkpoint has an invalid shape or dtype",
                    path_id=path_id,
                    stage=stage.value,
                )
        elif stage is Stage.COVERAGE:
            if array.shape != (height, width) or array.dtype != np.float32:
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    "coverage checkpoint dimensions do not match the canvas",
                    path_id=path_id,
                    stage=stage.value,
                )
        elif stage is Stage.COMPOSE:
            if array.shape != (height, width, 4) or array.dtype != np.float32:
                raise WorkbenchError(
                    ErrorCode.E_STAGE,
                    "compose checkpoint dimensions do not match the canvas",
                    path_id=path_id,
                    stage=stage.value,
                )
        if not np.isfinite(array).all():
            raise WorkbenchError(
                ErrorCode.E_STAGE,
                "checkpoint contains non-finite values",
                path_id=path_id,
                stage=stage.value,
            )
        return array

    def _load_snapshot(self) -> None:
        loaded: SnapshotLoad = self.store.load()
        self.invalidated_snapshot = loaded.invalidated
        self.invalidation_reason = loaded.reason
        if not loaded.entries:
            return
        tokens = loaded.tokens()
        if tokens != self.expected_order[: len(tokens)]:
            self.store.reset("checkpoint is not a valid path-major prefix")
            self.invalidated_snapshot = True
            self.invalidation_reason = "checkpoint is not a valid path-major prefix"
            return
        try:
            for entry in loaded.entries:
                array = self._decode_entry(entry.path_id, entry.stage, entry.payload)
                if entry.stage is Stage.FLATTEN:
                    self.flatten_cache[entry.path_id] = array
                elif entry.stage is Stage.COVERAGE:
                    self.coverage_cache[entry.path_id] = array
                else:
                    self.canvas = array.copy()
                    self.compose_stages[entry.path_id] = array.copy()
                self.states.restore_committed(entry.path_id, entry.stage)
                self.commit_sequence.append((entry.path_id, entry.stage.value))
        except WorkbenchError as exc:
            self.store.reset(f"semantic checkpoint validation failed: {exc.message}")
            self.states.reset()
            self.flatten_cache.clear()
            self.coverage_cache.clear()
            self.compose_stages.clear()
            self.canvas = empty_canvas(self.scene, self.params)
            self.commit_sequence.clear()
            self.invalidated_snapshot = True
            self.invalidation_reason = f"semantic checkpoint validation failed: {exc.message}"

    def _reset_runtime(self) -> None:
        self.states.reset()
        self.canvas = empty_canvas(self.scene, self.params)
        self.flatten_cache.clear()
        self.coverage_cache.clear()
        self.compose_stages.clear()
        self.commit_sequence.clear()

    def _event(
        self,
        event: str,
        path_id: str,
        stage: Stage,
        status: CellStatus,
        error: WorkbenchError | None = None,
    ) -> None:
        if self.event_callback is None:
            return
        self.event_callback(
            PipelineEvent(
                event=event,
                path_id=path_id,
                stage=stage.value,
                status=status.value,
                committed=self.states.committed_cells,
                total=self.states.total_cells,
                error=error.to_dict() if error else None,
            )
        )

    def _injection_error(self, path: VectorPath, stage: Stage) -> WorkbenchError | None:
        injection = self.effective_injection(path)
        if injection is None:
            return None
        if injection.kind == "self_intersection" and stage is Stage.FLATTEN:
            return WorkbenchError(
                ErrorCode.E_SELF,
                "self-intersecting contour injected by input or parameters",
                path_id=path.id,
                stage=stage.value,
                detail={"injection": "self_intersection"},
            )
        if injection.kind == "degenerate" and stage is Stage.COVERAGE:
            return WorkbenchError(
                ErrorCode.E_DEGEN,
                "zero-area closed loop injected by input or parameters",
                path_id=path.id,
                stage=stage.value,
                detail={"injection": "degenerate"},
            )
        if injection.kind == "clip" and stage is Stage.COVERAGE:
            return WorkbenchError(
                ErrorCode.E_CLIP,
                "stroke coverage overflow injected by input or parameters",
                path_id=path.id,
                stage=stage.value,
                detail={"injection": "clip"},
            )
        if injection.kind == "stage_failure" and injection.stage is stage:
            return WorkbenchError(
                ErrorCode.E_STAGE,
                "single-stage internal failure injected by input or parameters",
                path_id=path.id,
                stage=stage.value,
                detail={"injection": "stage_failure", "target_stage": stage.value},
            )
        return None

    def _compute_flatten(self, path: VectorPath) -> np.ndarray:
        injected = self._injection_error(path, Stage.FLATTEN)
        if injected is not None:
            raise injected
        points = flatten_path(
            path, self.scene.coord_scale, self.params.scale, self.params.tolerance
        )
        scene_area = polygon_area(points) / (self.params.scale * self.params.scale)
        if scene_area > 1e-10:
            intersection = find_self_intersection(points)
            if intersection is not None:
                raise WorkbenchError(
                    ErrorCode.E_SELF,
                    "flattened contour self-intersects",
                    path_id=path.id,
                    stage=Stage.FLATTEN.value,
                    segment=intersection[0],
                    detail={"edge_pair": list(intersection)},
                )
        return points

    def _compute_coverage(self, path: VectorPath, flattened: np.ndarray) -> np.ndarray:
        injected = self._injection_error(path, Stage.COVERAGE)
        if injected is not None:
            raise injected
        return path_coverage(self.scene, path, flattened, self.params)

    def _compute_base(self, path: VectorPath) -> BaseComputation:
        result = BaseComputation()
        if self.states.status(path.id, Stage.FLATTEN) is CellStatus.COMMITTED:
            flattened = self.flatten_cache[path.id]
        else:
            try:
                flattened = self._compute_flatten(path)
                result.flattened = flattened
            except WorkbenchError as exc:
                result.errors[Stage.FLATTEN] = exc
                return result
            except Exception as exc:
                result.errors[Stage.FLATTEN] = WorkbenchError(
                    ErrorCode.E_STAGE,
                    f"flatten worker failed: {exc}",
                    path_id=path.id,
                    stage=Stage.FLATTEN.value,
                )
                return result
        if self.states.status(path.id, Stage.COVERAGE) is CellStatus.COMMITTED:
            return result
        try:
            result.coverage = self._compute_coverage(path, flattened)
        except WorkbenchError as exc:
            result.errors[Stage.COVERAGE] = exc
        except Exception as exc:
            result.errors[Stage.COVERAGE] = WorkbenchError(
                ErrorCode.E_STAGE,
                f"coverage worker failed: {exc}",
                path_id=path.id,
                stage=Stage.COVERAGE.value,
            )
        return result

    def _precompute(self) -> dict[str, BaseComputation]:
        needed = [
            path
            for path in self.scene.paths
            if self.states.status(path.id, Stage.FLATTEN) is not CellStatus.COMMITTED
            or self.states.status(path.id, Stage.COVERAGE) is not CellStatus.COMMITTED
        ]
        if not needed:
            return {}
        workers = max(1, min(self.params.max_parallelism, len(needed)))
        if workers == 1:
            return {path.id: self._compute_base(path) for path in needed}
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="vw-path") as pool:
            futures = {path.id: pool.submit(self._compute_base, path) for path in needed}
            return {path_id: future.result() for path_id, future in futures.items()}

    def _ensure_error_context(
        self, error: WorkbenchError, path: VectorPath, stage: Stage
    ) -> WorkbenchError:
        if error.path_id is None:
            error.path_id = path.id
        if error.stage is None:
            error.stage = stage.value
        return error

    def _fail_stage(
        self, path: VectorPath, stage: Stage, error: WorkbenchError
    ) -> WorkbenchError:
        error = self._ensure_error_context(error, path, stage)
        try:
            self.states.fail(path.id, stage, error)
        except WorkbenchError:
            pass
        self._event("failed", path.id, stage, CellStatus.FAILED, error)
        return error

    def _begin_stage(self, path: VectorPath, stage: Stage) -> WorkbenchError | None:
        try:
            self.states.begin(path.id, stage)
        except WorkbenchError as exc:
            return exc
        self._event("processing", path.id, stage, CellStatus.PROCESSING)
        return None

    def _commit_stage(
        self,
        path: VectorPath,
        stage: Stage,
        payload: bytes,
    ) -> None:
        try:
            self.store.commit(path.id, stage, payload)
        except WorkbenchError:
            raise
        except Exception as exc:
            raise WorkbenchError(
                ErrorCode.E_STAGE,
                f"checkpoint commit failed: {exc}",
                path_id=path.id,
                stage=stage.value,
            ) from exc
        self.states.commit(path.id, stage)
        self.commit_sequence.append((path.id, stage.value))
        self._event("committed", path.id, stage, CellStatus.COMMITTED)

    def _run_flatten(
        self, path: VectorPath, base: BaseComputation | None
    ) -> None:
        if self.states.status(path.id, Stage.FLATTEN) is CellStatus.COMMITTED:
            return
        error = self._begin_stage(path, Stage.FLATTEN)
        if error is not None:
            raise error
        if base is not None and Stage.FLATTEN in base.errors:
            raise self._fail_stage(path, Stage.FLATTEN, base.errors[Stage.FLATTEN])
        if base is None or base.flattened is None:
            try:
                flattened = self._compute_flatten(path)
            except WorkbenchError as exc:
                raise self._fail_stage(path, Stage.FLATTEN, exc)
        else:
            flattened = base.flattened
        try:
            self._commit_stage(path, Stage.FLATTEN, _array_bytes(flattened))
        except WorkbenchError as exc:
            raise self._fail_stage(path, Stage.FLATTEN, exc)
        self.flatten_cache[path.id] = flattened

    def _run_coverage(
        self, path: VectorPath, base: BaseComputation | None
    ) -> None:
        if self.states.status(path.id, Stage.COVERAGE) is CellStatus.COMMITTED:
            return
        error = self._begin_stage(path, Stage.COVERAGE)
        if error is not None:
            raise error
        if base is not None and Stage.COVERAGE in base.errors:
            raise self._fail_stage(path, Stage.COVERAGE, base.errors[Stage.COVERAGE])
        if base is None or base.coverage is None:
            flattened = self.flatten_cache[path.id]
            try:
                coverage = self._compute_coverage(path, flattened)
            except WorkbenchError as exc:
                raise self._fail_stage(path, Stage.COVERAGE, exc)
        else:
            coverage = base.coverage
        try:
            self._commit_stage(path, Stage.COVERAGE, _array_bytes(coverage))
        except WorkbenchError as exc:
            raise self._fail_stage(path, Stage.COVERAGE, exc)
        self.coverage_cache[path.id] = coverage

    def _run_compose(self, path: VectorPath) -> None:
        if self.states.status(path.id, Stage.COMPOSE) is CellStatus.COMMITTED:
            return
        error = self._begin_stage(path, Stage.COMPOSE)
        if error is not None:
            raise error
        injected = self._injection_error(path, Stage.COMPOSE)
        if injected is not None:
            raise self._fail_stage(path, Stage.COMPOSE, injected)
        try:
            coverage = self.coverage_cache[path.id]
            composed = composite_coverage(self.canvas, path, coverage)
            self._commit_stage(path, Stage.COMPOSE, _array_bytes(composed))
        except WorkbenchError as exc:
            raise self._fail_stage(path, Stage.COMPOSE, exc)
        except Exception as exc:
            wrapped = WorkbenchError(
                ErrorCode.E_STAGE,
                f"compose stage failed: {exc}",
                path_id=path.id,
                stage=Stage.COMPOSE.value,
            )
            raise self._fail_stage(path, Stage.COMPOSE, wrapped)
        self.canvas = composed
        self.compose_stages[path.id] = composed.copy()

    def _result(
        self,
        status: str,
        recoverable: bool,
        errors: Sequence[WorkbenchError] = (),
    ) -> RunResult:
        return RunResult(
            status=status,
            recoverable=recoverable,
            image=self.canvas.copy(),
            progress_committed=self.states.committed_cells,
            progress_total=self.states.total_cells,
            commit_sequence=list(self.commit_sequence),
            errors=[error.to_dict() for error in errors],
            invalidated_snapshot=self.invalidated_snapshot,
            invalidation_reason=self.invalidation_reason,
        )

    def _stop_result(self, action: str) -> RunResult:
        if action == "pause":
            return self._result("paused", True)
        if action == "cancel":
            return self._result("cancelled", True)
        return self._result("terminated", False)

    def run(self, mode: str = "incremental", *, raise_on_error: bool = True) -> RunResult:
        if mode not in {"full", "incremental", "resume"}:
            raise WorkbenchError(
                ErrorCode.E_PARA, f"unsupported run mode {mode!r}", field="mode"
            )
        if mode == "full":
            self.store.reset("explicit full recompute")
            self._reset_runtime()
            self.invalidated_snapshot = True
            self.invalidation_reason = "explicit full recompute"
        action = self.control.action(self.states.committed_cells)
        if action:
            return self._stop_result(action)
        base_results = self._precompute()
        failures: list[WorkbenchError] = []
        try:
            for path in self.scene.paths:
                for stage in Stage.ordered():
                    action = self.control.action(self.states.committed_cells)
                    if action:
                        return self._stop_result(action)
                    if self.states.status(path.id, stage) is CellStatus.COMMITTED:
                        continue
                    if stage is Stage.FLATTEN:
                        self._run_flatten(path, base_results.get(path.id))
                    elif stage is Stage.COVERAGE:
                        self._run_coverage(path, base_results.get(path.id))
                    else:
                        self._run_compose(path)
        except WorkbenchError as exc:
            failures.append(exc)
            result = self._result("failed", True, failures)
            if raise_on_error:
                raise
            return result
        if self.states.all_committed():
            return self._result("completed", True)
        return self._result("partial", True, failures)

    def run_path(
        self, path_id: str, *, raise_on_error: bool = False
    ) -> RunResult:
        path = self.scene.path_map().get(path_id)
        if path is None:
            raise WorkbenchError(
                ErrorCode.E_PARA, "unknown path id", path_id=path_id, field="path_id"
            )
        action = self.control.action(self.states.committed_cells)
        if action:
            return self._stop_result(action)
        base = self._compute_base(path)
        failures: list[WorkbenchError] = []
        try:
            if self.states.status(path.id, Stage.FLATTEN) is not CellStatus.COMMITTED:
                self._run_flatten(path, base)
            if self.states.status(path.id, Stage.COVERAGE) is not CellStatus.COMMITTED:
                self._run_coverage(path, base)
            if self.states.status(path.id, Stage.COMPOSE) is not CellStatus.COMMITTED:
                self._run_compose(path)
        except WorkbenchError as exc:
            failures.append(exc)
            result = self._result("failed", True, failures)
            if raise_on_error:
                raise
            return result
        if self.states.all_committed():
            return self._result("completed", True)
        return self._result("partial", True)

    def materialize_compose_stages(self) -> dict[str, np.ndarray]:
        stages: dict[str, np.ndarray] = {}
        canvas = np.zeros_like(self.canvas)
        for path in self.scene.paths:
            coverage = self.coverage_cache.get(path.id)
            if coverage is None:
                continue
            canvas = composite_coverage(canvas, path, coverage)
            stages[path.id] = canvas.copy()
        return stages

    def rows(self) -> list[dict[str, Any]]:
        return self.states.rows()

