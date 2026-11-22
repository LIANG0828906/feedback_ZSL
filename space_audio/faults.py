from __future__ import annotations

import dataclasses
from enum import Enum

import numpy as np

from .config import WorkbenchConfig
from .errors import WorkbenchError, para_error
from .model import AcousticSample, ConstructionChannel, DirectEvent


class FaultKind(str, Enum):
    CHANNEL_MISSING = "channel_missing"
    TIMING_INVERSION = "timing_inversion"
    GAIN_OVERFLOW = "gain_overflow"
    BAD_FRAME = "bad_frame"
    BOUND_VIOLATION = "bound_violation"


def _channel_index(sample: AcousticSample, channel: str | int) -> int:
    if isinstance(channel, int):
        if not 0 <= channel < sample.channel_count:
            raise para_error("channel index is outside the sample", "channel")
        return channel
    ids = list(sample.layout.ids)
    if channel not in ids:
        raise para_error(f"unknown channel {channel!r}", "channel")
    return ids.index(channel)


def inject_fault(
    sample: AcousticSample,
    kind: FaultKind | str,
    *,
    channel: str | int = 0,
    frame_index: int = 0,
    factor: float = 8.0,
    config: WorkbenchConfig | None = None,
) -> AcousticSample:
    """Create a faulty in-memory input; the original sample is never mutated."""

    try:
        fault = FaultKind(kind)
    except ValueError as exc:
        raise para_error("unknown fault kind", "fault_kind") from exc
    index = _channel_index(sample, channel)
    if not 0 <= frame_index < 1_000_000:
        raise para_error("frame_index must be non-negative", "frame_index")
    if config is None:
        config = WorkbenchConfig()
    config.validate()
    frame_len = config.analysis.frame_len
    hop = config.analysis.hop
    frame_start = frame_index * hop
    ir = np.array(sample.ir, copy=True)
    construction = list(sample.construction)

    if fault == FaultKind.CHANNEL_MISSING:
        if ir.shape[0] <= 1:
            raise para_error("cannot remove the final channel", "channel")
        ir = np.delete(ir, index, axis=0)
    elif fault == FaultKind.TIMING_INVERSION:
        source = construction[index]
        current_events = [
            event
            for event in source.direct_events
            if event.source_frame == frame_index
        ]
        previous_events = [
            event
            for event in source.direct_events
            if event.source_frame < frame_index
        ]
        if not current_events:
            raise para_error(
                "no analytic direct event exists for the requested frame",
                "frame_index",
            )
        if previous_events:
            inverted_position = min(
                event.position_samples for event in previous_events
            ) - 4.0
        else:
            inverted_position = max(0.0, current_events[0].position_samples - 8.0)
        new_events = []
        changed = False
        for event in source.direct_events:
            if event.source_frame == frame_index and not changed:
                new_events.append(
                    dataclasses.replace(
                        event,
                        position_samples=float(inverted_position),
                    )
                )
                changed = True
            else:
                new_events.append(event)
        construction[index] = ConstructionChannel(
            channel_id=source.channel_id,
            direct_events=tuple(new_events),
            reflections=source.reflections,
            tails=source.tails,
        )
    elif fault == FaultKind.GAIN_OVERFLOW:
        position = min(ir.shape[1] - 1, frame_start + 28)
        ir[index, position] = max(
            ir[index, position] * factor,
            factor,
        )
    elif fault == FaultKind.BAD_FRAME:
        end = min(ir.shape[1], frame_start + frame_len)
        if frame_start >= ir.shape[1]:
            raise para_error("frame starts beyond the IR", "frame_index")
        ir[index, frame_start:end] = np.nan
    elif fault == FaultKind.BOUND_VIOLATION:
        source = construction[index]
        new_events: list[DirectEvent] = []
        changed = False
        for event in source.direct_events:
            if event.source_frame == frame_index and not changed:
                event = dataclasses.replace(
                    event,
                    position_samples=event.position_samples + 10.0,
                )
                changed = True
            new_events.append(event)
        if not changed:
            raise para_error(
                "no analytic event exists for the requested frame",
                "frame_index",
            )
        construction[index] = ConstructionChannel(
            channel_id=source.channel_id,
            direct_events=tuple(new_events),
            reflections=source.reflections,
            tails=source.tails,
        )
    return dataclasses.replace(
        sample,
        ir=ir,
        construction=tuple(construction),
    )


def expect_error_code(callable_obj, expected: str):
    try:
        callable_obj()
    except WorkbenchError as exc:
        if exc.code.value != expected:
            raise AssertionError(
                f"expected {expected}, got {exc.code.value}: {exc}"
            ) from exc
        return exc
    raise AssertionError(f"expected {expected}, call succeeded")
