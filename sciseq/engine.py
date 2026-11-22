"""Deterministic frame processing engine and three execution paths."""

from __future__ import annotations

import threading
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .checkpoint import CheckpointStore, SnapshotLoad, SnapshotRecord
from .config import ALGORITHM_VERSION, PipelineConfig, RegistrationConfig
from .denoising import adaptive_denoise, estimate_noise
from .errors import SciSeqError
from .features import (
    aggregate_statistics,
    detection_threshold,
    extract_features,
    frame_statistics,
)
from .generator import fourier_shift
from .metrics import feature_center_errors, quality_metrics
from .models import (
    ExtractedFeature,
    FeatureSpec,
    FrameResult,
    NoiseEstimate,
    Sequence,
    SequenceResult,
    array_sha256,
)
from .planner import ModeDecision, plan_mode
from .registration import estimate_translation, translate_to_reference
from .state import (
    CommittedPayload,
    FrameStateMachine,
    FrameStatus,
    progress as state_progress,
)

ProgressCallback = Callable[[int, int, FrameResult], None]
StateCallback = Callable[[int, str], None]


@dataclass
class RunOutcome:
    records: list[FrameResult]
    complete: bool
    paused: bool
    cancelled: bool
    snapshot_reason: str
    invalidated: bool
    quarantine_path: Path | None
    result: SequenceResult | None

    @property
    def committed_count(self) -> int:
        return len(self.records)


def downsample_image(image: np.ndarray, factor: int) -> np.ndarray:
    if factor <= 1:
        return np.ascontiguousarray(image, dtype=np.float32)
    height, width = image.shape
    down_height = height // factor
    down_width = width // factor
    if down_height < 1 or down_width < 1:
        raise ValueError("downsample factor is too large for frame")
    cropped = image[: down_height * factor, : down_width * factor].astype(np.float64)
    reduced = cropped.reshape(down_height, factor, down_width, factor).mean(axis=(1, 3))
    return reduced.astype(np.float32)


def _shift_feature(feature: FeatureSpec, dx: float, dy: float) -> FeatureSpec:
    endpoints = None
    if feature.endpoints is not None:
        endpoints = (
            (feature.endpoints[0][0] + dx, feature.endpoints[0][1] + dy),
            (feature.endpoints[1][0] + dx, feature.endpoints[1][1] + dy),
        )
    return FeatureSpec(
        feature_id=feature.feature_id,
        kind=feature.kind,
        center=(feature.center[0] + dx, feature.center[1] + dy),
        sigma=feature.sigma,
        amplitude=feature.amplitude,
        endpoints=endpoints,
    )


