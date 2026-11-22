"""Text-line interval state machine.

States: PENDING -> RECOGNIZING -> COMMITTED | FAILED, FAILED -> PENDING.
Any other transition raises E_STATE and leaves the state unchanged.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass
from typing import Dict, Optional, Tuple

from .errors import StateError


class IntervalState(enum.Enum):
    PENDING = "pending"
    RECOGNIZING = "recognizing"
    COMMITTED = "committed"
    FAILED = "failed"


_TRANSITIONS: Dict[IntervalState, frozenset] = {
    IntervalState.PENDING: frozenset({IntervalState.RECOGNIZING}),
    IntervalState.RECOGNIZING: frozenset(
        {IntervalState.COMMITTED, IntervalState.FAILED}
    ),
    IntervalState.FAILED: frozenset({IntervalState.PENDING}),
    IntervalState.COMMITTED: frozenset(),
}


@dataclass
class Interval:
    """One interval of consecutive text lines of a single image."""

    image_id: str
    interval_index: int
    line_start: int
    line_end: int  # exclusive
    state: IntervalState = IntervalState.PENDING
    frozen_params: Optional[Tuple[float, float]] = None
    frozen_source_version: Optional[str] = None

    def transition(self, target: IntervalState) -> None:
        if target not in _TRANSITIONS[self.state]:
            raise StateError(
                f"illegal transition {self.state.value} -> {target.value}",
                current=self.state.value,
                target=target.value,
            )
        self.state = target

    def commit(self, merge_threshold: float, conf_threshold: float,
               source_version: str) -> None:
        """Commit: freeze the parameter and source-version snapshot."""
        self.transition(IntervalState.COMMITTED)
        self.frozen_params = (merge_threshold, conf_threshold)
        self.frozen_source_version = source_version


def build_intervals(image_id: str, num_lines: int, lines_per_interval: int):
    intervals = []
    start = 0
    index = 0
    while start < num_lines:
        end = min(start + lines_per_interval, num_lines)
        intervals.append(Interval(image_id, index, start, end))
        start = end
        index += 1
    return intervals


def progress(intervals) -> Tuple[int, int]:
    committed = sum(1 for iv in intervals if iv.state is IntervalState.COMMITTED)
    return committed, len(intervals)
