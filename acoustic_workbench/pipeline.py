"""Analysis, checkpoint commit, resume, and persisted full recomputation."""

from __future__ import annotations

import hashlib
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .analysis import FrameAnalyzer
from .codes import E_META, E_PARA, E_STATE
from .concurrency import BoundedParallelRunner
from .config import ALGORITHM_VERSION, AnalysisConfig, frame_count, validate_analysis_config
from .errors import WorkbenchError, error
from .io import validate_sample
from .models import (
    FrameEstimate,
    RenderConfig,
    RenderResult,
    Sample,
    canonical_json,
)
from .render import (
    ConvolutionRenderer,
    FrameContribution,
    make_block_plan,
    resolve_channel_mapping,
    validate_render_config,
)
from .state import FrameState, FrameStateMachine
from .strategy import resolve_analysis_config


@dataclass
class PipelineRun:
    complete: bool
    committed_frames: int
    total_frames: int
    input_hash: str
    snapshot_reason: str
    decision: dict[str, Any]
    estimates: list[FrameEstimate] = field(default_factory=list)
    render_result: RenderResult | None = None
    progress: dict[str, Any] = field(default_factory=dict)
    checkpoint_path: str = ""
    cancelled: bool = False


@dataclass
class _TaskOutcome:
    frame_index: int
    estimate: FrameEstimate | None = None
    failure: WorkbenchError | None = None


def build_input_hash(
    sample: Sample,
) -> str:
    # The content hash intentionally excludes tuning parameters. Committed
    # frames stay valid while only uncommitted frames adopt updated estimates.
    payload = {
        "algorithm_version": ALGORITHM_VERSION,
        "sample_id": sample.id,
        "source_hash": sample.source_hash,
        "sample_rate": sample.sample_rate,
        "layout": sample.layout.to_dict() if sample.layout else None,
        "ir_shape": list(sample.ir.shape),
        "dry_shape": list(sample.dry.shape),
    }
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()


