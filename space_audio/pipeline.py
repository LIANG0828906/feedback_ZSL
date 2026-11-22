from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Sequence

import numpy as np

from .analysis import FrameResult, analyze_frame
from .bounds import ObservedErrors
from .checkpoint import CheckpointStore, SnapshotStatus
from .config import WorkbenchConfig
from .errors import ErrorCode, WorkbenchError, para_error
from .model import AcousticSample, validate_construction_order
from .render import RenderResult, render_audio
from .state import FrameState, FrameStateMachine

ProgressCallback = Callable[[int, int, FrameResult | None], None]


@dataclass
class RunResult:
    mode: str
    sample_id: str
    total_frames: int
    committed_frames: int
    frames: tuple[FrameResult, ...]
    state_counts: dict[str, int]
    completed: bool
    report_complete: bool
    input_hash: str
    algorithm_version: str
    checkpoint_path: str | None
    invalidated: bool = False
    stopped_reason: str | None = None
    frame_errors: tuple[WorkbenchError, ...] = ()
    render: RenderResult | None = None
    config: WorkbenchConfig | None = None
    elapsed_s: float = 0.0

    @property
    def output(self) -> np.ndarray | None:
        return None if self.render is None else self.render.output

    @property
    def commit_sequence(self) -> tuple[int, ...]:
        return tuple(frame.frame_index for frame in self.frames)

    def to_report(self, sample: AcousticSample | None = None) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "schema": "spatial-audio-report-v1",
            "mode": self.mode,
            "sample_id": self.sample_id,
            "total_frames": self.total_frames,
            "committed_frames": self.committed_frames,
            "completed": self.completed,
            "report_complete": self.report_complete,
            "input_hash": self.input_hash,
            "algorithm_version": self.algorithm_version,
            "checkpoint_path": self.checkpoint_path,
            "invalidated": self.invalidated,
            "stopped_reason": self.stopped_reason,
            "state_counts": self.state_counts,
            "commit_sequence": list(self.commit_sequence),
            "frame_errors": [error.to_dict() for error in self.frame_errors],
            "elapsed_s": self.elapsed_s,
            "config": None if self.config is None else self.config.to_dict(),
            "render": None if self.render is None else self.render.to_metadata(),
            "frames": [frame.to_dict() for frame in self.frames],
        }
        if sample is not None:
            payload["sample_metadata"] = {
                "sample_rate": sample.sample_rate,
                "channel_layout": sample.layout.to_dict(),
                "ir_shape": list(sample.ir.shape),
                "dry_shape": list(sample.dry.shape),
                "manifest_sha256": sample.manifest_sha256,
                "declared_truth": [
                    truth.to_dict() for truth in sample.declared_truth
                ],
            }
        return payload


@dataclass(frozen=True)
class ComparisonSummary:
    identical: bool
    compared_samples: int
    compared_frames: int
    first_mismatch_sample: int | None = None
    first_mismatch_frame: int | None = None


