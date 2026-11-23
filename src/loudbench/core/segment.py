"""节目段数据模型。"""
from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .states import SegmentState, SegmentStateMachine


@dataclass
class ProgramSegment:
    """一段节目素材及其运行期状态。"""

    index: int
    name: str
    sample_rate: int
    samples: np.ndarray | None = None
    source_path: str | None = None
    channels: int = 2
    truth: dict = field(default_factory=dict)
    state_machine: SegmentStateMachine = field(
        default_factory=lambda: SegmentStateMachine(0))
    error: str | None = None

    def __post_init__(self) -> None:
        self.state_machine = SegmentStateMachine(self.index)

    @property
    def state(self) -> SegmentState:
        return self.state_machine.state

    @property
    def duration_seconds(self) -> float:
        if self.samples is None:
            return 0.0
        return self.samples.shape[0] / self.sample_rate
