"""Adaptive target-delay controller.

The controller is a pure, deterministic function of *observed* arrival
statistics (water level, late ratio, playout clock drift).  It never
looks at frames that have not arrived yet, which is what makes the
realtime, resume and offline paths produce identical decision
sequences for the same sample and parameters.
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, asdict, field

from .errors import ParamError


def _std(values):
    n = len(values)
    if n < 2:
        return 0.0
    mean = sum(values) / n
    return (sum((v - mean) ** 2 for v in values) / n) ** 0.5


@dataclass
class ControllerParams:
    # target delay bounds (ms)
    min_delay_ms: int = 40
    max_delay_ms: int = 400
    init_delay_ms: int = 120
    step_ms: int = 20
    # hysteresis: minimum ticks between two adjustments
    adjust_interval_ticks: int = 5
    # late-ratio thresholds (fraction of gap windows)
    high_late_ratio: float = 0.10
    low_late_ratio: float = 0.02
    # water level (frames buffered ahead) considered excessive
    high_water_frames: int = 12
    # jitter surge detection
    surge_window: int = 20          # arrivals in the sliding window
    surge_factor: float = 2.5       # delay std surge vs baseline
    # duplicate tolerance for the loader/engine
    dup_tolerance_ratio: float = 0.10
    dup_tolerance_abs: int = 20

    def validate(self) -> "ControllerParams":
        p = self
        errs = []
        if p.min_delay_ms < 0:
            errs.append("min_delay_ms")
        if p.max_delay_ms <= p.min_delay_ms:
            errs.append("max_delay_ms")
        if not (p.min_delay_ms <= p.init_delay_ms <= p.max_delay_ms):
            errs.append("init_delay_ms")
        if p.step_ms <= 0 or p.step_ms > p.max_delay_ms - p.min_delay_ms:
            errs.append("step_ms")
        if p.adjust_interval_ticks < 1:
            errs.append("adjust_interval_ticks")
        if not (0.0 <= p.low_late_ratio < p.high_late_ratio <= 1.0):
            errs.append("late_ratio")
        if p.high_water_frames < 1:
            errs.append("high_water_frames")
        if p.surge_window < 2:
            errs.append("surge_window")
        if p.surge_factor <= 1.0:
            errs.append("surge_factor")
        if p.dup_tolerance_ratio < 0 or p.dup_tolerance_abs < 0:
            errs.append("dup_tolerance")
        if errs:
            raise ParamError(
                "controller parameter(s) out of range: %s" % ",".join(errs),
                field=errs[0],
                detail={"fields": errs},
            )
        return self

    @property
    def version(self) -> str:
        """Stable policy-parameter version used by checkpoint gating."""
        blob = json.dumps(asdict(self), sort_keys=True).encode("utf-8")
        return hashlib.sha1(blob).hexdigest()[:16]


@dataclass
class Decision:
    tick: int
    reason: str
    old_delay_ms: int
    new_delay_ms: int
    water_frames: int
    late_ratio: float
    drift_ms: int
    rationale: str = ""
    gap_count: int = 0
    avg_queue_delay_ms: float = 0.0

    def key(self):
        """Fields that must match across the three paths."""
        return (self.tick, self.reason, self.old_delay_ms, self.new_delay_ms)


class AdaptiveController:
    """Water-level / clock-drift driven target-delay adaptation."""

    def __init__(self, params: ControllerParams):
        self.params = params.validate()
        self.target_delay_ms = params.init_delay_ms
        self._last_adjust_tick = -10 ** 9
        self._last_direction = 0          # +1 raised, -1 lowered
        self._last_surge_tick = -10 ** 9
        self.decisions: list[Decision] = []

    # ------------------------------------------------------------------
    def _clamp(self, value: int) -> int:
        p = self.params
        return max(p.min_delay_ms, min(p.max_delay_ms, value))

    def _record(self, tick, reason, old, new, water, late_ratio, drift,
                rationale="", gap_count=0, avg_queue_delay=0.0):
        self.decisions.append(Decision(
            tick=tick, reason=reason, old_delay_ms=old, new_delay_ms=new,
            water_frames=water, late_ratio=round(late_ratio, 6),
            drift_ms=drift, rationale=rationale, gap_count=gap_count,
            avg_queue_delay_ms=round(avg_queue_delay, 3),
        ))

    # ------------------------------------------------------------------
    def observe(self, tick_idx: int, t_ms: int, *, water_frames: int,
                water_trend: float, late_ratio: float, drift_ms: int,
                recent_delays: list,
                recent_gap_count: int,
                recent_queue_delays: list) -> int:
        """One control step; returns the (possibly new) target delay.

        ``tick_idx`` is a monotonic control-step counter (cooldowns are
        measured in ticks); ``t_ms`` is the virtual time in milliseconds
        recorded on decisions.  Only uses information observed so far.
        """
        p = self.params
        old = self.target_delay_ms
        tick = tick_idx

        # --- jitter surge trade-off (low latency vs continuity) --------
        # current-window delay std vs the std of everything observed
        # before the window: a jump means the jitter regime changed.
        if (len(recent_delays) >= 2 * p.surge_window
                and tick - self._last_surge_tick >= p.surge_window):
            win = recent_delays[-p.surge_window:]
            hist = recent_delays[:-p.surge_window]
            std = _std(win)
            baseline = max(_std(hist), 1.0)
            if std > p.surge_factor * baseline:
                self._last_surge_tick = tick
                avg_q = (sum(recent_queue_delays) / len(recent_queue_delays)
                         if recent_queue_delays else 0.0)
                if recent_gap_count > 0 and old < p.max_delay_ms:
                    new = self._clamp(old + p.step_ms)
                    self._record(
                        t_ms, "jitter_surge", old, new, water_frames,
                        late_ratio, drift_ms,
                        rationale=("delay std %.1fms > %.1fx baseline %.1fms; "
                                   "%d gaps in window -> prefer continuity"
                                   % (std, p.surge_factor, baseline,
                                      recent_gap_count)),
                        gap_count=recent_gap_count, avg_queue_delay=avg_q)
                    self.target_delay_ms = new
                    self._last_adjust_tick = tick
                    self._last_direction = +1
                    return new
                self._record(
                    t_ms, "jitter_surge", old, old, water_frames,
                    late_ratio, drift_ms,
                    rationale=("delay std %.1fms > %.1fx baseline %.1fms; "
                               "no gaps -> hold low latency"
                               % (std, p.surge_factor, baseline)),
                    gap_count=recent_gap_count, avg_queue_delay=avg_q)
                return old

        # --- hysteresis cooldown --------------------------------------
        if tick - self._last_adjust_tick < p.adjust_interval_ticks:
            return old

        # --- raise delay: too many late/gap windows --------------------
        if late_ratio > p.high_late_ratio and old < p.max_delay_ms:
            new = self._clamp(old + p.step_ms)
            if new != old:
                self._record(t_ms, "late_ratio_high", old, new, water_frames,
                             late_ratio, drift_ms,
                             rationale="late ratio %.3f > %.3f; water trend %.1f"
                                       % (late_ratio, p.high_late_ratio,
                                          water_trend))
                self.target_delay_ms = new
                self._last_adjust_tick = tick
                self._last_direction = +1
                return new

        # --- lower delay: water high and lateness rare -----------------
        # hysteresis: right after a raise we demand a much lower late
        # ratio before allowing a decrease, preventing oscillation.
        low_thresh = p.low_late_ratio
        if self._last_direction == +1:
            low_thresh *= 0.5
        if (late_ratio < low_thresh and water_frames > p.high_water_frames
                and old > p.min_delay_ms):
            new = self._clamp(old - p.step_ms)
            if new != old:
                self._record(t_ms, "water_high_low_latency", old, new,
                             water_frames, late_ratio, drift_ms,
                             rationale="water %d > %d and late ratio %.3f < %.3f"
                                       % (water_frames, p.high_water_frames,
                                          late_ratio, low_thresh))
                self.target_delay_ms = new
                self._last_adjust_tick = tick
                self._last_direction = -1
                return new

        return old