class ProcessingSession:
    """Frame lifecycle owner: estimate, freeze, checkpoint, retry."""

    def __init__(
        self,
        sample: AcousticSample,
        config: WorkbenchConfig,
        *,
        checkpoint: CheckpointStore | None = None,
        committed_results: Sequence[FrameResult] | None = None,
    ) -> None:
        config.validate()
        if sample.channel_count != sample.layout.count:
            raise WorkbenchError(
                ErrorCode.CHAN,
                "sample channel count does not match declared layout",
                field="ir",
                details={
                    "ir_channels": sample.channel_count,
                    "layout_channels": sample.layout.count,
                },
            )
        validate_construction_order(sample)
        self.sample = sample
        self.config = config
        self.checkpoint = checkpoint
        self._input_hash = sample.input_hash()
        self._total_frames = sample.frame_count(config.analysis)
        self._state = FrameStateMachine(
            self._total_frames,
            committed_results=committed_results,
        )
        self._results: dict[int, FrameResult] = {
            result.frame_index: result for result in self._state.committed_results
        }
        self._executor: ThreadPoolExecutor | None = None
        self._executor_workers = 0

    @property
    def state_machine(self) -> FrameStateMachine:
        return self._state

    @property
    def input_hash(self) -> str:
        return self._input_hash

    @property
    def total_frames(self) -> int:
        return self._total_frames

    @property
    def progress(self) -> tuple[int, int]:
        return self._state.progress

    @property
    def committed_results(self) -> tuple[FrameResult, ...]:
        return self._state.committed_results

    def _get_executor(self) -> ThreadPoolExecutor | None:
        workers = min(self.config.max_workers, self.sample.channel_count)
        if workers <= 1:
            return None
        if self._executor is None or self._executor_workers != workers:
            if self._executor is not None:
                self._executor.shutdown(wait=True, cancel_futures=False)
            self._executor = ThreadPoolExecutor(
                max_workers=workers,
                thread_name_prefix="frame-estimate",
            )
            self._executor_workers = workers
        return self._executor

    def _previous_cumulative(self, frame_index: int) -> ObservedErrors | None:
        for index in range(frame_index - 1, -1, -1):
            result = self._results.get(index)
            if result is not None:
                return result.cumulative_errors
        return None

    def estimate_frame(self, frame_index: int) -> FrameResult:
        existing = self._state.begin_estimate(frame_index)
        if existing is not None:
            return existing
        try:
            result = analyze_frame(
                self.sample,
                frame_index,
                self.config,
                previous_cumulative_errors=self._previous_cumulative(frame_index),
                executor=self._get_executor(),
            )
        except WorkbenchError as exc:
            self._state.mark_failed(frame_index, exc)
            self._results.pop(frame_index, None)
            raise
        self._state.mark_estimated(frame_index, result)
        self._results[frame_index] = result
        return result

    def commit_frame(self, frame_index: int) -> FrameResult:
        if self._state.state(frame_index) != FrameState.ESTIMATED:
            raise WorkbenchError(
                ErrorCode.STATE,
                "frame must be estimated before commit",
                frame_index=frame_index,
                details={"state": self._state.state(frame_index).value},
            )
        result = self._state.pending_result(frame_index)
        if result is None:
            raise WorkbenchError(
                ErrorCode.STATE,
                "estimated frame has no pending result",
                frame_index=frame_index,
            )
        if self.checkpoint is not None:
            self.checkpoint.save_frame(result)
        committed = self._state.commit(frame_index)
        return committed

    def process_frame(self, frame_index: int) -> FrameResult:
        self.estimate_frame(frame_index)
        return self.commit_frame(frame_index)

    def retry_frame(self, frame_index: int) -> None:
        self._state.retry(frame_index)
        self._results.pop(frame_index, None)

    def next_uncommitted(self) -> int | None:
        for index in range(self._total_frames):
            if self._state.state(index) != FrameState.COMMITTED:
                return index
        return None

    def reconfigure(self, config: WorkbenchConfig) -> None:
        config.validate()
        committed = self._state.committed_results
        new_total = self.sample.frame_count(config.analysis)
        if new_total < len(committed):
            raise WorkbenchError(
                ErrorCode.STATE,
                "new frame configuration would discard committed frames",
                details={
                    "new_total_frames": new_total,
                    "committed_frames": len(committed),
                },
            )
        self._state = FrameStateMachine(new_total, committed_results=committed)
        self._total_frames = new_total
        self.config = config
        self._results = {
            result.frame_index: result for result in committed
        }

    def close(self) -> None:
        if self._executor is not None:
            self._executor.shutdown(wait=True, cancel_futures=False)
            self._executor = None


def _checkpoint_for(
    sample: AcousticSample,
    config: WorkbenchConfig,
    path: str | Path,
) -> tuple[CheckpointStore, SnapshotStatus]:
    store = CheckpointStore(
        path,
        sample_id=sample.sample_id,
        input_hash=sample.input_hash(),
        algorithm_version=config.algorithm_version,
    )
    status = store.prepare(sample.frame_count(config.analysis))
    return store, status


def _wait_if_paused(
    pause_event: threading.Event | None,
    cancel_event: threading.Event | None,
) -> bool:
    if pause_event is None:
        return False
    while pause_event.is_set():
        if cancel_event is not None and cancel_event.is_set():
            return True
        time.sleep(0.02)
    return cancel_event is not None and cancel_event.is_set()


def _finish_result(
    *,
    mode: str,
    sample: AcousticSample,
    config: WorkbenchConfig,
    session: ProcessingSession,
    checkpoint_path: str | None,
    status: SnapshotStatus | None,
    errors: list[WorkbenchError],
    stopped_reason: str | None,
    started: float,
) -> RunResult:
    committed = session.committed_results
    complete = (
        len(committed) == session.total_frames
        and not errors
        and stopped_reason is None
    )
    render: RenderResult | None = None
    if complete:
        try:
            render = render_audio(sample, committed, config)
        except WorkbenchError as exc:
            errors.append(exc)
            complete = False
    return RunResult(
        mode=mode,
        sample_id=sample.sample_id,
        total_frames=session.total_frames,
        committed_frames=len(committed),
        frames=committed,
        state_counts=session.state_machine.state_counts(),
        completed=complete,
        report_complete=complete and render is not None,
        input_hash=session.input_hash,
        algorithm_version=config.algorithm_version,
        checkpoint_path=checkpoint_path,
        invalidated=bool(status.invalidated) if status is not None else False,
        stopped_reason=stopped_reason,
        frame_errors=tuple(errors),
        render=render,
        config=config,
        elapsed_s=time.perf_counter() - started,
    )


