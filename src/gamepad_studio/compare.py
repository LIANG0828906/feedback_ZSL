"""Full-vs-incremental reconciliation and difference heatmap data."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import DivergenceError
from .replay import Frame, ReplayResult

ACTION_FIELDS = (
    "pressed",
    "active",
    "value",
    "trigger_count",
    "repeat_count",
    "trigger_t_ms",
    "trigger_seq",
    "last_reason",
    "triggered_now",
)


@dataclass(frozen=True)
class DiffReport:
    equal: bool
    first_frame_index: int | None
    event_seq: int | None
    action: str | None
    field: str | None
    left_value: Any
    right_value: Any
    checked_frames: int
    mismatch_count: int
    message: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "first_frame_index": self.first_frame_index,
            "event_seq": self.event_seq,
            "action": self.action,
            "field": self.field,
            "left_value": self.left_value,
            "right_value": self.right_value,
            "checked_frames": self.checked_frames,
            "mismatch_count": self.mismatch_count,
            "message": self.message,
        }


def _values_equal(left: Any, right: Any, tolerance: float) -> bool:
    if isinstance(left, float) or isinstance(right, float):
        if left is None or right is None:
            return left == right
        return math.isclose(
            float(left),
            float(right),
            rel_tol=0.0,
            abs_tol=tolerance,
        )
    return left == right


def _frame_difference(
    left: Frame,
    right: Frame,
    tolerance: float,
) -> tuple[int, str | None, str | None, Any, Any] | None:
    if left.t_ms != right.t_ms:
        return 1, None, "t_ms", left.t_ms, right.t_ms
    if left.tick_key != right.tick_key:
        return 1, None, "tick_key", left.tick_key.to_dict(), right.tick_key.to_dict()
    if left.tick_kind != right.tick_kind:
        return 1, None, "tick_kind", left.tick_kind, right.tick_kind
    if left.timer_rule != right.timer_rule:
        return 1, None, "timer_rule", left.timer_rule, right.timer_rule
    if left.source_event_seq != right.source_event_seq:
        return (
            1,
            None,
            "source_event_seq",
            left.source_event_seq,
            right.source_event_seq,
        )
    for axis_id in sorted(set(left.axis_values) | set(right.axis_values)):
        left_value = left.axis_values.get(axis_id)
        right_value = right.axis_values.get(axis_id)
        if not _values_equal(left_value, right_value, tolerance):
            return 1, None, f"axis.{axis_id}", left_value, right_value
    for action in sorted(set(left.actions) | set(right.actions)):
        left_action = left.actions.get(action)
        right_action = right.actions.get(action)
        if left_action is None or right_action is None:
            return 1, action, "action_presence", left_action, right_action
        if left_action.action != right_action.action:
            return 1, action, "action", left_action.action, right_action.action
        for field in ACTION_FIELDS:
            left_value = getattr(left_action, field)
            right_value = getattr(right_action, field)
            if not _values_equal(left_value, right_value, tolerance):
                return 1, action, field, left_value, right_value
    return None


def compare_results(
    left: ReplayResult,
    right: ReplayResult,
    *,
    axis_tolerance: float = 1e-9,
    raise_on_divergence: bool = False,
) -> DiffReport:
    mismatch_count = 0
    first: tuple[int, str | None, str | None, Any, Any] | None = None
    checked = min(len(left.frames), len(right.frames))
    for index in range(checked):
        difference = _frame_difference(
            left.frames[index],
            right.frames[index],
            axis_tolerance,
        )
        if difference is not None:
            mismatch_count += 1
            if first is None:
                first = difference
    if len(left.frames) != len(right.frames):
        mismatch_count += abs(len(left.frames) - len(right.frames))
        if first is None:
            index = checked
            left_frame = left.frames[index].to_dict() if index < len(left.frames) else None
            right_frame = right.frames[index].to_dict() if index < len(right.frames) else None
            first = (1, None, "frame_count", left_frame, right_frame)
    if first is None:
        report = DiffReport(
            equal=True,
            first_frame_index=None,
            event_seq=None,
            action=None,
            field=None,
            left_value=None,
            right_value=None,
            checked_frames=checked,
            mismatch_count=0,
            message="incremental and full replay are identical",
        )
        return report
    index = min(
        [
            position
            for position in range(checked)
            if _frame_difference(left.frames[position], right.frames[position], axis_tolerance)
            is not None
        ]
        or [checked]
    )
    event_seq = None
    if index < len(left.frames):
        event_seq = left.frames[index].source_event_seq
    elif left.frames:
        event_seq = left.frames[-1].source_event_seq
    _, action, field, left_value, right_value = first
    report = DiffReport(
        equal=False,
        first_frame_index=index,
        event_seq=event_seq,
        action=action,
        field=field,
        left_value=left_value,
        right_value=right_value,
        checked_frames=checked,
        mismatch_count=mismatch_count,
        message=f"first divergence at frame {index}: {field}",
    )
    if raise_on_divergence:
        raise DivergenceError(
            report.message,
            field=field or "output",
            event_seq=event_seq,
            context=report.to_dict(),
        )
    return report


def latest_state_matrix(
    result: ReplayResult,
    action_ids: list[str],
    *,
    bin_count: int = 24,
) -> tuple[np.ndarray, list[int]]:
    if not result.frames:
        return np.zeros((len(action_ids), bin_count), dtype=float), [0] * (bin_count + 1)
    start = result.frames[0].t_ms
    end = max(result.frames[-1].t_ms, start + 1)
    edges = [start + (end - start) * index / bin_count for index in range(bin_count + 1)]
    matrix = np.zeros((len(action_ids), bin_count), dtype=float)
    frame_index = 0
    for bucket in range(bin_count):
        midpoint = (edges[bucket] + edges[bucket + 1]) / 2.0
        while (
            frame_index + 1 < len(result.frames)
            and result.frames[frame_index + 1].t_ms <= midpoint
        ):
            frame_index += 1
        frame = result.frames[frame_index]
        for action_index, action_id in enumerate(action_ids):
            snapshot = frame.actions.get(action_id)
            if snapshot is None:
                continue
            matrix[action_index, bucket] = (
                snapshot.value
                + (1.0 if snapshot.pressed else 0.0)
                + 0.05 * snapshot.trigger_count
            )
    return matrix, [int(round(edge)) for edge in edges]


def comparison_heatmap(
    left: ReplayResult,
    right: ReplayResult,
    *,
    action_ids: list[str],
    bin_count: int = 24,
) -> tuple[np.ndarray, list[int]]:
    left_matrix, edges = latest_state_matrix(left, action_ids, bin_count=bin_count)
    right_matrix, _ = latest_state_matrix(right, action_ids, bin_count=bin_count)
    return np.abs(left_matrix - right_matrix), edges


def change_heatmap(
    previous: ReplayResult | None,
    current: ReplayResult,
    *,
    action_ids: list[str],
    bin_count: int = 24,
) -> tuple[np.ndarray, list[int]]:
    if previous is None:
        matrix, edges = latest_state_matrix(current, action_ids, bin_count=bin_count)
        return np.zeros_like(matrix), edges
    return comparison_heatmap(previous, current, action_ids=action_ids, bin_count=bin_count)


__all__ = [
    "ACTION_FIELDS",
    "DiffReport",
    "change_heatmap",
    "compare_results",
    "comparison_heatmap",
    "latest_state_matrix",
]
