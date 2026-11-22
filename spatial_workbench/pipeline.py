"""Reconstruction orchestration for realtime, recovery and replay paths."""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .analysis import (
    AnalysisContext,
    FrameSchedule,
    build_analysis_context,
    build_frame_schedule,
    estimate_frame,
    truth_report,
)
from .checkpoint import CheckpointLoadResult, CheckpointStore, PersistedFrame
from .errors import ErrorCode, WorkbenchError
from .models import (
    AcousticSample,
    AnalysisConfig,
    FrameParameters,
    FrameRenderStats,
    RenderResult,
)
from .render import FrameContribution, RenderEngine
from .state import FrameState, FrameStateMachine


@dataclass
class PipelineOutcome:
    mode: str
    complete: bool
    result: RenderResult | None
    state: FrameStateMachine | None
    error: WorkbenchError | None
    checkpoint_load: CheckpointLoadResult | None
    checkpoint_invalidated: bool
    checkpoint_generation: int
    truth: dict[str, Any] | None
    scheduled_frames: int
    cancelled: bool = False

    def to_dict(self, *, include_frames: bool = True) -> dict[str, Any]:
        value: dict[str, Any] = {
            "ok": self.error is None and self.complete,
            "mode": self.mode,
            "complete": self.complete,
            "checkpoint_generation": self.checkpoint_generation,
            "checkpoint_invalidated": self.checkpoint_invalidated,
            "scheduled_frames": self.scheduled_frames,
            "cancelled": self.cancelled,
            "state_counts": self.state.counts() if self.state is not None else {},
            "error": self.error.to_dict() if self.error is not None else None,
            "truth": self.truth,
        }
        if self.checkpoint_load is not None:
            value["checkpoint_load"] = {
                "valid": self.checkpoint_load.valid,
                "found": self.checkpoint_load.found,
                "invalid_reason": self.checkpoint_load.invalid_reason,
                "frame_count": self.checkpoint_load.frame_count,
                "generation": self.checkpoint_load.generation,
            }
        if self.result is not None:
            value["render"] = {
                "path": self.result.path,
                "channels": list(self.result.channels),
                "sample_rate": self.result.sample_rate,
                "sample_count": int(self.result.output.shape[1]),
                "output_hash": self.result.output_hash,
                "frame_count": len(self.result.frames),
                "render_stats": self.result.stats_dict(),
            }
            if include_frames:
                value["frames"] = [
                    {
                        "frame_index": frame.frame_index,
                        "state": (
                            self.state.state(frame.frame_index).value
                            if self.state is not None
                            else "committed"
                        ),
                        "parameters": frame.to_dict(),
                        "energy_statistics": frame.energy_statistics(
                            self.result.render_stats.get(frame.frame_index)
                        ),
                    }
                    for frame in self.result.frames
                ]
        return value


