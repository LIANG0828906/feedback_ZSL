from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass

from .errors import ErrorCode, HapticError
from .models import DeviceConfig, WaveSequence
from .sampler import (
    CancelToken,
    SamplingResult,
    SegmentSignal,
    assemble_result,
    complement_ranges,
    compute_segment_signal,
    segment_bounds,
    segment_factor_hash,
    total_frames,
    transition_ranges,
    validate_for_sampling,
)

SegmentCheckpoint = Callable[[SegmentSignal], None]


@dataclass(frozen=True, slots=True)
class RecomputePlan:
    sequence: WaveSequence
    config: DeviceConfig
    frame_count: int
    affected_segments: tuple[int, ...]
    recompute_segments: tuple[int, ...]
    reuse_segments: tuple[int, ...]
    affected_ranges: tuple[tuple[int, int], ...]
    reused_ranges: tuple[tuple[int, int], ...]
    full_recompute: bool
    full_reason: str | None
    reasons: tuple[str, ...]
    previous: SamplingResult | None


@dataclass(frozen=True, slots=True)
class ExecutionResult:
    sampling: SamplingResult
    computed_segments: tuple[int, ...]
    restored_from_checkpoint: tuple[int, ...]
    reused_segments: tuple[int, ...]
    cancelled: bool = False


def _segment_map(sequence: WaveSequence) -> dict[int, object]:
    return {segment.index: segment for segment in sequence.segments}


def _config_global_reasons(
    previous: DeviceConfig, current: DeviceConfig
) -> tuple[list[str], bool]:
    reasons: list[str] = []
    full = False
    if previous.sample_rate != current.sample_rate:
        reasons.append("sample_rate_changed")
        full = True
    if previous.bit_depth != current.bit_depth:
        reasons.append("bit_depth_changed")
        full = True
    if previous.limiter != current.limiter:
        reasons.append("limiter_changed")
        full = True
    if (
        previous.left_ratio != current.left_ratio
        or previous.right_ratio != current.right_ratio
    ):
        reasons.append("allocation_changed")
    if (
        previous.left_polarity != current.left_polarity
        or previous.right_polarity != current.right_polarity
    ):
        reasons.append("polarity_changed")
    return reasons, full


def build_plan(
    sequence: WaveSequence,
    config: DeviceConfig,
    previous_result: SamplingResult | None,
) -> RecomputePlan:
    validate_for_sampling(sequence, config)
    frame_count = total_frames(sequence, config.sample_rate)
    if previous_result is None:
        indexes = tuple(segment.index for segment in sequence.sorted_segments)
        affected_ranges = ((0, frame_count),) if frame_count else ()
        return RecomputePlan(
            sequence=sequence,
            config=config,
            frame_count=frame_count,
            affected_segments=indexes,
            recompute_segments=indexes,
            reuse_segments=(),
            affected_ranges=affected_ranges,
            reused_ranges=(),
            full_recompute=True,
            full_reason="initial_compute",
            reasons=("initial_compute",),
            previous=None,
        )

    if previous_result.total_frames != frame_count:
        indexes = tuple(segment.index for segment in sequence.sorted_segments)
        return RecomputePlan(
            sequence=sequence,
            config=config,
            frame_count=frame_count,
            affected_segments=indexes,
            recompute_segments=indexes,
            reuse_segments=(),
            affected_ranges=((0, frame_count),) if frame_count else (),
            reused_ranges=(),
            full_recompute=True,
            full_reason="frame_count_changed",
            reasons=("frame_count_changed",),
            previous=previous_result,
        )

    previous_signals = previous_result.segment_by_index
    previous_sequence_segments = {
        signal.index: signal for signal in previous_result.segment_signals
    }
    # The previous SamplingResult intentionally contains only immutable sample
    # data. Recover the prior parameter set through factor hashes and caller
    # supplied previous state stored alongside the result by the engine.
    return _build_from_segment_state(
        sequence,
        config,
        previous_result,
        previous_sequence_segments=previous_sequence_segments,
        previous_signals=previous_signals,
    )


