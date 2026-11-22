"""Stateful windowed compositing session with incremental invalidation."""

from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field, replace
from typing import Any, Callable, Mapping

import numpy as np

from .checkpoint import CheckpointStore, RecoveryResult
from .diff import DiffReport, assert_frames_equal, difference_heatmap, first_difference
from .errors import E_DIFF, E_PARA, E_STATE, PipelineError
from .frame import FrameResult
from .model import Layer, Rect, SceneModel
from .renderer import render_all_frames, render_frame
from .state import WindowSlot, WindowState, transition
from .version import ALGORITHM_VERSION

MAX_PARALLELISM = 8
MAX_FRAME_BUFFERS_PER_THREAD = 4


@dataclass(frozen=True)
class InvalidationRecord:
    window_index: int | None
    layer_ids: tuple[str, ...]
    reason: str
    stage: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "window_index": self.window_index,
            "layer_ids": list(self.layer_ids),
            "reason": self.reason,
            "stage": self.stage,
        }


@dataclass
class EditResult:
    model: SceneModel
    invalidations: tuple[InvalidationRecord, ...]
    export_invalidated: bool


@dataclass
class RecomputeResult:
    frames: dict[int, FrameResult]
    reused_windows: tuple[int, ...]
    recomputed_windows: tuple[int, ...]
    failed_windows: tuple[int, ...]
    commit_order: tuple[int, ...]
    committed_count: int
    total_windows: int
    invalidations: tuple[InvalidationRecord, ...] = ()
    errors: dict[int, dict[str, Any]] = field(default_factory=dict)

    @property
    def progress_label(self) -> str:
        return f"{self.committed_count}/{self.total_windows}"


@dataclass
class FullComparison:
    ok: bool
    report: DiffReport | None
    heatmaps: dict[int, np.ndarray]
    full_frames: dict[int, FrameResult]


class _FrameBufferGate:
    def __init__(self, limit: int) -> None:
        self.limit = limit
        self._local = threading.local()

    def acquire(self) -> None:
        semaphore = getattr(self._local, "semaphore", None)
        if semaphore is None:
            semaphore = threading.BoundedSemaphore(self.limit)
            self._local.semaphore = semaphore
        semaphore.acquire()

    def release(self) -> None:
        self._local.semaphore.release()


@dataclass
class _WindowOutcome:
    index: int
    frames: tuple[FrameResult, ...] | None = None
    snapshot: dict[str, Any] | None = None
    error: dict[str, Any] | None = None