class SequenceProcessor:
    def __init__(
        self,
        sequence: Sequence,
        config: PipelineConfig,
        *,
        checkpoint_dir: Path | str | None = None,
        include_quality: bool = True,
        state_callback: StateCallback | None = None,
    ) -> None:
        config.validate(sequence.shape, sequence.frame_count)
        self.sequence = sequence
        self.config = config
        self.checkpoint_dir = None if checkpoint_dir is None else Path(checkpoint_dir)
        self.include_quality = bool(include_quality)
        self.state_callback = state_callback
        self.reference_index = int(config.reference_frame)
        self._reference_frame = sequence.frame(self.reference_index)
        reference_shift = sequence.truth.shifts[self.reference_index]
        self._reference_shift = (float(reference_shift[0]), float(reference_shift[1]))
        self._truth_features = tuple(
            _shift_feature(feature, *self._reference_shift) for feature in sequence.truth.features
        )
        clean_base = sequence.clean_base()
        self._clean_reference = (
            clean_base
            if self._reference_shift == (0.0, 0.0)
            else fourier_shift(clean_base, *self._reference_shift)
        )
        self.parameter_version = config.parameter_version()
        self.algorithm_version = ALGORITHM_VERSION

    def plan(self) -> ModeDecision:
        return plan_mode(self.sequence.frame_count, self.sequence.shape, self.config)

    def _make_store(self) -> CheckpointStore:
        if self.checkpoint_dir is None:
            raise ValueError("checkpoint_dir is not configured")
        return CheckpointStore(
            self.checkpoint_dir,
            sequence_fingerprint=self.sequence.fingerprint(),
            parameter_version=self.parameter_version,
            algorithm_version=self.algorithm_version,
            sequence_id=self.sequence.sequence_id,
            frame_count=self.sequence.frame_count,
            shape=self.sequence.shape,
        )

    def _register(self, index: int, moving: np.ndarray) -> tuple[tuple[float, float], np.ndarray]:
        if index == self.reference_index:
            return (0.0, 0.0), np.ascontiguousarray(moving, dtype=np.float32)
        estimate = estimate_translation(
            self._reference_frame,
            moving,
            self.config.registration,
            frame_index=index,
        )
        aligned = translate_to_reference(moving, estimate.offset)
        return estimate.offset, aligned

    def _process_frame(
        self,
        index: int,
        state: FrameStateMachine | None = None,
    ) -> FrameResult:
        moving = self.sequence.frame(index)
        try:
            offset, aligned = self._register(index, moving)
        except Exception as exc:
            if state is not None:
                try:
                    state.fail(str(exc))
                except SciSeqError:
                    pass
                if self.state_callback is not None:
                    self.state_callback(index, FrameStatus.FAILED.value)
            raise
        if state is not None:
            state.transition(FrameStatus.REGISTERED)
            if self.state_callback is not None:
                self.state_callback(index, FrameStatus.REGISTERED.value)
            state.transition(FrameStatus.DENOISING)
            if self.state_callback is not None:
                self.state_callback(index, FrameStatus.DENOISING.value)
        try:
            noise = estimate_noise(aligned, self.config.denoise.block_size)
            denoised, denoise_details = adaptive_denoise(
                aligned,
                self.config.denoise,
                noise,
            )
            features = extract_features(denoised, noise, frame_index=index)
            threshold = detection_threshold(denoised, noise)
            stats = frame_statistics(index, features, noise, offset, threshold)
            stats["denoise"] = denoise_details
            stats["registration"] = {
                "offset": [float(offset[0]), float(offset[1])],
                "reference_frame": int(self.reference_index),
            }
            if self.include_quality:
                quality = quality_metrics(denoised, self._clean_reference, self._truth_features)
                center_errors = feature_center_errors(
                    features,
                    self._truth_features,
                    index,
                )
                stats["quality"] = quality
                stats["truth_feature_match"] = center_errors
        except Exception as exc:
            if state is not None:
                try:
                    state.fail(str(exc))
                except SciSeqError:
                    pass
                if self.state_callback is not None:
                    self.state_callback(index, FrameStatus.FAILED.value)
            raise
        return FrameResult(
            index=index,
            offset=np.asarray(offset, dtype=np.float64),
            denoised=np.ascontiguousarray(denoised, dtype=np.float32),
            noise=noise,
            background=float(noise.background),
            features=features,
            stats=stats,
            content_sha256=self.sequence.frame_hashes[index],
            denoised_sha256=array_sha256(denoised),
        )

    def _restored_result(self, record: SnapshotRecord) -> FrameResult:
        return FrameResult(
            index=record.index,
            offset=np.asarray(record.offset, dtype=np.float64),
            denoised=np.ascontiguousarray(record.denoised, dtype=np.float32),
            noise=NoiseEstimate(
                record.noise_sigma,
                self.config.denoise.block_size,
                np.zeros((0, 0), dtype=np.float32),
                record.background,
            ),
            background=record.background,
            features=record.features(),
            stats=record.stats,
            content_sha256=record.content_sha256,
            denoised_sha256=record.denoised_sha256,
        )

    def _restore_states(
        self,
        records: list[SnapshotRecord],
    ) -> tuple[list[FrameResult], list[FrameStateMachine]]:
        restored_results: list[FrameResult] = []
        states: list[FrameStateMachine] = []
        for index in range(self.sequence.frame_count):
            if index < len(records):
                record = records[index]
                result = self._restored_result(record)
                restored_results.append(result)
                state = FrameStateMachine(index, FrameStatus.DENOISING)
                state.commit(
                    CommittedPayload(
                        offset=(
                            float(record.offset[0]),
                            float(record.offset[1]),
                        ),
                        denoised_sha256=record.denoised_sha256,
                        content_sha256=record.content_sha256,
                        parameter_version=record.parameter_version,
                        algorithm_version=record.algorithm_version,
                        extra={"restored": True, "snapshot": True},
                    )
                )
            else:
                state = FrameStateMachine(index)
            states.append(state)
        return restored_results, states

    def _commit_frame(
        self,
        result: FrameResult,
        state: FrameStateMachine,
        store: CheckpointStore | None,
    ) -> None:
        try:
            if store is not None:
                digest = store.commit(
                    result.index,
                    offset=result.offset,
                    denoised=result.denoised,
                    noise_sigma=result.noise.sigma,
                    background=result.background,
                    stats=result.stats,
                    content_sha256=result.content_sha256,
                )
                if digest != result.denoised_sha256:
                    raise SciSeqError(
                        "E_CHECKPOINT",
                        "checkpoint digest does not match frame result",
                        frame=result.index,
                        field="denoised_sha256",
                    )
            state.commit(
                CommittedPayload(
                    offset=(float(result.offset[0]), float(result.offset[1])),
                    denoised_sha256=result.denoised_sha256,
                    content_sha256=result.content_sha256,
                    parameter_version=self.parameter_version,
                    algorithm_version=self.algorithm_version,
                    extra={"noise_sigma": float(result.noise.sigma)},
                )
            )
            if self.state_callback is not None:
                self.state_callback(result.index, FrameStatus.COMMITTED.value)
        except Exception:
            if state.status != FrameStatus.COMMITTED:
                try:
                    state.fail("commit boundary failed")
                except SciSeqError:
                    pass
                if self.state_callback is not None:
                    self.state_callback(result.index, FrameStatus.FAILED.value)
            raise

    def _serial_execute(
        self,
        states: list[FrameStateMachine],
        records: list[FrameResult],
        store: CheckpointStore | None,
        pause_after: int | None,
        pause_event: threading.Event | None,
        cancel_event: threading.Event | None,
        progress_callback: ProgressCallback | None,
    ) -> tuple[list[FrameResult], str | None]:
        committed = len(records)
        start = len(records)
        for index in range(start, self.sequence.frame_count):
            if cancel_event is not None and cancel_event.is_set():
                return records, "cancelled"
            if pause_event is not None and pause_event.is_set():
                return records, "paused"
            if pause_after is not None and committed >= pause_after:
                return records, "paused"
            result = self._process_frame(index, states[index])
            self._commit_frame(result, states[index], store)
            records.append(result)
            committed += 1
            if progress_callback is not None:
                progress_callback(committed, self.sequence.frame_count, result)
        return records, None

    def _parallel_execute(
        self,
        states: list[FrameStateMachine],
        records: list[FrameResult],
        store: CheckpointStore | None,
        workers: int,
        frames_per_worker: int,
        pause_after: int | None,
        pause_event: threading.Event | None,
        cancel_event: threading.Event | None,
        progress_callback: ProgressCallback | None,
    ) -> tuple[list[FrameResult], str | None]:
        pending = list(range(len(records), self.sequence.frame_count))
        if not pending:
            return records, None
        outstanding_limit = max(1, int(workers) * int(frames_per_worker))
        ready: dict[int, FrameResult] = {}
        futures: dict[Future[FrameResult], int] = {}
        submit_position = 0
        commit_position = 0
        stop_reason: str | None = None
        failure: BaseException | None = None

        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="sciseq") as executor:
            while commit_position < len(pending):
                if stop_reason is None:
                    if cancel_event is not None and cancel_event.is_set():
                        stop_reason = "cancelled"
                    elif pause_event is not None and pause_event.is_set():
                        stop_reason = "paused"
                    elif pause_after is not None and len(records) >= pause_after:
                        stop_reason = "paused"
                while (
                    stop_reason is None
                    and submit_position < len(pending)
                    and len(futures) < outstanding_limit
                ):
                    index = pending[submit_position]
                    future = executor.submit(self._process_frame, index, states[index])
                    futures[future] = index
                    submit_position += 1

                if stop_reason is not None:
                    for future in futures:
                        future.cancel()
                    break
                if not futures:
                    break
                completed, _ = wait(futures, return_when=FIRST_COMPLETED)
                for future in sorted(completed, key=lambda item: futures[item]):
                    index = futures.pop(future)
                    try:
                        ready[index] = future.result()
                    except BaseException as exc:
                        failure = exc
                        stop_reason = "failed"
                        for pending_future in futures:
                            pending_future.cancel()
                        break
                if failure is not None:
                    break
                while commit_position < len(pending):
                    index = pending[commit_position]
                    result = ready.pop(index, None)
                    if result is None:
                        break
                    self._commit_frame(result, states[index], store)
                    records.append(result)
                    commit_position += 1
                    if progress_callback is not None:
                        progress_callback(len(records), self.sequence.frame_count, result)
                    if pause_after is not None and len(records) >= pause_after:
                        stop_reason = "paused"
                        break
        if failure is not None:
            raise failure
        return records, stop_reason

    def _execute(
        self,
        states: list[FrameStateMachine],
        records: list[FrameResult],
        store: CheckpointStore | None,
        *,
        parallel: bool,
        pause_after: int | None,
        pause_event: threading.Event | None,
        cancel_event: threading.Event | None,
        progress_callback: ProgressCallback | None,
    ) -> tuple[list[FrameResult], str | None]:
        workers = self.config.budget.max_workers if parallel else 1
        workers = max(1, min(workers, self.sequence.frame_count))
        if workers == 1:
            return self._serial_execute(
                states,
                records,
                store,
                pause_after,
                pause_event,
                cancel_event,
                progress_callback,
            )
        return self._parallel_execute(
            states,
            records,
            store,
            workers,
            self.config.budget.frames_per_worker,
            pause_after,
            pause_event,
            cancel_event,
            progress_callback,
        )

    def _decision_payload(self, executed_mode: str, downsample: int) -> dict[str, Any]:
        decision = self.plan()
        payload = decision.to_dict()
        payload["auto_selected_mode"] = decision.mode
        payload["executed_mode"] = executed_mode
        payload["executed_downsample_factor"] = int(downsample)
        payload["decision_hash"] = decision.decision_hash()
        payload["parameter_version"] = self.parameter_version
        return payload

    def _build_result(
        self,
        records: list[FrameResult],
        *,
        mode: str = "full",
        downsample_factor: int = 1,
        denoised_mask: np.ndarray | None = None,
        preview: bool = False,
        stats_override: dict[str, Any] | None = None,
    ) -> SequenceResult:
        if len(records) != self.sequence.frame_count:
            raise ValueError("cannot build a complete result from partial records")
        ordered = sorted(records, key=lambda item: item.index)
        offsets = np.stack([record.offset for record in ordered]).astype(np.float64)
        denoised = np.stack([record.denoised for record in ordered]).astype(np.float32)
        noise_sigmas = np.asarray([record.noise.sigma for record in ordered], dtype=np.float64)
        backgrounds = np.asarray([record.background for record in ordered], dtype=np.float64)
        features = tuple(tuple(record.features) for record in ordered)
        frame_stats = [record.stats for record in ordered]
        stats = stats_override if stats_override is not None else aggregate_statistics(frame_stats)
        if stats_override is None and self.include_quality:
            residual_values = [
                float(record.stats["quality"]["residual_std"])
                for record in ordered
                if "quality" in record.stats
            ]
            edge_values = [
                float(record.stats["quality"]["edge_fidelity"])
                for record in ordered
                if "quality" in record.stats
            ]
            if residual_values:
                stats["quality_summary"] = {
                    "residual_std_mean": float(np.mean(residual_values)),
                    "residual_std_max": float(np.max(residual_values)),
                    "edge_fidelity_min": float(np.min(edge_values)),
                    "edge_fidelity_mean": float(np.mean(edge_values)),
                }
        mask = (
            np.ones(self.sequence.frame_count, dtype=bool)
            if denoised_mask is None
            else np.asarray(denoised_mask, dtype=bool)
        )
        return SequenceResult(
            sequence_id=self.sequence.sequence_id,
            shape=(int(denoised.shape[1]), int(denoised.shape[2])),
            frame_count=self.sequence.frame_count,
            offsets=offsets,
            denoised=denoised,
            noise_sigmas=noise_sigmas,
            backgrounds=backgrounds,
            features=features,
            stats=stats,
            mode=mode,
            downsample_factor=downsample_factor,
            denoised_mask=mask,
            decision=self._decision_payload(mode, downsample_factor),
            parameter_version=self.parameter_version,
            algorithm_version=self.algorithm_version,
            preview=preview,
        )

    def run_full(
        self,
        *,
        parallel: bool = True,
        progress_callback: ProgressCallback | None = None,
        cancel_event: threading.Event | None = None,
    ) -> SequenceResult:
        states = [FrameStateMachine(index) for index in range(self.sequence.frame_count)]
        records, reason = self._execute(
            states,
            [],
            None,
            parallel=parallel,
            pause_after=None,
            pause_event=None,
            cancel_event=cancel_event,
            progress_callback=progress_callback,
        )
        if reason is not None or len(records) != self.sequence.frame_count:
            raise SciSeqError(
                "E_STATE",
                "full run did not reach all committed frames",
                field="progress",
                details={"committed": len(records), "total": self.sequence.frame_count},
            )
        return self._build_result(records)

    def run_incremental(
        self,
        *,
        resume: bool = True,
        parallel: bool = True,
        pause_after: int | None = None,
        pause_event: threading.Event | None = None,
        cancel_event: threading.Event | None = None,
        progress_callback: ProgressCallback | None = None,
        reset: bool = False,
    ) -> RunOutcome:
        store = self._make_store()
        snapshot: SnapshotLoad
        if reset or not resume:
            snapshot = store.reset("fresh incremental run")
            snapshot.records = []
        else:
            snapshot = store.load_and_validate(self.sequence.frame_hashes)
        restored_records, states = self._restore_states(snapshot.records)
        records = list(restored_records)
        reason = None
        if len(records) < self.sequence.frame_count:
            records, reason = self._execute(
                states,
                records,
                store,
                parallel=parallel,
                pause_after=pause_after,
                pause_event=pause_event,
                cancel_event=cancel_event,
                progress_callback=progress_callback,
            )
        complete = len(records) == self.sequence.frame_count
        result = self._build_result(records) if complete else None
        return RunOutcome(
            records=sorted(records, key=lambda item: item.index),
            complete=complete,
            paused=reason == "paused",
            cancelled=reason == "cancelled",
            snapshot_reason=snapshot.reason,
            invalidated=snapshot.invalidated,
            quarantine_path=snapshot.quarantine_path,
            result=result,
        )

    def run_resume(self, **kwargs: Any) -> RunOutcome:
        kwargs["resume"] = True
        return self.run_incremental(**kwargs)

    def run_preview(
        self,
        *,
        progress_callback: ProgressCallback | None = None,
    ) -> SequenceResult:
        decision = self.plan()
        if decision.mode == "full" or decision.downsample_factor <= 1:
            return self.run_full(
                parallel=True,
                progress_callback=progress_callback,
            )
        factor = int(decision.downsample_factor)
        down_shape = (self.sequence.shape[0] // factor, self.sequence.shape[1] // factor)
        reference = downsample_image(self._reference_frame, factor)
        registration_config = replace(
            self.config.registration,
            max_shift=max(0.5, self.config.registration.max_shift / factor),
        )
        block_size = max(
            4,
            min(down_shape[0], down_shape[1], self.config.denoise.block_size // factor),
        )
        denoise_config = replace(self.config.denoise, block_size=block_size)

        provisional: list[tuple[int, np.ndarray, tuple[float, float], np.ndarray, NoiseEstimate]] = []
        for index in range(self.sequence.frame_count):
            moving = downsample_image(self.sequence.frame(index), factor)
            if index == self.reference_index:
                low_offset = (0.0, 0.0)
                aligned = moving
            else:
                estimate = estimate_translation(
                    reference,
                    moving,
                    registration_config,
                    frame_index=index,
                )
                low_offset = estimate.offset
                aligned = translate_to_reference(moving, low_offset)
            noise = estimate_noise(aligned, block_size)
            provisional.append((index, aligned, low_offset, moving, noise))
            if progress_callback is not None:
                progress_callback(index + 1, self.sequence.frame_count, None)  # type: ignore[arg-type]

        quota = min(decision.preview_denoise_quota, self.sequence.frame_count)
        ranking = sorted(
            range(self.sequence.frame_count),
            key=lambda index: (-provisional[index][4].sigma, index),
        )
        selected = set(ranking[:quota])
        records: list[FrameResult] = []
        for index, aligned, low_offset, moving, noise in provisional:
            if index in selected:
                denoised, details = adaptive_denoise(aligned, denoise_config, noise)
            else:
                denoised = aligned
                details = {
                    "filter": denoise_config.filter,
                    "measured_noise_sigma": float(noise.sigma),
                    "selective_denoise_skipped": 1,
                }
            features = extract_features(denoised, noise, frame_index=index)
            threshold = detection_threshold(denoised, noise)
            high_offset = (low_offset[0] * factor, low_offset[1] * factor)
            stats = frame_statistics(index, features, noise, high_offset, threshold)
            stats["denoise"] = details
            stats["preview"] = {
                "downsample_factor": factor,
                "selective_denoise": index in selected,
            }
            records.append(
                FrameResult(
                    index=index,
                    offset=np.asarray(high_offset, dtype=np.float64),
                    denoised=np.ascontiguousarray(denoised, dtype=np.float32),
                    noise=noise,
                    background=float(noise.background),
                    features=features,
                    stats=stats,
                    content_sha256=self.sequence.frame_hashes[index],
                    denoised_sha256=array_sha256(denoised),
                )
            )
        mask = np.asarray([index in selected for index in range(self.sequence.frame_count)], dtype=bool)
        stats = aggregate_statistics([record.stats for record in records])
        stats["selective_denoise_frames"] = [int(index) for index in sorted(selected)]
        stats["preview_downsample_factor"] = factor
        return self._build_result(
            records,
            mode="preview",
            downsample_factor=factor,
            denoised_mask=mask,
            preview=True,
            stats_override=stats,
        )


class LiveIncrementalSession:
    """In-process incremental session supporting parameter changes at commit boundaries.

    Frames committed before ``update_config`` retain their frozen payloads. New
    commits carry the new parameter version, which intentionally makes the
    snapshot invalid as a whole on any later restart.
    """

    def __init__(
        self,
        sequence: Sequence,
        config: PipelineConfig,
        checkpoint_dir: Path | str,
        *,
        resume: bool = True,
        reset: bool = False,
        include_quality: bool = True,
    ) -> None:
        self.processor = SequenceProcessor(
            sequence,
            config,
            checkpoint_dir=checkpoint_dir,
            include_quality=include_quality,
        )
        self.store = self.processor._make_store()
        if reset or not resume:
            snapshot = self.store.reset("fresh live session")
            snapshot.records = []
        else:
            snapshot = self.store.load_and_validate(sequence.frame_hashes)
        self.records, self.states = self.processor._restore_states(snapshot.records)
        self.snapshot = snapshot
        self.parameter_versions = [self.processor.parameter_version]
        self.config_updates: list[dict[str, Any]] = []
        self.mixed_parameter_versions = False

    @property
    def committed_count(self) -> int:
        return len(self.records)

    def update_config(self, config: PipelineConfig) -> dict[str, Any]:
        if config.reference_frame != self.processor.config.reference_frame:
            raise SciSeqError(
                "E_PARA",
                "reference_frame cannot change within a live session",
                field="reference_frame",
                details={
                    "current": self.processor.config.reference_frame,
                    "requested": config.reference_frame,
                },
            )
        config.validate(self.processor.sequence.shape, self.processor.sequence.frame_count)
        previous = self.processor.parameter_version
        self.processor.config = config
        self.processor.parameter_version = config.parameter_version()
        self.store.parameter_version = self.processor.parameter_version
        if self.processor.parameter_version != previous:
            self.parameter_versions.append(self.processor.parameter_version)
            self.mixed_parameter_versions = self.committed_count > 0
        update = {
            "previous_parameter_version": previous,
            "parameter_version": self.processor.parameter_version,
            "frozen_committed_frames": self.committed_count,
            "applies_from_frame": self.committed_count,
        }
        self.config_updates.append(update)
        return update

    def advance(
        self,
        *,
        frame_count: int | None = None,
        parallel: bool = True,
        pause_event: threading.Event | None = None,
        cancel_event: threading.Event | None = None,
        progress_callback: ProgressCallback | None = None,
    ) -> RunOutcome:
        start = len(self.records)
        pause_after = None if frame_count is None else min(
            self.processor.sequence.frame_count,
            start + max(0, int(frame_count)),
        )
        reason = None
        if pause_after is None or pause_after > start:
            self.records, reason = self.processor._execute(
                self.states,
                self.records,
                self.store,
                parallel=parallel,
                pause_after=pause_after,
                pause_event=pause_event,
                cancel_event=cancel_event,
                progress_callback=progress_callback,
            )
        complete = len(self.records) == self.processor.sequence.frame_count
        result = self.processor._build_result(self.records) if complete else None
        if result is not None:
            result.stats["parameter_epochs"] = list(self.parameter_versions)
            result.stats["config_updates"] = list(self.config_updates)
            if self.mixed_parameter_versions:
                result.parameter_version = "mixed:" + ":".join(self.parameter_versions)
                result.decision["mixed_parameter_versions"] = True
        return RunOutcome(
            records=sorted(self.records, key=lambda item: item.index),
            complete=complete,
            paused=reason == "paused",
            cancelled=reason == "cancelled",
            snapshot_reason=self.snapshot.reason,
            invalidated=self.snapshot.invalidated,
            quarantine_path=self.snapshot.quarantine_path,
            result=result,
        )