def _run_session(
    *,
    session: ProcessingSession,
    mode: str,
    sample: AcousticSample,
    config: WorkbenchConfig,
    checkpoint_path: str | None,
    status: SnapshotStatus | None,
    stop_after_commits: int | None,
    pause_event: threading.Event | None,
    cancel_event: threading.Event | None,
    progress_callback: ProgressCallback | None,
) -> RunResult:
    started = time.perf_counter()
    errors: list[WorkbenchError] = []
    stopped_reason: str | None = None
    if stop_after_commits is not None and stop_after_commits < 0:
        raise para_error("stop_after_commits cannot be negative", "stop_after_commits")

    while True:
        if _wait_if_paused(pause_event, cancel_event):
            stopped_reason = "cancelled"
            break
        if cancel_event is not None and cancel_event.is_set():
            stopped_reason = "cancelled"
            break
        if (
            stop_after_commits is not None
            and session.progress[0] >= stop_after_commits
        ):
            stopped_reason = "paused"
            break
        frame_index = session.next_uncommitted()
        if frame_index is None:
            break
        try:
            result = session.process_frame(frame_index)
        except WorkbenchError as exc:
            errors.append(exc)
            stopped_reason = "frame_failed"
            break
        if progress_callback is not None:
            progress_callback(*session.progress, result)

    try:
        return _finish_result(
            mode=mode,
            sample=sample,
            config=config,
            session=session,
            checkpoint_path=checkpoint_path,
            status=status,
            errors=errors,
            stopped_reason=stopped_reason,
            started=started,
        )
    finally:
        session.close()


def run_realtime(
    sample: AcousticSample,
    config: WorkbenchConfig,
    checkpoint_path: str | Path | None = None,
    *,
    stop_after_commits: int | None = None,
    pause_event: threading.Event | None = None,
    cancel_event: threading.Event | None = None,
    progress_callback: ProgressCallback | None = None,
    fresh: bool = True,
) -> RunResult:
    config.validate()
    total_frames = sample.frame_count(config.analysis)
    store: CheckpointStore | None = None
    status: SnapshotStatus | None = None
    committed: tuple[FrameResult, ...] = ()
    if checkpoint_path is not None:
        store, status = _checkpoint_for(sample, config, checkpoint_path)
        if fresh:
            store.clear()
            status = store.prepare(total_frames)
        else:
            committed = store.load_frames() if status.committed_frames else ()
    session = ProcessingSession(
        sample,
        config,
        checkpoint=store,
        committed_results=committed,
    )
    try:
        return _run_session(
            session=session,
            mode="realtime",
            sample=sample,
            config=config,
            checkpoint_path=None if checkpoint_path is None else str(checkpoint_path),
            status=status,
            stop_after_commits=stop_after_commits,
            pause_event=pause_event,
            cancel_event=cancel_event,
            progress_callback=progress_callback,
        )
    finally:
        if store is not None:
            store.close()


def run_resume(
    sample: AcousticSample,
    config: WorkbenchConfig,
    checkpoint_path: str | Path,
    *,
    stop_after_commits: int | None = None,
    pause_event: threading.Event | None = None,
    cancel_event: threading.Event | None = None,
    progress_callback: ProgressCallback | None = None,
) -> RunResult:
    config.validate()
    store, status = _checkpoint_for(sample, config, checkpoint_path)
    committed: tuple[FrameResult, ...] = ()
    if not status.invalidated:
        committed = store.load_frames()
    session = ProcessingSession(
        sample,
        config,
        checkpoint=store,
        committed_results=committed,
    )
    try:
        return _run_session(
            session=session,
            mode="resume",
            sample=sample,
            config=config,
            checkpoint_path=str(checkpoint_path),
            status=status,
            stop_after_commits=stop_after_commits,
            pause_event=pause_event,
            cancel_event=cancel_event,
            progress_callback=progress_callback,
        )
    finally:
        store.close()


