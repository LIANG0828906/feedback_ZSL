"""Deterministic performance-expression playback engine.

Time is always tracked in canonical beats; seconds are derived with the score
tempo.  A pluggable :class:`Clock` keeps the engine fully testable without a
running audio device.  NumPy powers the velocity-curve resampling used by the
timeline view.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

import numpy as np

from .errors import ParameterError
from .model import ExpressionEvent, Score


class Clock:
    def now(self) -> float:
        return time.monotonic()


@dataclass
class Frame:
    elapsed_seconds: float
    current_beat: float
    velocity: float
    legato: float
    pedal: float
    active_event_ids: list[str]


class PlaybackEngine:
    def __init__(self, score: Score, clock: Clock | None = None):
        self.score = score
        self.clock = clock or Clock()
        self._playing = False
        self._start_clock = 0.0
        self._start_beat = 0.0
        self._cursor_beat = 0.0
        self._sorted_events = sorted(
            score.events, key=score.event_absolute_beat
        )
        self.total_beats = score.total_beats()

    # ---- controls -----------------------------------------------------
    def play(self, start_beat: float | None = None) -> float:
        if start_beat is not None:
            self._check_beat(start_beat, event_id=None)
            self._cursor_beat = float(start_beat)
        self._start_beat = self._cursor_beat
        self._start_clock = self.clock.now()
        self._playing = True
        return self._start_beat

    def pause(self) -> float:
        if self._playing:
            self._cursor_beat = self.current_beat()
        self._playing = False
        return self._cursor_beat

    def stop(self) -> None:
        self._playing = False
        self._cursor_beat = 0.0

    def seek(self, beat: float) -> None:
        self._check_beat(beat, event_id=None)
        self._cursor_beat = float(beat)
        if self._playing:
            self._start_beat = self._cursor_beat
            self._start_clock = self.clock.now()

    @property
    def is_playing(self) -> bool:
        return self._playing

    # ---- state --------------------------------------------------------
    def current_beat(self) -> float:
        if not self._playing:
            return self._cursor_beat
        elapsed = self.clock.now() - self._start_clock
        beat = self._start_beat + self.score.seconds_to_beat(elapsed)
        return min(beat, self.total_beats)

    def current_seconds(self) -> float:
        return self.score.beat_to_seconds(self.current_beat())

    def frame(self) -> Frame:
        beat = self.current_beat()
        active = self._active_events(beat)
        velocity = self._latest_value(active, "velocity", default=0.75)
        legato = self._latest_value(active, "legato", default=0.0)
        pedal = self._latest_value(active, "pedal", default=0.0)
        return Frame(
            elapsed_seconds=self.current_seconds(),
            current_beat=beat,
            velocity=velocity,
            legato=legato,
            pedal=pedal,
            active_event_ids=[event.event_id for event in active],
        )

    def _active_events(self, beat: float) -> list[ExpressionEvent]:
        active = []
        for event in self._sorted_events:
            event_beat = self.score.event_absolute_beat(event)
            if event_beat > beat:
                break
            end_beat = event_beat
            if event.kind == "legato":
                end_beat = event_beat + event.duration_beats
            if event_beat <= beat <= end_beat or event_beat == beat:
                active.append(event)
        return active

    @staticmethod
    def _latest_value(events, kind: str, default: float) -> float:
        values = [event for event in events if event.kind == kind]
        if not values:
            return default
        return values[-1].value

    # ---- curves / validation -----------------------------------------
    def velocity_curve(self, sample_count: int = 256) -> np.ndarray:
        if sample_count <= 0:
            raise ParameterError(
                "sample_count must be positive",
                {"sample_count": sample_count},
            )
        samples = np.linspace(0.0, self.total_beats, sample_count)
        curve = np.full(sample_count, 0.75, dtype=float)
        velocity_events = [
            event for event in self._sorted_events if event.kind == "velocity"
        ]
        for event in velocity_events:
            event_beat = self.score.event_absolute_beat(event)
            curve[samples >= event_beat] = event.value
        return curve

    def pedal_curve(self, sample_count: int = 256) -> np.ndarray:
        samples = np.linspace(0.0, self.total_beats, sample_count)
        curve = np.zeros(sample_count, dtype=float)
        for event in self._sorted_events:
            if event.kind != "pedal":
                continue
            event_beat = self.score.event_absolute_beat(event)
            curve[samples >= event_beat] = (
                event.depth if event.depth is not None else event.value
            )
        return curve

    def legato_spans(self) -> list[tuple[float, float, str]]:
        spans = []
        for event in self._sorted_events:
            if event.kind != "legato":
                continue
            start = self.score.event_absolute_beat(event)
            spans.append(
                (start, start + event.duration_beats, event.event_id)
            )
        return spans

    def event_absolute_seconds(self, event: ExpressionEvent) -> float:
        beat = self.score.event_absolute_beat(event)
        return self.score.beat_to_seconds(beat)

    def _check_beat(self, beat: float, event_id: str | None) -> None:
        if not isinstance(beat, (int, float)) or beat < 0 or beat > self.total_beats:
            raise ParameterError(
                "Playback beat is outside the score range",
                {
                    "source_event_id": event_id,
                    "beat": beat,
                    "valid_range": [0.0, self.total_beats],
                },
            )

    def validate_event_ranges(self) -> None:
        """E_PARA guard for out-of-range velocity / beat on every event."""
        for event in self.score.events:
            beat = self.score.event_absolute_beat(event)
            self._check_beat(beat, event.event_id)
            if event.kind == "velocity" and not 0.0 <= event.value <= 1.0:
                raise ParameterError(
                    "Velocity event value outside [0,1]",
                    {"source_event_id": event.event_id, "value": event.value},
                )


class FakeClock(Clock):
    """Deterministic clock driven manually in tests."""

    def __init__(self):
        self._now = 0.0

    def now(self) -> float:
        return self._now

    def advance(self, seconds: float) -> None:
        self._now += seconds
