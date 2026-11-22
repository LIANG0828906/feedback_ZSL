"""Render-frame scheduling: predicted vs authoritative frame arbitration.

The renderer runs at a finite frame rate.  Each frame it must decide
whether to show the *predicted* state (for input responsiveness) or the
*authoritative* state (for consistency).  The policy bounds:

* input response latency to ``max_input_delay_frames``
* authoritative correction display to ``correction_window_frames``

When both demands conflict the correction wins (consistency over
responsiveness) and the trade-off reason is recorded on the frame log.
Out-of-range parameters raise ParamError (E_PARA).
"""

from __future__ import annotations

from dataclasses import dataclass

from .errors import ParamError

MODE_PREDICTED = "predicted"
MODE_AUTHORITATIVE = "authoritative"


@dataclass(frozen=True)
class FramePolicy:
    render_fps: int = 60
    tick_rate: int = 20
    max_input_delay_frames: int = 3
    correction_window_frames: int = 6

    def __post_init__(self):
        if not 1 <= self.render_fps <= 240:
            raise ParamError("render_fps out of range [1, 240]", field="render_fps")
        if not 1 <= self.tick_rate <= 120:
            raise ParamError("tick_rate out of range [1, 120]", field="tick_rate")
        if self.render_fps % self.tick_rate != 0:
            raise ParamError(
                "render_fps must be a multiple of tick_rate", field="render_fps"
            )
        if not 1 <= self.max_input_delay_frames:
            raise ParamError(
                "max_input_delay_frames must be >= 1",
                field="max_input_delay_frames",
            )
        if not 1 <= self.correction_window_frames:
            raise ParamError(
                "correction_window_frames must be >= 1",
                field="correction_window_frames",
            )

    @property
    def frames_per_tick(self):
        return self.render_fps // self.tick_rate


@dataclass
class FrameRecord:
    frame: int
    tick: int
    mode: str
    reason: str


class FrameScheduler:
    """Decides per render frame which state the client displays."""

    def __init__(self, policy: FramePolicy):
        self.policy = policy
        self.log: list[FrameRecord] = []
        self._last_input_frame = None
        self._correction_frame = None

    def note_input(self, frame):
        self._last_input_frame = frame

    def note_correction(self, frame):
        """An authoritative frame arrived and must be displayed soon."""
        self._correction_frame = frame

    def decide(self, frame, tick, has_unacked_inputs):
        p = self.policy
        correction_pending = self._correction_frame is not None
        recent_input = (
            self._last_input_frame is not None
            and frame - self._last_input_frame <= p.max_input_delay_frames
        )
        if correction_pending and recent_input:
            mode = MODE_AUTHORITATIVE
            reason = "conflict:correction_priority(consistency>responsiveness)"
        elif correction_pending:
            mode = MODE_AUTHORITATIVE
            reason = "correction_applied_within_window"
        elif has_unacked_inputs or recent_input:
            mode = MODE_PREDICTED
            reason = "input_responsiveness"
        else:
            mode = MODE_AUTHORITATIVE
            reason = "steady_state"
        if correction_pending:
            self._correction_frame = None
        record = FrameRecord(frame=frame, tick=tick, mode=mode, reason=reason)
        self.log.append(record)
        return record

    def correction_frames(self):
        """Frames on which an authoritative correction was displayed."""
        return [
            r.frame
            for r in self.log
            if r.reason.startswith("conflict:")
            or r.reason.startswith("correction_applied")
        ]

    def input_delay_violations(self, input_frames):
        """Inputs not reflected by a predicted frame within the bound."""
        violations = []
        for f_in in input_frames:
            window = [
                r
                for r in self.log
                if f_in <= r.frame <= f_in + self.policy.max_input_delay_frames
            ]
            if window and all(
                r.mode != MODE_PREDICTED and not r.reason.startswith("conflict:")
                for r in window
            ):
                violations.append(f_in)
        return violations