@dataclass(frozen=True, slots=True)
class PreviousState:
    sequence: WaveSequence
    config: DeviceConfig
    result: SamplingResult


def plan_from_previous(
    sequence: WaveSequence,
    config: DeviceConfig,
    previous: PreviousState | None,
) -> RecomputePlan:
    validate_for_sampling(sequence, config)
    if previous is None:
        return build_plan(sequence, config, None)
    return _build_from_segment_state(
        sequence,
        config,
        previous.result,
        previous_sequence_segments={
            s.index: s for s in previous.result.segment_signals
        },
        previous_signals=previous.result.segment_by_index,
        previous_sequence=previous.sequence,
        previous_config=previous.config,
    )


def _build_from_segment_state(
    sequence: WaveSequence,
    config: DeviceConfig,
    previous_result: SamplingResult,
    *,
    previous_sequence_segments: Mapping[int, SegmentSignal],
    previous_signals: Mapping[int, SegmentSignal],
    previous_sequence: WaveSequence | None = None,
    previous_config: DeviceConfig | None = None,
) -> RecomputePlan:
    frame_count = total_frames(sequence, config.sample_rate)
    current_segments = _segment_map(sequence)
    reasons: list[str] = []
    full_reason: str | None = None
    if previous_config is not None:
        config_reasons, full = _config_global_reasons(previous_config, config)
        reasons.extend(config_reasons)
        if full:
            full_reason = config_reasons[0]

    if previous_sequence is not None and (
        previous_sequence.total_duration_ms != sequence.total_duration_ms
        or previous_sequence.schema_version != sequence.schema_version
    ):
        full_reason = full_reason or "sequence_global_changed"
        reasons.append("sequence_global_changed")

    if full_reason is not None:
        indexes = tuple(segment.index for segment in sequence.sorted_segments)
        return RecomputePlan(
            sequence=sequence,
            config=config,
            frame_count=frame_count,
            affected_segments=indexes,
            recompute_segments=indexes,
            reuse_segments=(),
            affected_ranges=((0, frame_count),) if frame_count else (),
            reused_ranges=(),
            full_recompute=True,
            full_reason=full_reason,
            reasons=tuple(dict.fromkeys(reasons)),
            previous=previous_result,
        )

    previous_segment_map = (
        _segment_map(previous_sequence) if previous_sequence is not None else {}
    )
    changed: set[int] = set()
    affected_ranges: list[tuple[int, int]] = []
    reuse_segments: list[int] = []
    recompute_segments: list[int] = []
    crash_guard = False

    all_indexes = sorted(set(previous_signals) | set(current_segments))
    for index in all_indexes:
        old_segment = previous_segment_map.get(index)
        new_segment = current_segments.get(index)
        old_signal = previous_signals.get(index)
        if old_segment is None and new_segment is None:
            crash_guard = True
            continue
        segment_changed = False
        if (
            old_segment is None
            or new_segment is None
            or old_segment.to_dict() != new_segment.to_dict()
        ):
            segment_changed = True
        elif previous_config is not None and segment_factor_hash(
            new_segment, config
        ) != (old_signal.factor_hash if old_signal is not None else None):
            # Parameter objects may be identical while mapping factors changed.
            segment_changed = True
        if segment_changed:
            changed.add(index)
            recompute_segments.append(index)
            if old_segment is not None:
                start, end = segment_bounds(
                    old_segment, config.sample_rate, frame_count
                )
                if end > start:
                    affected_ranges.append((start, end))
            if new_segment is not None:
                start, end = segment_bounds(
                    new_segment, config.sample_rate, frame_count
                )
                if end > start:
                    affected_ranges.append((start, end))
        else:
            reuse_segments.append(index)

    if crash_guard:
        # This branch is only reachable with malformed internal state. Refuse to
        # claim a safe incremental result when the dependency proof is missing.
        indexes = tuple(segment.index for segment in sequence.sorted_segments)
        return RecomputePlan(
            sequence=sequence,
            config=config,
            frame_count=frame_count,
            affected_segments=indexes,
            recompute_segments=indexes,
            reuse_segments=(),
            affected_ranges=((0, frame_count),) if frame_count else (),
            reused_ranges=(),
            full_recompute=True,
            full_reason="unprovable_dependency",
            reasons=tuple(dict.fromkeys([*reasons, "unprovable_dependency"])),
            previous=previous_result,
        )

    expanded = transition_ranges(affected_ranges, frame_count)
    reused = complement_ranges(expanded, frame_count)
    if changed:
        reasons.append("segment_factors_changed")
    if not reasons:
        reasons.append("no_effective_change")

    return RecomputePlan(
        sequence=sequence,
        config=config,
        frame_count=frame_count,
        affected_segments=tuple(sorted(changed)),
        recompute_segments=tuple(sorted(recompute_segments)),
        reuse_segments=tuple(sorted(reuse_segments)),
        affected_ranges=expanded,
        reused_ranges=reused,
        full_recompute=False,
        full_reason=None,
        reasons=tuple(dict.fromkeys(reasons)),
        previous=previous_result,
    )