def run_full_recompute(
    sample: AcousticSample,
    checkpoint_path: str | Path,
    config: WorkbenchConfig | None = None,
) -> RunResult:
    if config is None:
        config = WorkbenchConfig()
    config.validate()
    store, status = _checkpoint_for(sample, config, checkpoint_path)
    try:
        if status.invalidated:
            raise WorkbenchError(
                ErrorCode.STATE,
                "checkpoint snapshot was invalidated and cannot be recomputed",
                details={"reason": status.reason},
            )
        frames = store.load_frames()
        total = sample.frame_count(config.analysis)
        if len(frames) != total:
            raise WorkbenchError(
                ErrorCode.STATE,
                "full recompute requires a complete persisted checkpoint",
                details={"committed_frames": len(frames), "total_frames": total},
            )
        started = time.perf_counter()
        render = render_audio(sample, frames, config)
        return RunResult(
            mode="full_recompute",
            sample_id=sample.sample_id,
            total_frames=total,
            committed_frames=len(frames),
            frames=frames,
            state_counts={
                FrameState.PENDING.value: 0,
                FrameState.ESTIMATED.value: 0,
                FrameState.COMMITTED.value: total,
                FrameState.FAILED.value: 0,
            },
            completed=True,
            report_complete=True,
            input_hash=sample.input_hash(),
            algorithm_version=config.algorithm_version,
            checkpoint_path=str(checkpoint_path),
            invalidated=False,
            frame_errors=(),
            render=render,
            config=config,
            elapsed_s=time.perf_counter() - started,
        )
    finally:
        store.close()


def compare_results(
    path_a: RunResult,
    path_b: RunResult,
    *,
    raise_on_mismatch: bool = True,
) -> ComparisonSummary:
    output_a = path_a.output
    output_b = path_b.output
    if output_a is None or output_b is None:
        raise WorkbenchError(
            ErrorCode.DIVERGE,
            "both paths must contain complete render output",
            details={
                "path_a_complete": path_a.report_complete,
                "path_b_complete": path_b.report_complete,
            },
        )
    if output_a.shape != output_b.shape:
        mismatch = WorkbenchError(
            ErrorCode.DIVERGE,
            "render output shapes differ",
            sample_index=0,
            details={
                "path_a_shape": list(output_a.shape),
                "path_b_shape": list(output_b.shape),
                "path_a_value": None,
                "path_b_value": None,
            },
        )
        if raise_on_mismatch:
            raise mismatch
        return ComparisonSummary(False, 0, 0, 0, 0)

    flat_a = output_a.reshape(-1)
    flat_b = output_b.reshape(-1)
    differences = np.flatnonzero(flat_a != flat_b)
    compared_samples = int(flat_a.size)
    if differences.size:
        local = int(differences[0])
        sample_index = local % output_a.shape[1]
        channel_index = local // output_a.shape[1]
        hop = 1
        if path_a.config is not None:
            hop = max(1, path_a.config.analysis.hop)
        frame_index = sample_index // hop
        mismatch = WorkbenchError(
            ErrorCode.DIVERGE,
            "rendered samples differ between paths",
            channel=int(channel_index),
            frame_index=int(frame_index),
            sample_index=int(sample_index),
            details={
                "path_a_value": float(flat_a[local]),
                "path_b_value": float(flat_b[local]),
                "flat_index": local,
            },
        )
        if raise_on_mismatch:
            raise mismatch
        return ComparisonSummary(
            False,
            compared_samples,
            0,
            sample_index,
            frame_index,
        )

    sequence_a = path_a.commit_sequence
    sequence_b = path_b.commit_sequence
    if sequence_a != sequence_b:
        first = next(
            (
                index
                for index, (left, right) in enumerate(zip(sequence_a, sequence_b))
                if left != right
            ),
            min(len(sequence_a), len(sequence_b)),
        )
        mismatch = WorkbenchError(
            ErrorCode.DIVERGE,
            "frame commit sequences differ",
            frame_index=first,
            details={
                "path_a_sequence": list(sequence_a),
                "path_b_sequence": list(sequence_b),
            },
        )
        if raise_on_mismatch:
            raise mismatch
        return ComparisonSummary(False, compared_samples, 0, None, first)

    def comparison_payload(frame: FrameResult) -> dict[str, Any]:
        payload = frame.to_dict()
        config_payload = payload.get("config")
        if isinstance(config_payload, dict):
            config_payload.pop("max_workers", None)
            config_payload.pop("per_thread_buffer_samples", None)
        return payload

    frames_a = {frame.frame_index: frame for frame in path_a.frames}
    frames_b = {frame.frame_index: frame for frame in path_b.frames}
    for frame_index in sequence_a:
        payload_a = comparison_payload(frames_a[frame_index])
        payload_b = comparison_payload(frames_b[frame_index])
        if payload_a != payload_b:
            mismatch = WorkbenchError(
                ErrorCode.DIVERGE,
                "frame parameters or error bounds differ between paths",
                frame_index=frame_index,
                details={
                    "path_a_frame": payload_a,
                    "path_b_frame": payload_b,
                },
            )
            if raise_on_mismatch:
                raise mismatch
            return ComparisonSummary(
                False,
                compared_samples,
                0,
                None,
                frame_index,
            )
    return ComparisonSummary(
        True,
        compared_samples,
        len(sequence_a),
        None,
        None,
    )