class CompositorSession:
    def __init__(
        self,
        model: SceneModel,
        checkpoint_store: CheckpointStore,
        *,
        max_workers: int = 2,
        max_frame_buffers_per_thread: int = 1,
    ) -> None:
        model.validate()
        if max_workers < 1 or max_workers > MAX_PARALLELISM:
            raise PipelineError(
                E_PARA,
                f"max_workers must be within [1, {MAX_PARALLELISM}]",
                field_path="max_workers",
            )
        if (
            max_frame_buffers_per_thread < 1
            or max_frame_buffers_per_thread > MAX_FRAME_BUFFERS_PER_THREAD
        ):
            raise PipelineError(
                E_PARA,
                "max_frame_buffers_per_thread is outside the supported range",
                field_path="max_frame_buffers_per_thread",
            )
        self.model = model
        self.store = checkpoint_store
        self.max_workers = max_workers
        self.max_frame_buffers_per_thread = max_frame_buffers_per_thread
        self._lock = threading.RLock()
        self._run_lock = threading.Lock()
        self._io_lock = threading.Lock()
        self._windows: dict[int, WindowSlot] = {
            index: WindowSlot(index=index, start=start, end_exclusive=end)
            for index, start, end in model.window_ranges()
        }
        self._identity = {
            "algorithm_version": ALGORITHM_VERSION,
            "source_version": model.source_version,
            "model_fingerprint": model.model_fingerprint,
            "scene_id": model.scene_id,
        }
        self.pending_invalidations: list[InvalidationRecord] = []
        self.export_dirty = False
        self.last_recompute: RecomputeResult | None = None
        self.recovery = self._recover()

    def _recover(self) -> RecoveryResult:
        recovery = self.store.load(self._identity)
        if recovery.discarded:
            self.store.discard()
            return recovery
        if not recovery.restored:
            return recovery
        ranges = self.model.window_ranges()
        mismatch_reasons = []
        for index, restored in recovery.restored.items():
            if index < 0 or index >= len(ranges):
                mismatch_reasons.append(f"unknown window index {index}")
                continue
            _, start, end = ranges[index]
            if restored.start != start or restored.end_exclusive != end:
                mismatch_reasons.append(f"window {index} range mismatch")
                continue
            expected_snapshot = self.model.window_snapshot(index)
            if restored.snapshot != expected_snapshot:
                mismatch_reasons.append(f"window {index} snapshot mismatch")
        if mismatch_reasons:
            self.store.discard()
            return RecoveryResult({}, True, tuple(mismatch_reasons))
        for index, restored in recovery.restored.items():
            slot = self._windows[index]
            slot.state = WindowState.COMMITTED
            slot.snapshot = restored.snapshot
            slot.frames = restored.frames
            slot.generation = 1
        return recovery

    @property
    def windows(self) -> tuple[WindowSlot, ...]:
        with self._lock:
            return tuple(self._windows[index] for index in sorted(self._windows))

    @property
    def committed_count(self) -> int:
        with self._lock:
            return sum(
                slot.state == WindowState.COMMITTED
                for slot in self._windows.values()
            )

    @property
    def total_windows(self) -> int:
        return len(self._windows)

    @property
    def progress(self) -> tuple[int, int]:
        return self.committed_count, self.total_windows

    def current_frames(self) -> dict[int, FrameResult]:
        with self._lock:
            frames: dict[int, FrameResult] = {}
            for slot in self._windows.values():
                if slot.state == WindowState.COMMITTED:
                    for frame in slot.frames:
                        frames[frame.frame_index] = frame
            return frames

    def frames_for_window(self, window_index: int) -> tuple[FrameResult, ...]:
        with self._lock:
            slot = self._get_slot(window_index)
            return slot.frames if slot.state == WindowState.COMMITTED else ()

    def _get_slot(self, window_index: int) -> WindowSlot:
        try:
            return self._windows[window_index]
        except KeyError as exc:
            raise PipelineError(
                E_PARA,
                "window_index is outside the timeline",
                field_path="window_index",
            ) from exc

    def _ensure_no_computing(self) -> None:
        for slot in self._windows.values():
            if slot.state == WindowState.COMPUTING:
                raise PipelineError(
                    E_STATE,
                    "Cannot edit while windows are computing",
                    field_path=f"windows[{slot.index}].state",
                    window_index=slot.index,
                    current=slot.state.value,
                )

    def apply_edits(
        self,
        layer_id: str,
        changes: Mapping[str, Any],
    ) -> EditResult:
        with self._lock:
            self._ensure_no_computing()
            old_model = self.model
            layer = old_model.layer(layer_id)
            validated = self._validate_layer_changes(layer, changes)
            new_model = old_model.replace_layer(layer_id, **validated)
            new_model.validate()
            reason = "layer_parameter:" + ",".join(sorted(changes))
            invalidations = self._invalidate_model_change(
                old_model, new_model, reason
            )
            self.model = new_model
            self.pending_invalidations.extend(invalidations)
            self.export_dirty = True
            return EditResult(
                model=self.model,
                invalidations=tuple(invalidations),
                export_invalidated=True,
            )

    def apply_edit(
        self,
        layer_id: str,
        field_name: str,
        value: Any,
    ) -> EditResult:
        return self.apply_edits(layer_id, {field_name: value})

    def set_frame_rate(self, numerator: int, denominator: int) -> EditResult:
        with self._lock:
            self._ensure_no_computing()
            if (
                isinstance(numerator, bool)
                or not isinstance(numerator, int)
                or numerator <= 0
            ):
                raise PipelineError(
                    E_PARA,
                    "Frame-rate numerator must be a positive integer",
                    field_path="fps.numerator",
                )
            if (
                isinstance(denominator, bool)
                or not isinstance(denominator, int)
                or denominator <= 0
            ):
                raise PipelineError(
                    E_PARA,
                    "Frame-rate denominator must be a positive integer",
                    field_path="fps.denominator",
                )
            old_model = self.model
            new_model = old_model.replace_timeline(
                fps_num=numerator, fps_den=denominator
            )
            new_model.validate()
            invalidations = self._invalidate_model_change(
                old_model, new_model, "timebase:fps"
            )
            self.model = new_model
            self.pending_invalidations.extend(invalidations)
            self.export_dirty = True
            return EditResult(
                model=new_model,
                invalidations=tuple(invalidations),
                export_invalidated=True,
            )

    def _validate_layer_changes(
        self,
        layer: Layer,
        changes: Mapping[str, Any],
    ) -> dict[str, Any]:
        validated: dict[str, Any] = {}
        allowed = {
            "opacity",
            "alpha",
            "color_linear",
            "visible",
            "visible_from",
            "visible_to",
            "bounds",
            "parent_id",
            "name",
            "kind",
        }
        for field_name, value in changes.items():
            if field_name not in allowed:
                raise PipelineError(
                    E_PARA,
                    f"Unsupported editable field {field_name!r}",
                    field_path=field_name,
                )
            if field_name in {"opacity", "alpha"}:
                if isinstance(value, bool) or not isinstance(value, (int, float)):
                    raise PipelineError(
                        E_PARA,
                        f"{field_name} must be numeric",
                        field_path=field_name,
                    )
                number = float(value)
                if not 0.0 <= number <= 1.0:
                    raise PipelineError(
                        E_PARA,
                        f"{field_name} must be within [0, 1]",
                        field_path=field_name,
                    )
                validated[field_name] = number
            elif field_name == "color_linear":
                if (
                    not isinstance(value, (list, tuple))
                    or len(value) != 3
                    or any(
                        isinstance(channel, bool)
                        or not isinstance(channel, (int, float))
                        for channel in value
                    )
                ):
                    raise PipelineError(
                        E_PARA,
                        "color_linear must contain three numeric channels",
                        field_path=field_name,
                    )
                color = tuple(float(channel) for channel in value)
                if any(not 0.0 <= channel <= 1.0 for channel in color):
                    raise PipelineError(
                        E_PARA,
                        "Linear RGB channels must be within [0, 1]",
                        field_path=field_name,
                    )
                validated[field_name] = color
            elif field_name == "visible":
                if not isinstance(value, bool):
                    raise PipelineError(
                        E_PARA,
                        "visible must be boolean",
                        field_path=field_name,
                    )
                validated[field_name] = value
            elif field_name in {"visible_from", "visible_to"}:
                if isinstance(value, bool) or not isinstance(value, int):
                    raise PipelineError(
                        E_PARA,
                        f"{field_name} must be an integer",
                        field_path=field_name,
                    )
                if value < 0 or value > self.model.timeline.frames:
                    raise PipelineError(
                        E_PARA,
                        f"{field_name} is outside the timeline",
                        field_path=field_name,
                    )
                validated[field_name] = value
            elif field_name == "bounds":
                validated[field_name] = self._validate_bounds(value)
            elif field_name == "parent_id":
                if value is not None and not isinstance(value, str):
                    raise PipelineError(
                        E_PARA,
                        "parent_id must be a string or null",
                        field_path=field_name,
                    )
                validated[field_name] = value
            elif field_name in {"name", "kind"}:
                if not isinstance(value, str) or not value:
                    raise PipelineError(
                        E_PARA,
                        f"{field_name} must be a non-empty string",
                        field_path=field_name,
                    )
                validated[field_name] = value
        if layer.visible_from > validated.get("visible_to", layer.visible_to):
            raise PipelineError(
                E_PARA,
                "visible_to must not precede visible_from",
                field_path="visible_to",
            )
        if layer.visible_to < validated.get("visible_from", layer.visible_from):
            raise PipelineError(
                E_PARA,
                "visible_from must not exceed visible_to",
                field_path="visible_from",
            )
        return validated

    @staticmethod
    def _validate_bounds(value: Any) -> Rect:
        if isinstance(value, Rect):
            return value
        if not isinstance(value, Mapping):
            raise PipelineError(
                E_PARA,
                "bounds must be a mapping",
                field_path="bounds",
            )
        keys = ("x", "y", "width", "height")
        values = []
        for key in keys:
            if key not in value:
                raise PipelineError(
                    E_PARA,
                    f"bounds.{key} is required",
                    field_path=f"bounds.{key}",
                )
            item = value[key]
            if isinstance(item, bool) or not isinstance(item, int):
                raise PipelineError(
                    E_PARA,
                    f"bounds.{key} must be an integer",
                    field_path=f"bounds.{key}",
                )
            values.append(item)
        if values[2] < 0 or values[3] < 0:
            raise PipelineError(
                E_PARA,
                "bounds width and height must be non-negative",
                field_path="bounds",
            )
        return Rect(*values)

    def _invalidate_model_change(
        self,
        old_model: SceneModel,
        new_model: SceneModel,
        reason: str,
    ) -> list[InvalidationRecord]:
        records: list[InvalidationRecord] = []
        changed_union: set[str] = set()
        all_ids = tuple(layer.layer_id for layer in old_model.layers)
        for index, start, end in old_model.window_ranges():
            old_snapshot = old_model.window_snapshot(index)
            new_snapshot = new_model.window_snapshot(index)
            if old_snapshot == new_snapshot:
                continue
            changed_ids = self._changed_layer_ids(old_snapshot, new_snapshot)
            if not changed_ids and old_snapshot.get("timeline") != new_snapshot.get(
                "timeline"
            ):
                changed_ids = set(all_ids)
            changed_union.update(changed_ids)
            slot = self._windows[index]
            if slot.state in {WindowState.COMMITTED, WindowState.FAILED}:
                transition(slot, WindowState.PENDING, allow_invalidation=True)
            slot.invalidation_reason = reason
            records.append(
                InvalidationRecord(
                    window_index=index,
                    layer_ids=tuple(sorted(changed_ids)),
                    reason=reason,
                    stage="composite",
                )
            )
        records.append(
            InvalidationRecord(
                window_index=None,
                layer_ids=tuple(sorted(changed_union)),
                reason=reason,
                stage="export",
            )
        )
        return records

    @staticmethod
    def _changed_layer_ids(
        old_snapshot: dict[str, Any],
        new_snapshot: dict[str, Any],
    ) -> set[str]:
        old_layers = {
            layer["layer_id"]: layer for layer in old_snapshot.get("layers", [])
        }
        new_layers = {
            layer["layer_id"]: layer for layer in new_snapshot.get("layers", [])
        }
        changed = set(old_layers) ^ set(new_layers)
        for layer_id in set(old_layers) & set(new_layers):
            if old_layers[layer_id] != new_layers[layer_id]:
                changed.add(layer_id)
        return changed

    def run(
        self,
        *,
        parallel: bool = True,
        max_workers: int | None = None,
        frame_hook: Callable[[int], None] | None = None,
        full: bool = False,
    ) -> RecomputeResult:
        if not self._run_lock.acquire(blocking=False):
            raise PipelineError(
                E_STATE,
                "A recomputation is already running",
                field_path="session",
            )
        try:
            workers = self.max_workers if max_workers is None else max_workers
            if workers < 1 or workers > MAX_PARALLELISM:
                raise PipelineError(
                    E_PARA,
                    f"max_workers must be within [1, {MAX_PARALLELISM}]",
                    field_path="max_workers",
                )
            with self._lock:
                self._ensure_no_computing()
                if full:
                    with self._io_lock:
                        self.store.discard()
                    for slot in self._windows.values():
                        if slot.state == WindowState.COMMITTED:
                            transition(
                                slot,
                                WindowState.PENDING,
                                allow_invalidation=True,
                            )
                            slot.invalidation_reason = "full_recompute"
                selected = [
                    slot.index
                    for slot in sorted(
                        self._windows.values(), key=lambda item: item.index
                    )
                    if slot.state in {WindowState.PENDING, WindowState.FAILED}
                ]
                for index in selected:
                    transition(self._windows[index], WindowState.COMPUTING)
                model = self.model
                snapshots = {
                    index: model.window_snapshot(index) for index in selected
                }
                invalidations = tuple(self.pending_invalidations)
                self.pending_invalidations.clear()

            outcomes: dict[int, _WindowOutcome] = {}
            gate = _FrameBufferGate(self.max_frame_buffers_per_thread)

            def compute(index: int) -> _WindowOutcome:
                try:
                    _, start, end = model.window_ranges()[index]
                    frames = []
                    for frame_index in range(start, end):
                        if frame_hook is not None:
                            frame_hook(frame_index)
                        gate.acquire()
                        try:
                            frames.append(render_frame(model, frame_index))
                        finally:
                            gate.release()
                    return _WindowOutcome(
                        index=index,
                        frames=tuple(frames),
                        snapshot=snapshots[index],
                    )
                except Exception as exc:
                    if isinstance(exc, PipelineError):
                        error = exc.to_dict()
                    else:
                        error = {
                            "code": "E_INTERNAL",
                            "message": f"{type(exc).__name__}: {exc}",
                        }
                    return _WindowOutcome(index=index, error=error)

            if parallel and selected:
                with ThreadPoolExecutor(max_workers=workers) as executor:
                    futures: dict[int, Future[_WindowOutcome]] = {
                        index: executor.submit(compute, index)
                        for index in selected
                    }
                    for index in selected:
                        outcomes[index] = futures[index].result()
            else:
                for index in selected:
                    outcomes[index] = compute(index)

            commit_order: list[int] = []
            recomputed: list[int] = []
            failed: list[int] = []
            errors: dict[int, dict[str, Any]] = {}
            with self._lock:
                for index in selected:
                    outcome = outcomes[index]
                    slot = self._windows[index]
                    if outcome.error is not None:
                        transition(slot, WindowState.FAILED)
                        slot.last_error = outcome.error
                        failed.append(index)
                        errors[index] = outcome.error
                        continue
                    assert outcome.frames is not None
                    assert outcome.snapshot is not None
                    try:
                        with self._io_lock:
                            self.store.commit_window(
                                identity=self._identity,
                                index=index,
                                start=slot.start,
                                end_exclusive=slot.end_exclusive,
                                snapshot=outcome.snapshot,
                                frames=outcome.frames,
                                source_version=model.source_version,
                            )
                    except Exception as exc:
                        error = {
                            "code": "E_IO",
                            "message": f"Checkpoint commit failed: {exc}",
                        }
                        transition(slot, WindowState.FAILED)
                        slot.last_error = error
                        failed.append(index)
                        errors[index] = error
                        continue
                    transition(slot, WindowState.COMMITTED)
                    slot.frames = outcome.frames
                    slot.snapshot = outcome.snapshot
                    slot.invalidation_reason = None
                    commit_order.append(index)
                    recomputed.append(index)

                reused = tuple(
                    slot.index
                    for slot in sorted(
                        self._windows.values(), key=lambda item: item.index
                    )
                    if slot.state == WindowState.COMMITTED
                    and slot.index not in recomputed
                )
                committed_count = sum(
                    slot.state == WindowState.COMMITTED
                    for slot in self._windows.values()
                )
                result = RecomputeResult(
                    frames=self.current_frames(),
                    reused_windows=reused,
                    recomputed_windows=tuple(recomputed),
                    failed_windows=tuple(failed),
                    commit_order=tuple(commit_order),
                    committed_count=committed_count,
                    total_windows=len(self._windows),
                    invalidations=invalidations,
                    errors=errors,
                )
                self.last_recompute = result
                return result
        finally:
            self._run_lock.release()

    def compare_with_full(
        self,
        window_index: int | None = None,
        *,
        raise_on_diff: bool = True,
    ) -> FullComparison:
        with self._lock:
            if window_index is None:
                selected = list(self._windows)
            else:
                self._get_slot(window_index)
                selected = [window_index]
            for index in selected:
                if self._windows[index].state != WindowState.COMMITTED:
                    raise PipelineError(
                        E_STATE,
                        "Only committed windows can be verified",
                        field_path=f"windows[{index}].state",
                        window_index=index,
                        current=self._windows[index].state.value,
                    )
            model = self.model
        full_frames = render_all_frames(model)
        actual: dict[int, FrameResult] = {}
        expected: dict[int, FrameResult] = {}
        for index in selected:
            _, start, end = model.window_ranges()[index]
            for frame in self.frames_for_window(index):
                actual[frame.frame_index] = frame
            for frame_index in range(start, end):
                expected[frame_index] = full_frames[frame_index]
        report = first_difference(actual, expected, model)
        heatmaps: dict[int, np.ndarray] = {}
        for index in selected:
            _, start, end = model.window_ranges()[index]
            for frame_index in range(start, end):
                if frame_index in actual and frame_index in expected:
                    heatmaps[frame_index] = difference_heatmap(
                        actual[frame_index], expected[frame_index]
                    )
        if report is not None and raise_on_diff:
            raise PipelineError(
                E_DIFF,
                report.message,
                field_path="frames",
                details=report.to_dict(),
            )
        return FullComparison(
            ok=report is None,
            report=report,
            heatmaps=heatmaps,
            full_frames=full_frames,
        )

    def assert_exact_against_full(self) -> None:
        current = self.current_frames()
        full = render_all_frames(self.model)
        assert_frames_equal(current, full, self.model)

    def mark_exported(self) -> None:
        with self._lock:
            self.export_dirty = False