class ReconstructionPipeline:
    def __init__(
        self,
        sample: AcousticSample,
        config: AnalysisConfig,
        checkpoint_path: str | Path,
    ) -> None:
        self.sample = sample
        self.config = config
        self.checkpoint_path = Path(checkpoint_path)
        self.store = CheckpointStore(
            self.checkpoint_path,
            input_hash=sample.content_hash,
            algorithm_version=config.algorithm_version,
        )
        self._last_outcome: PipelineOutcome | None = None
        self._cancel_event = threading.Event()

    @property
    def last_outcome(self) -> PipelineOutcome | None:
        return self._last_outcome

    def cancel(self) -> None:
        """Request a frame-boundary cancellation.

        Already committed frames remain in the checkpoint. No exit report is
        promised if the process is killed while a checkpoint replacement is in
        progress; the previous snapshot remains intact by atomic rename.
        """

        self._cancel_event.set()

    def reconfigure_uncommitted(
        self,
        *,
        frame_length: int | None = None,
        energy_threshold: float | None = None,
        output_layout: str | None = None,
    ) -> AnalysisConfig:
        """Apply analysis/render options for a subsequent recovery pass.

        Existing checkpoint records are never rewritten. The next recovery run
        restores their committed parameters and only estimates/renders the
        uncommitted suffix with the new configuration.
        """

        if self.config.algorithm_version != self.store.algorithm_version:
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "algorithm version changes require a fresh checkpoint generation",
                stage="reconfigure",
            )
        changes: dict[str, Any] = {}
        if frame_length is not None:
            changes["frame_length"] = frame_length
        if energy_threshold is not None:
            changes["energy_threshold"] = energy_threshold
        if output_layout is not None:
            changes["output_layout"] = output_layout
        if not changes:
            return self.config
        self.config = self.config.replace(**changes)
        return self.config

    def _new_state(self, frame_count: int) -> FrameStateMachine:
        if frame_count <= 0:
            raise WorkbenchError(
                ErrorCode.E_FRAME,
                "no render frames were scheduled",
                stage="pipeline",
            )
        return FrameStateMachine(frame_count)

    def _estimate_indices(
        self,
        state: FrameStateMachine,
        context: AnalysisContext,
        schedules: list[FrameSchedule],
        indices: Iterable[int],
    ) -> WorkbenchError | None:
        pending = [
            index
            for index in indices
            if state.state(index) != FrameState.COMMITTED
        ]
        if not pending:
            return None
        required = (
            context.config.frame_length
            * context.channel_count
            * 8
            * 8
        )
        if required > context.config.max_buffer_bytes:
            error = WorkbenchError(
                ErrorCode.E_RESOURCE,
                "worker analysis buffer exceeds max_buffer_bytes",
                stage="analysis",
                details={
                    "required_bytes_estimate": required,
                    "max_buffer_bytes": context.config.max_buffer_bytes,
                },
            )
            for index in pending:
                try:
                    state.fail(index, error)
                except WorkbenchError:
                    pass
            return error
        workers = max(1, min(context.config.max_workers, len(pending)))
        first_error: WorkbenchError | None = None
        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = {
                index: executor.submit(
                    state.estimate,
                    index,
                    lambda index=index: estimate_frame(
                        self.sample,
                        schedules[index],
                        context,
                    ),
                )
                for index in pending
            }
            for index in pending:
                try:
                    futures[index].result()
                except WorkbenchError as exc:
                    if first_error is None:
                        first_error = exc
                except Exception as exc:
                    wrapped = WorkbenchError.wrap(
                        ErrorCode.E_FRAME,
                        "frame estimate worker failed",
                        exc,
                        stage="analysis",
                        frame_index=index,
                    )
                    if first_error is None:
                        first_error = wrapped
        return first_error

    def _render_candidates(
        self,
        state: FrameStateMachine,
        indices: Iterable[int],
    ) -> tuple[dict[int, FrameContribution], dict[int, WorkbenchError]]:
        candidates: list[int] = []
        for index in indices:
            frame_state = state.state(index)
            if frame_state in (FrameState.ESTIMATED, FrameState.COMMITTED):
                candidates.append(index)
        if not candidates:
            return {}, {}
        try:
            max_length = max(state.params(index).frame_length for index in candidates)
            max_sources = max(
                len(state.params(index).local_arrival_delays_samples)
                for index in candidates
            )
            max_outputs = max(
                len(state.params(index).output_channels) for index in candidates
            )
        except WorkbenchError as exc:
            return {}, {index: exc for index in candidates}
        required = max_length * max(max_sources, max_outputs, 1) * 8 * 8
        if required > self.config.max_buffer_bytes:
            error = WorkbenchError(
                ErrorCode.E_RESOURCE,
                "worker render buffer exceeds max_buffer_bytes",
                stage="render",
                details={
                    "required_bytes_estimate": required,
                    "max_buffer_bytes": self.config.max_buffer_bytes,
                },
            )
            return {}, {index: error for index in candidates}
        engine = RenderEngine(clip_limit=self.config.clip_limit)
        workers = max(1, min(self.config.max_workers, len(candidates)))
        contributions: dict[int, FrameContribution] = {}
        errors: dict[int, WorkbenchError] = {}
        if workers == 1:
            for index in candidates:
                try:
                    contributions[index] = engine.render_contribution(
                        state.params(index),
                        self.sample.dry,
                    )
                except WorkbenchError as exc:
                    errors[index] = exc
            return contributions, errors
        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = {
                index: executor.submit(
                    engine.render_contribution,
                    state.params(index),
                    self.sample.dry,
                )
                for index in candidates
            }
            for index in candidates:
                try:
                    contributions[index] = futures[index].result()
                except WorkbenchError as exc:
                    errors[index] = exc
                except Exception as exc:
                    errors[index] = WorkbenchError.wrap(
                        ErrorCode.E_FRAME,
                        "render worker failed",
                        exc,
                        stage="render",
                        frame_index=index,
                    )
        return contributions, errors

    def _seed_records(
        self,
        state: FrameStateMachine,
        records: tuple[PersistedFrame, ...],
    ) -> None:
        for record in records:
            if record.frame_index >= len(state):
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "checkpoint contains more frames than the current schedule",
                    stage="checkpoint-load",
                    frame_index=record.frame_index,
                    details={
                        "scheduled_frames": len(state),
                        "checkpoint_frames": len(records),
                    },
                )
            state.load_committed(
                record.frame_index,
                record.params,
                record.render_stats,
            )

    def _assemble_result(
        self,
        state: FrameStateMachine,
        contributions: dict[int, FrameContribution],
        committed_indices: list[int],
        *,
        path: str,
        channels_hint: tuple[str, ...],
    ) -> RenderResult | None:
        if not committed_indices:
            return None
        output_channels: list[str] = []
        buffers: dict[str, np.ndarray] = {}
        dry_length = int(self.sample.dry.size)
        for index in sorted(committed_indices):
            contribution = contributions.get(index)
            if contribution is None:
                continue
            for channel_index, channel in enumerate(contribution.channels):
                if channel not in buffers:
                    output_channels.append(channel)
                    buffers[channel] = np.zeros(dry_length, dtype=np.float64)
                available = min(
                    contribution.signal.shape[1],
                    dry_length - contribution.start,
                )
                if available > 0:
                    buffers[channel][
                        contribution.start : contribution.start + available
                    ] += contribution.signal[channel_index, :available]
        if not output_channels:
            output_channels = list(channels_hint)
            for channel in output_channels:
                buffers[channel] = np.zeros(dry_length, dtype=np.float64)
        output = np.stack([buffers[channel] for channel in output_channels], axis=0)
        if output.shape[1] != dry_length:
            raise WorkbenchError(
                ErrorCode.E_FRAME,
                "assembled output length does not equal dry duration",
                stage="pipeline",
                details={
                    "output_samples": int(output.shape[1]),
                    "dry_samples": dry_length,
                },
            )
        if output.size and float(np.max(np.abs(output))) > self.config.clip_limit:
            raise WorkbenchError(
                ErrorCode.E_CLIP,
                "assembled output exceeds clip limit",
                stage="pipeline",
                details={
                    "peak": float(np.max(np.abs(output))),
                    "clip_limit": self.config.clip_limit,
                },
            )
        frames = tuple(
            state.params(index) for index in sorted(committed_indices)
        )
        stats = {
            index: state.render_stats(index)
            for index in sorted(committed_indices)
        }
        if any(value is None for value in stats.values()):
            raise WorkbenchError(
                ErrorCode.E_STATE,
                "committed frame is missing render statistics",
                stage="pipeline",
            )
        typed_stats = {
            index: value
            for index, value in stats.items()
            if value is not None
        }
        return RenderResult(
            output=np.ascontiguousarray(output, dtype=np.float64),
            channels=tuple(output_channels),
            sample_rate=self.sample.sample_rate,
            frames=frames,
            render_stats=typed_stats,
            path=path,
            checkpoint_generation=self.store.generation,
        )

    def _run_commit_loop(
        self,
        state: FrameStateMachine,
        frame_count: int,
        *,
        mode: str,
        stop_after: int | None,
        committed_prefix: int,
        channels_hint: tuple[str, ...],
    ) -> tuple[RenderResult | None, WorkbenchError | None, list[int]]:
        contributions, render_errors = self._render_candidates(state, range(frame_count))
        committed_indices: list[int] = list(range(committed_prefix))
        new_commits = 0
        error: WorkbenchError | None = None
        for index in range(frame_count):
            if self._cancel_event.is_set():
                break
            frame_state = state.state(index)
            if frame_state == FrameState.COMMITTED:
                contribution = contributions.get(index)
                if contribution is None:
                    error = render_errors.get(index) or WorkbenchError(
                        ErrorCode.E_FRAME,
                        "committed frame could not be re-rendered",
                        stage="render",
                        frame_index=index,
                    )
                    break
                if self.store.records and index < len(self.store.records):
                    persisted_stats = self.store.records[index].render_stats
                    if (
                        persisted_stats is not None
                        and persisted_stats != contribution.stats
                    ):
                        error = WorkbenchError(
                            ErrorCode.E_DIFF,
                            "checkpoint statistics do not reproduce exactly",
                            stage="replay",
                            frame_index=index,
                            details={
                                "persisted": persisted_stats.to_dict(),
                                "recomputed": contribution.stats.to_dict(),
                            },
                        )
                        break
                if state.render_stats(index) is None:
                    state.attach_render_stats(index, contribution.stats)
                continue
            if frame_state == FrameState.FAILED:
                error = state.error(index) or WorkbenchError(
                    ErrorCode.E_FRAME,
                    "frame failed before commit",
                    stage="pipeline",
                    frame_index=index,
                )
                break
            if frame_state == FrameState.PENDING:
                error = WorkbenchError(
                    ErrorCode.E_STATE,
                    "a pending frame reached the commit boundary",
                    stage="commit",
                    frame_index=index,
                )
                break
            contribution = contributions.get(index)
            if contribution is None:
                error = render_errors.get(index) or WorkbenchError(
                    ErrorCode.E_FRAME,
                    "frame could not be rendered",
                    stage="render",
                    frame_index=index,
                )
                state.fail(index, error)
                break
            # Validate the OLA prefix before freezing and committing this frame.
            try:
                preview = self._preview_peak(contributions, committed_indices + [index])
            except WorkbenchError as exc:
                error = exc
                state.fail(index, exc)
                break
            if preview > self.config.clip_limit:
                error = WorkbenchError(
                    ErrorCode.E_CLIP,
                    "overlap-add prefix exceeds the clip limit",
                    stage="render",
                    frame_index=index,
                    details={
                        "peak": preview,
                        "clip_limit": self.config.clip_limit,
                    },
                )
                state.fail(index, error)
                break
            state.attach_render_stats(index, contribution.stats)
            try:
                state.commit(
                    index,
                    lambda params, stats, index=index: self.store.commit(
                        params,
                        stats,
                    ),
                )
            except WorkbenchError as exc:
                error = exc
                break
            committed_indices.append(index)
            new_commits += 1
            if stop_after is not None and new_commits >= stop_after:
                break
        result = self._assemble_result(
            state,
            contributions,
            committed_indices,
            path=mode,
            channels_hint=channels_hint,
        )
        return result, error, committed_indices

    def _preview_peak(
        self,
        contributions: dict[int, FrameContribution],
        indices: list[int],
    ) -> float:
        channels: list[str] = []
        buffers: dict[str, np.ndarray] = {}
        size = int(self.sample.dry.size)
        for index in sorted(indices):
            contribution = contributions.get(index)
            if contribution is None:
                continue
            for channel_index, channel in enumerate(contribution.channels):
                if channel not in buffers:
                    channels.append(channel)
                    buffers[channel] = np.zeros(size, dtype=np.float64)
                available = min(
                    contribution.signal.shape[1],
                    size - contribution.start,
                )
                if available > 0:
                    buffers[channel][
                        contribution.start : contribution.start + available
                    ] += contribution.signal[channel_index, :available]
        peak = 0.0
        for buffer in buffers.values():
            peak = max(peak, float(np.max(np.abs(buffer))))
        return peak

    def _outcome(
        self,
        *,
        mode: str,
        frame_count: int,
        result: RenderResult | None,
        state: FrameStateMachine | None,
        error: WorkbenchError | None,
        checkpoint_load: CheckpointLoadResult | None,
        invalidated: bool,
        truth: dict[str, Any] | None,
    ) -> PipelineOutcome:
        complete = (
            error is None
            and state is not None
            and len(state) == frame_count
            and all(item == FrameState.COMMITTED for item in state.states())
            and result is not None
            and len(result.frames) == frame_count
        )
        outcome = PipelineOutcome(
            mode=mode,
            complete=complete,
            result=result,
            state=state,
            error=error,
            checkpoint_load=checkpoint_load,
            checkpoint_invalidated=invalidated,
            checkpoint_generation=self.store.generation,
            truth=truth,
            scheduled_frames=frame_count,
            cancelled=self._cancel_event.is_set(),
        )
        self._last_outcome = outcome
        return outcome

    def run_realtime(
        self,
        *,
        stop_after: int | None = None,
    ) -> PipelineOutcome:
        mode = "realtime"
        self._cancel_event.clear()
        truth: dict[str, Any] | None = None
        state: FrameStateMachine | None = None
        try:
            context = build_analysis_context(self.sample, self.config)
            schedules = build_frame_schedule(self.sample, self.config)
            frame_count = len(schedules)
            truth = truth_report(self.sample, context)
            self.store.reset()
            state = self._new_state(frame_count)
            estimate_error = self._estimate_indices(
                state,
                context,
                schedules,
                range(frame_count),
            )
            result, commit_error, _ = self._run_commit_loop(
                state,
                frame_count,
                mode=mode,
                stop_after=stop_after,
                committed_prefix=0,
                channels_hint=context.mapping.output_channels,
            )
            error = commit_error or estimate_error
            return self._outcome(
                mode=mode,
                frame_count=frame_count,
                result=result,
                state=state,
                error=error,
                checkpoint_load=self.store.load(),
                invalidated=False,
                truth=truth,
            )
        except WorkbenchError as exc:
            return self._outcome(
                mode=mode,
                frame_count=len(state) if state is not None else 0,
                result=None,
                state=state,
                error=exc,
                checkpoint_load=None,
                invalidated=False,
                truth=truth,
            )

    def run_recovery(
        self,
        *,
        stop_after: int | None = None,
    ) -> PipelineOutcome:
        mode = "recovery"
        self._cancel_event.clear()
        truth: dict[str, Any] | None = None
        state: FrameStateMachine | None = None
        try:
            context = build_analysis_context(self.sample, self.config)
            schedules = build_frame_schedule(self.sample, self.config)
            frame_count = len(schedules)
            truth = truth_report(self.sample, context)
            load_result = self.store.load()
            invalidated = not load_result.valid
            if invalidated:
                self.store.reset()
                records: tuple[PersistedFrame, ...] = ()
            else:
                records = load_result.records
            state = self._new_state(frame_count)
            self._seed_records(state, records)
            estimate_error = self._estimate_indices(
                state,
                context,
                schedules,
                range(frame_count),
            )
            result, commit_error, _ = self._run_commit_loop(
                state,
                frame_count,
                mode=mode,
                stop_after=stop_after,
                committed_prefix=len(records),
                channels_hint=context.mapping.output_channels,
            )
            error = commit_error or estimate_error
            return self._outcome(
                mode=mode,
                frame_count=frame_count,
                result=result,
                state=state,
                error=error,
                checkpoint_load=load_result,
                invalidated=invalidated,
                truth=truth,
            )
        except WorkbenchError as exc:
            return self._outcome(
                mode=mode,
                frame_count=len(state) if state is not None else 0,
                result=None,
                state=state,
                error=exc,
                checkpoint_load=None,
                invalidated=False,
                truth=truth,
            )

    def run_replay(self) -> PipelineOutcome:
        mode = "replay"
        self._cancel_event.clear()
        truth: dict[str, Any] | None = None
        state: FrameStateMachine | None = None
        try:
            context = build_analysis_context(self.sample, self.config)
            schedules = build_frame_schedule(self.sample, self.config)
            frame_count = len(schedules)
            truth = truth_report(self.sample, context)
            load_result = self.store.load()
            if not load_result.valid:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "cannot replay from an invalid or mismatched checkpoint",
                    stage="replay",
                    details={"invalid_reason": load_result.invalid_reason},
                )
            if load_result.frame_count != frame_count:
                raise WorkbenchError(
                    ErrorCode.E_STATE,
                    "checkpoint is incomplete for full replay",
                    stage="replay",
                    details={
                        "checkpoint_frames": load_result.frame_count,
                        "scheduled_frames": frame_count,
                    },
                )
            state = self._new_state(frame_count)
            self._seed_records(state, load_result.records)
            result, commit_error, _ = self._run_commit_loop(
                state,
                frame_count,
                mode=mode,
                stop_after=None,
                committed_prefix=frame_count,
                channels_hint=context.mapping.output_channels,
            )
            return self._outcome(
                mode=mode,
                frame_count=frame_count,
                result=result,
                state=state,
                error=commit_error,
                checkpoint_load=load_result,
                invalidated=False,
                truth=truth,
            )
        except WorkbenchError as exc:
            return self._outcome(
                mode=mode,
                frame_count=len(state) if state is not None else 0,
                result=None,
                state=state,
                error=exc,
                checkpoint_load=None,
                invalidated=False,
                truth=truth,
            )