def execute_plan(
    plan: RecomputePlan,
    *,
    cancellation: CancelToken | None = None,
    checkpoint: SegmentCheckpoint | None = None,
    restored_signals: Mapping[int, SegmentSignal] | None = None,
) -> ExecutionResult:
    if cancellation is not None:
        cancellation.check()
    restored = dict(restored_signals or {})
    current_segments = {
        segment.index: segment for segment in plan.sequence.sorted_segments
    }
    working_signals: dict[int, SegmentSignal] = {}
    computed: list[int] = []
    restored_used: list[int] = []
    reused: list[int] = []

    if plan.previous is not None:
        working_signals.update(plan.previous.segment_by_index)

    for index in plan.recompute_segments:
        segment = current_segments[index]
        restored_signal = restored.get(index)
        if (
            restored_signal is not None
            and restored_signal.factor_hash == segment_factor_hash(segment, plan.config)
        ):
            working_signals[index] = restored_signal
            restored_used.append(index)
            continue
        if cancellation is not None:
            cancellation.check()
        signal = compute_segment_signal(
            segment,
            plan.sequence,
            plan.config,
            frame_count=plan.frame_count,
            cancellation=cancellation,
        )
        working_signals[index] = signal
        computed.append(index)
        if checkpoint is not None:
            checkpoint(signal)

    for index in plan.reuse_segments:
        if index not in working_signals:
            raise HapticError(
                ErrorCode.RECOVERY_INVALID,
                "reuse signal missing from previous result",
                details={"segment": index},
            )
        reused.append(index)
    for index in plan.affected_segments:
        if index not in current_segments:
            working_signals.pop(index, None)

    final_signals = [
        working_signals[segment.index] for segment in plan.sequence.sorted_segments
    ]
    if plan.full_recompute:
        recompute_ranges = ((0, plan.frame_count),) if plan.frame_count else ()
        previous = None
    else:
        recompute_ranges = plan.affected_ranges
        previous = plan.previous
    sampling = assemble_result(
        plan.sequence,
        plan.config,
        final_signals,
        frame_count=plan.frame_count,
        recompute_ranges=recompute_ranges,
        previous=previous,
        cancellation=cancellation,
    )
    return ExecutionResult(
        sampling=sampling,
        computed_segments=tuple(sorted(computed)),
        restored_from_checkpoint=tuple(sorted(restored_used)),
        reused_segments=tuple(sorted(reused)),
    )


def ranges_to_json(
    ranges: tuple[tuple[int, int], ...],
) -> list[dict[str, int]]:
    return [
        {"start_sample": start, "end_sample": end, "length": end - start}
        for start, end in ranges
    ]