class AcousticPipeline:
    def __init__(
        self,
        sample: Sample,
        requested_analysis: AnalysisConfig,
        render_config: RenderConfig,
        checkpoint_path: str | Path,
        *,
        analyzer: FrameAnalyzer | None = None,
        renderer: ConvolutionRenderer | None = None,
    ) -> None:
        self.sample = sample
        self.requested_analysis = requested_analysis
        self.render_config = render_config
        self.checkpoint_path = Path(checkpoint_path)
        self.analyzer = analyzer or FrameAnalyzer()
        self.renderer = renderer or ConvolutionRenderer()
        try:
            validate_sample(sample)
        except WorkbenchError as exc:
            exc.add_stage("pipeline.preflight", context={"sample_id": sample.id})
            raise
        validate_analysis_config(requested_analysis)
        validate_render_config(render_config)
        self.effective_config, self.decision = resolve_analysis_config(
            sample, requested_analysis
        )
        self.total_frames = frame_count(
            sample.ir.shape[1],
            self.effective_config.frame_length,
            self.effective_config.hop_length,
        )
        self.input_hash = build_input_hash(sample)
        self.mapping = resolve_channel_mapping(sample, render_config)
        self.block_plan = make_block_plan(
            sample.dry.size,
            max(1, self.total_frames),
            render_config.render_block_size,
        )
        self.analysis_runner = BoundedParallelRunner(
            self.effective_config.max_parallelism,
            self.effective_config.thread_buffer_mb * 1024 * 1024,
            stage="pipeline.analysis_parallel",
        )
        self.render_runner = BoundedParallelRunner(
            self.effective_config.max_parallelism,
            self.effective_config.thread_buffer_mb * 1024 * 1024,
            stage="pipeline.render_parallel",
        )
        from .checkpoint import CheckpointStore

        self.store = CheckpointStore(self.checkpoint_path)
        try:
            self._guard_committed_config()
            self._initialize_store()
            self.snapshot = self.store.load_snapshot(
                input_hash=self.input_hash,
                algorithm_version=ALGORITHM_VERSION,
                total_frames=self.total_frames,
            )
        except Exception:
            self.store.close()
            raise
        self.state = FrameStateMachine(self.total_frames)
        self._cancel_event = threading.Event()
        self._restore_snapshot()

    def _guard_committed_config(self) -> None:
        prior = self.store.read_meta()
        if not prior or self.store.committed_count() == 0:
            return
        if prior.get("input_hash") != self.input_hash:
            return
        if prior.get("algorithm_version") != ALGORITHM_VERSION:
            return
        prior_render = prior.get("render_config")
        if prior_render != self.render_config.to_dict():
            raise error(
                E_STATE,
                "render configuration cannot change after frames are committed",
                stage="pipeline.frozen_config",
                field="render_config",
                context={
                    "committed_frames": self.store.committed_count(),
                    "prior": prior_render,
                    "requested": self.render_config.to_dict(),
                },
            )
        prior_analysis = prior.get("analysis_config") or {}
        for field_name in ("frame_length", "hop_length"):
            if prior_analysis.get(field_name) != getattr(self.effective_config, field_name):
                raise error(
                    E_STATE,
                    f"{field_name} cannot change after frames are committed",
                    stage="pipeline.frozen_config",
                    field=field_name,
                    context={
                        "committed_frames": self.store.committed_count(),
                        "prior": prior_analysis.get(field_name),
                        "requested": getattr(self.effective_config, field_name),
                    },
                )

    def _initialize_store(self) -> None:
        self.store.initialize(
            {
                "schema_version": 1,
                "input_hash": self.input_hash,
                "algorithm_version": ALGORITHM_VERSION,
                "sample_id": self.sample.id,
                "total_frames": self.total_frames,
                "analysis_config": self.effective_config.to_dict(),
                "render_config": self.render_config.to_dict(),
                "resolution": self.decision.to_dict(),
            }
        )

    def _restore_snapshot(self) -> None:
        if not self.snapshot.valid:
            return
        for record in self.snapshot.committed_records:
            estimate = FrameEstimate.from_dict(record["estimate"])
            index = int(record["frame_index"])
            self.state.mark_estimated(index, estimate)
            self.state.mark_committed(index)

    def _thread_buffer_bytes(self) -> int:
        channels = max(1, self.sample.channel_count)
        frame_bytes = channels * (self.effective_config.frame_length + 512) * 8 * 3
        blocks_per_frame = max(
            1,
            (
                (self.sample.dry.size + self.render_config.render_block_size - 1)
                // self.render_config.render_block_size
                + max(1, self.total_frames)
                - 1
            )
            // max(1, self.total_frames),
        )
        render_bytes = (
            blocks_per_frame
            * len(self.mapping)
            * (self.render_config.render_block_size + self.render_config.convolution_window)
            * 8
        )
        return max(frame_bytes, render_bytes)

    def progress(self) -> dict[str, Any]:
        progress = self.state.progress()
        progress["input_hash"] = self.input_hash
        progress["snapshot_reason"] = self.snapshot.reason
        progress["decision"] = self.decision.to_dict()
        return progress

    def request_cancel(self) -> None:
        self._cancel_event.set()

    def clear_cancel(self) -> None:
        self._cancel_event.clear()

    def run(
        self,
        *,
        stop_after: int | None = None,
        progress_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> PipelineRun:
        if stop_after is not None and stop_after < 0:
            raise error(
                E_PARA,
                "stop_after cannot be negative",
                stage="pipeline.run",
                field="stop_after",
                context={"value": stop_after},
            )
        committed = self.state.progress()["committed_frames"]
        target = self.total_frames if stop_after is None else min(self.total_frames, stop_after)
        if committed >= target:
            if committed == self.total_frames:
                return self._complete_run()
            return self._partial_run()

        indices = range(committed, target)

        def worker(frame_index: int) -> _TaskOutcome:
            try:
                estimate = self.analyzer.estimate_frame(
                    self.sample,
                    self.effective_config,
                    frame_index,
                    resolution=self.decision.to_dict(),
                )
                return _TaskOutcome(frame_index=frame_index, estimate=estimate)
            except WorkbenchError as exc:
                exc.add_stage("pipeline.frame_analysis", context={"frame_index": frame_index})
                return _TaskOutcome(frame_index=frame_index, failure=exc)

        runner = self.analysis_runner
        outcomes = runner.map_ordered(
            indices,
            worker,
            buffer_estimator=lambda _: self._thread_buffer_bytes(),
        )
        cancelled = False
        for outcome in outcomes:
            if self._cancel_event.is_set():
                cancelled = True
                break
            if outcome.failure is not None:
                self.state.mark_failed(
                    outcome.frame_index,
                    outcome.failure.to_dict(),
                )
                raise outcome.failure
            if outcome.estimate is None:
                raise error(
                    E_STATE,
                    "analysis worker returned no estimate",
                    stage="pipeline.run",
                    frame_index=outcome.frame_index,
                )
            self.state.mark_estimated(outcome.frame_index, outcome.estimate)
            try:
                self.store.commit_frame(
                    frame_index=outcome.frame_index,
                    sequence=outcome.frame_index,
                    estimate=outcome.estimate,
                    input_hash=self.input_hash,
                    algorithm_version=ALGORITHM_VERSION,
                )
            except Exception as exc:
                failure = exc if isinstance(exc, WorkbenchError) else error(
                    E_META,
                    f"checkpoint commit failed: {exc}",
                    stage="pipeline.commit",
                    frame_index=outcome.frame_index,
                )
                self.state.mark_failed(outcome.frame_index, failure.to_dict())
                raise failure
            self.state.mark_committed(outcome.frame_index)
            if progress_callback:
                progress_callback(self.progress())

        committed = self.state.progress()["committed_frames"]
        if cancelled:
            return self._partial_run(cancelled=True)
        if committed == self.total_frames:
            return self._complete_run()
        return self._partial_run()

    def _complete_run(self) -> PipelineRun:
        estimates = self.state.frozen_estimates()
        if len(estimates) != self.total_frames:
            raise error(
                E_STATE,
                "cannot render before every frame is committed",
                stage="pipeline.render",
                context={"committed": len(estimates), "total": self.total_frames},
            )
        render_result = self._render_estimates(estimates)
        return PipelineRun(
            complete=True,
            committed_frames=len(estimates),
            total_frames=self.total_frames,
            input_hash=self.input_hash,
            snapshot_reason=self.snapshot.reason,
            decision=self.decision.to_dict(),
            estimates=estimates,
            render_result=render_result,
            progress=self.progress(),
            checkpoint_path=str(self.checkpoint_path),
            cancelled=False,
        )

    def _partial_run(self, *, cancelled: bool = False) -> PipelineRun:
        progress = self.progress()
        return PipelineRun(
            complete=False,
            committed_frames=int(progress["committed_frames"]),
            total_frames=self.total_frames,
            input_hash=self.input_hash,
            snapshot_reason=self.snapshot.reason,
            decision=self.decision.to_dict(),
            estimates=self.state.frozen_estimates(),
            render_result=None,
            progress=progress,
            checkpoint_path=str(self.checkpoint_path),
            cancelled=cancelled,
        )

    def _render_estimates(self, estimates: list[FrameEstimate]) -> RenderResult:
        def worker(record: FrameEstimate) -> FrameContribution:
            return self.renderer.render_frame_contribution(
                self.sample,
                record,
                self.render_config,
                mapping=self.mapping,
                block_indices=self.block_plan.frame_blocks[record.frame_index],
            )

        contributions = list(
            self.render_runner.map_ordered(
                sorted(estimates, key=lambda item: item.frame_index),
                worker,
                buffer_estimator=lambda _: self._thread_buffer_bytes(),
            )
        )
        return self.renderer.assemble(
            self.sample,
            contributions,
            self.render_config,
            mapping=self.mapping,
        )

    def full_recompute_from_checkpoint(self) -> PipelineRun:
        snapshot = self.store.load_snapshot(
            input_hash=self.input_hash,
            algorithm_version=ALGORITHM_VERSION,
            total_frames=self.total_frames,
        )
        if not snapshot.valid:
            raise error(
                E_STATE,
                "checkpoint snapshot is not usable for full recomputation",
                stage="pipeline.full_recompute",
                context={"reason": snapshot.reason, "input_hash": self.input_hash},
            )
        if snapshot.committed_count != self.total_frames:
            raise error(
                E_STATE,
                "checkpoint is incomplete; full recomputation cannot fabricate missing frames",
                stage="pipeline.full_recompute",
                frame_index=snapshot.committed_count,
                context={
                    "committed": snapshot.committed_count,
                    "total": self.total_frames,
                },
            )
        persisted = [record["estimate"] for record in snapshot.committed_records]
        estimates = [FrameEstimate.from_dict(record) for record in persisted]
        result = self._render_estimates(estimates)
        return PipelineRun(
            complete=True,
            committed_frames=len(estimates),
            total_frames=self.total_frames,
            input_hash=self.input_hash,
            snapshot_reason="loaded persisted parameters",
            decision=self.decision.to_dict(),
            estimates=estimates,
            render_result=result,
            progress=self.state.progress(),
            checkpoint_path=str(self.checkpoint_path),
            cancelled=False,
        )

    def update_uncommitted_analysis_config(
        self,
        requested: AnalysisConfig,
    ) -> dict[str, Any]:
        validate_analysis_config(requested)
        effective, decision = resolve_analysis_config(self.sample, requested)
        if effective.frame_length != self.effective_config.frame_length:
            raise error(
                E_STATE,
                "frame_length is frozen once the checkpoint exists",
                stage="pipeline.update_config",
                field="frame_length",
                context={
                    "current": self.effective_config.frame_length,
                    "requested": effective.frame_length,
                },
            )
        if effective.hop_length != self.effective_config.hop_length:
            raise error(
                E_STATE,
                "hop_length is frozen once the checkpoint exists",
                stage="pipeline.update_config",
                field="hop_length",
                context={
                    "current": self.effective_config.hop_length,
                    "requested": effective.hop_length,
                },
            )
        committed = self.state.progress()["committed_frames"]
        for frame_index in range(committed, self.total_frames):
            if self.state.state(frame_index) is FrameState.ESTIMATED:
                self.state.revise_uncommitted(frame_index)
        self.requested_analysis = requested
        self.effective_config = effective
        self.decision = decision
        self._initialize_store()
        self.analysis_runner = BoundedParallelRunner(
            effective.max_parallelism,
            effective.thread_buffer_mb * 1024 * 1024,
            stage="pipeline.analysis_parallel",
        )
        self.render_runner = BoundedParallelRunner(
            effective.max_parallelism,
            effective.thread_buffer_mb * 1024 * 1024,
            stage="pipeline.render_parallel",
        )
        return {
            "committed_frames_frozen": committed,
            "decision": decision.to_dict(),
            "input_hash": self.input_hash,
        }

    def update_render_config(self, config: RenderConfig) -> None:
        validate_render_config(config)
        if self.state.progress()["committed_frames"] > 0:
            raise error(
                E_STATE,
                "render configuration is frozen after the first commit",
                stage="pipeline.update_render_config",
                field="render_config",
                context={"committed_frames": self.state.progress()["committed_frames"]},
            )
        self.render_config = config
        self.mapping = resolve_channel_mapping(self.sample, config)
        self.block_plan = make_block_plan(
            self.sample.dry.size,
            max(1, self.total_frames),
            config.render_block_size,
        )
        self._initialize_store()

    def retry_frame(
        self,
        frame_index: int,
        *,
        progress_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> PipelineRun:
        state = self.state.state(frame_index)
        if state is FrameState.FAILED:
            self.state.retry(frame_index)
        elif state is not FrameState.PENDING:
            raise error(
                E_STATE,
                "only failed or pending frames can be retried",
                stage="pipeline.retry",
                frame_index=frame_index,
                context={"state": state.value},
            )
        committed = self.state.progress()["committed_frames"]
        if frame_index != committed:
            raise error(
                E_STATE,
                "retry must target the first uncommitted frame",
                stage="pipeline.retry",
                frame_index=frame_index,
                context={"first_uncommitted": committed},
            )
        try:
            estimate = self.analyzer.estimate_frame(
                self.sample,
                self.effective_config,
                frame_index,
                resolution=self.decision.to_dict(),
            )
        except WorkbenchError as exc:
            exc.add_stage("pipeline.retry_analysis", context={"frame_index": frame_index})
            self.state.mark_failed(frame_index, exc.to_dict())
            raise
        self.state.mark_estimated(frame_index, estimate)
        self.store.commit_frame(
            frame_index=frame_index,
            sequence=frame_index,
            estimate=estimate,
            input_hash=self.input_hash,
            algorithm_version=ALGORITHM_VERSION,
        )
        self.state.mark_committed(frame_index)
        if progress_callback:
            progress_callback(self.progress())
        if self.state.progress()["committed_frames"] == self.total_frames:
            return self._complete_run()
        return self._partial_run()

    def close(self) -> None:
        self.store.close()

    def __enter__(self) -> "AcousticPipeline":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()
