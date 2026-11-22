"""Receive-side jitter buffer engine.

The engine is driven by a *virtual* clock in integer milliseconds.  At
each tick it
  1. admits arrived frames (sorted by media timestamp, deduplicated),
  2. lets the adaptive controller adjust the target delay using only
     information observed so far,
  3. commits every play window whose media time is due, overlap-adding
     frames into the continuous output stream (silence for gaps).

Because admission order is fully determined by (arrival_ts, seq, dup)
and the tick schedule is fixed, the realtime (threaded ingress),
checkpoint-resume and offline-replay paths produce bit-identical
output, decision sequences and energy statistics.
"""
from __future__ import annotations

from dataclasses import asdict, dataclass, field

import numpy as np

from .controller import AdaptiveController, ControllerParams, Decision
from .checkpoint import CheckpointManager
from .loader import Sample


@dataclass
class RunResult:
    output: np.ndarray                 # float64, duration_samples
    decisions: list                    # list[Decision]
    energy: np.ndarray                 # float64, num_frames
    gap_flags: list                    # bool per committed window
    dup_records: list
    drop_records: list
    failed_windows: list
    stats: dict


class WindowCommitError(Exception):
    """A single window failed; engine state is left untouched."""

    def __init__(self, seq, cause):
        super().__init__("window %d commit failed: %s" % (seq, cause))
        self.seq = seq
        self.cause = cause


class JitterEngine:
    def __init__(self, sample: Sample, params: ControllerParams,
                 checkpoint: CheckpointManager | None = None,
                 max_pending_frames: int = 64):
        self.sample = sample
        self.params = params.validate()
        self.controller = AdaptiveController(self.params)
        self.checkpoint = checkpoint
        self.max_pending_frames = max_pending_frames

        s = sample
        self.hop_ms = s.hop_ms
        self.num_frames = s.num_frames
        self.output = np.zeros(s.duration_samples, dtype=np.float64)
        self.energy = np.zeros(s.num_frames, dtype=np.float64)
        self.gap_flags: list[bool] = []
        self.dup_records: list[dict] = []
        self.drop_records: list[dict] = []
        self.failed_windows: list[int] = []

        self.tick = 0
        self.origin_ms: int | None = None
        self.next_seq = 0
        self.buffer: dict[int, object] = {}     # seq -> Arrival
        self.admitted: set[int] = set()
        self.ingress_pos = 0                    # arrival records consumed
        self._arrival_delays: list[float] = []
        self._queue_delays: list[float] = []
        self._last_water = 0
        self._tick_idx = 0
        # tick for which the control step already ran (set by restore)
        self._control_done_tick: int | None = None

    # ------------------------------------------------------------------
    # admission
    # ------------------------------------------------------------------
    def _admit(self, arrival) -> None:
        seq = arrival.seq
        if seq in self.admitted:
            self.dup_records.append({
                "seq": seq, "arrival_ts": arrival.arrival_ts,
                "reason": "duplicate",
            })
            return
        self.admitted.add(seq)
        if seq < self.next_seq:
            self.drop_records.append({
                "seq": seq, "arrival_ts": arrival.arrival_ts,
                "reason": "late_after_commit",
            })
            return
        self.buffer[seq] = arrival
        self._arrival_delays.append(float(arrival.arrival_ts
                                          - arrival.media_ts))

    # ------------------------------------------------------------------
    # controller step
    # ------------------------------------------------------------------
    def _control_step(self) -> None:
        committed = self.next_seq
        w = self.params.surge_window
        window_flags = self.gap_flags[-w:]
        late_ratio = (sum(window_flags) / len(window_flags)
                      if window_flags else 0.0)
        water = sum(1 for q in self.buffer if q >= self.next_seq)
        water_trend = float(water - self._last_water)
        self._last_water = water
        drift = (self.tick - self.origin_ms) - self.next_seq * self.hop_ms
        recent_gaps = sum(self.gap_flags[-w:]) if self.gap_flags else 0
        old = self.controller.target_delay_ms
        new = self.controller.observe(
            self._tick_idx, self.tick,
            water_frames=water,
            water_trend=water_trend,
            late_ratio=late_ratio,
            drift_ms=drift,
            recent_delays=self._arrival_delays,
            recent_gap_count=recent_gaps,
            recent_queue_delays=self._queue_delays[-w:],
        )
        if new != old:
            # shift playout origin; committed windows are never touched
            self.origin_ms += (new - old)
        self._tick_idx += 1

    # ------------------------------------------------------------------
    # window commit
    # ------------------------------------------------------------------
    def _commit_window(self, seq: int) -> None:
        s = self.sample
        arrival = self.buffer.pop(seq, None)
        try:
            if arrival is not None:
                frame = np.asarray(s.frame_data(seq), dtype=np.float64)
                gap = False
                queue_delay = float(self.tick - arrival.arrival_ts)
            else:
                frame = np.zeros(s.frame_len, dtype=np.float64)
                gap = True
                queue_delay = None
            energy = float(np.sum(frame * frame))
        except Exception as exc:  # window failed; state untouched
            self.failed_windows.append(seq)
            raise WindowCommitError(seq, exc)

        start = seq * s.hop
        self.output[start:start + s.frame_len] += frame
        self.energy[seq] = energy
        self.gap_flags.append(gap)
        if queue_delay is not None:
            self._queue_delays.append(queue_delay)
        self.next_seq = seq + 1
        # purge frames that can never be played any more (recorded)
        stale = [q for q in self.buffer if q < self.next_seq]
        for q in stale:
            a = self.buffer.pop(q)
            self.drop_records.append({
                "seq": q, "arrival_ts": a.arrival_ts,
                "reason": "late_after_commit",
            })
        if self.checkpoint is not None:
            self.checkpoint.write(self.snapshot())

    # ------------------------------------------------------------------
    # main loop
    # ------------------------------------------------------------------
    def run(self, ingress, *, stop_after_windows: int | None = None,
            max_ticks: int = 2_000_000) -> RunResult:
        """Drive the virtual clock until all windows are committed.

        ``ingress`` must provide ``poll(t_ms) -> list[Arrival]`` and an
        ``exhausted`` flag.  ``stop_after_windows`` halts the run after
        exactly that many committed windows (used by resume tests).
        """
        s = self.sample
        ticks = 0
        while self.next_seq < self.num_frames:
            ticks += 1
            if ticks > max_ticks:
                raise RuntimeError("engine tick budget exceeded")
            for arrival in ingress.poll(self.tick):
                self._admit(arrival)
                self.ingress_pos += 1
            if self.origin_ms is None and (self.buffer or ingress.exhausted):
                base = min((a.arrival_ts for a in self.buffer.values()),
                           default=0)
                self.origin_ms = base + self.controller.target_delay_ms
            if self.origin_ms is not None:
                if self._control_done_tick != self.tick:
                    self._control_step()
                    self._control_done_tick = self.tick
                while (self.next_seq < self.num_frames
                       and self.next_seq * self.hop_ms
                       <= self.tick - self.origin_ms):
                    self._commit_window(self.next_seq)
                    if (stop_after_windows is not None
                            and self.next_seq >= stop_after_windows):
                        return self._result()
            self.tick += self.hop_ms
        return self._result()

    # ------------------------------------------------------------------
    # checkpoint state
    # ------------------------------------------------------------------
    def snapshot(self) -> dict:
        c = self.controller
        return {
            "content_hash": self.sample.content_hash,
            "policy_version": self.params.version,
            "tick": self.tick,
            "origin_ms": self.origin_ms,
            "target_delay_ms": c.target_delay_ms,
            "next_seq": self.next_seq,
            "ingress_pos": self.ingress_pos,
            "buffer": [{"seq": a.seq, "media_ts": a.media_ts,
                        "arrival_ts": a.arrival_ts}
                       for a in sorted(self.buffer.values(),
                                       key=lambda x: x.seq)],
            "admitted": sorted(self.admitted),
            "dup_records": list(self.dup_records),
            "drop_records": list(self.drop_records),
            "gap_flags": list(self.gap_flags),
            "arrival_delays": list(self._arrival_delays),
            "queue_delays": list(self._queue_delays),
            "energy": self.energy.tolist(),
            "output_committed": self.next_seq,
            "decisions": [asdict(d) for d in c.decisions],
            "controller": {
                "last_adjust_tick": c._last_adjust_tick,
                "last_direction": c._last_direction,
                "last_surge_tick": c._last_surge_tick,
            },
            "last_water": self._last_water,
            "failed_windows": list(self.failed_windows),
            "tick_idx": self._tick_idx,
        }

    def restore(self, state: dict) -> None:
        """Restore from a validated checkpoint (hash/version checked by
        the CheckpointManager before this is called)."""
        from .loader import Arrival
        c = self.controller
        self.tick = state["tick"]
        self.origin_ms = state["origin_ms"]
        c.target_delay_ms = state["target_delay_ms"]
        self.next_seq = state["next_seq"]
        self.ingress_pos = state["ingress_pos"]
        self.buffer = {r["seq"]: Arrival(seq=r["seq"],
                                         media_ts=r["media_ts"],
                                         arrival_ts=r["arrival_ts"],
                                         dup=False)
                       for r in state["buffer"]}
        self.admitted = set(state["admitted"])
        self.dup_records = list(state["dup_records"])
        self.drop_records = list(state["drop_records"])
        self.gap_flags = list(state["gap_flags"])
        self._arrival_delays = list(state["arrival_delays"])
        self._queue_delays = list(state["queue_delays"])
        self.energy = np.array(state["energy"], dtype=np.float64)
        c.decisions = [Decision(**d) for d in state["decisions"]]
        c._last_adjust_tick = state["controller"]["last_adjust_tick"]
        c._last_direction = state["controller"]["last_direction"]
        c._last_surge_tick = state["controller"]["last_surge_tick"]
        self._last_water = state["last_water"]
        self.failed_windows = list(state.get("failed_windows", []))
        self._tick_idx = state.get("tick_idx", 0)
        # the control step for the restored tick already ran before the
        # checkpoint was written; skip it to stay aligned with the
        # uninterrupted run
        self._control_done_tick = self.tick
        # rebuild the committed portion of the output stream
        s = self.sample
        self.output = np.zeros(s.duration_samples, dtype=np.float64)
        for seq in range(self.next_seq):
            if not self.gap_flags[seq]:
                start = seq * s.hop
                self.output[start:start + s.frame_len] += \
                    np.asarray(s.frame_data(seq), dtype=np.float64)

    # ------------------------------------------------------------------
    def _result(self) -> RunResult:
        gaps = int(sum(self.gap_flags))
        avg_q = (sum(self._queue_delays) / len(self._queue_delays)
                 if self._queue_delays else 0.0)
        return RunResult(
            output=self.output,
            decisions=list(self.controller.decisions),
            energy=self.energy.copy(),
            gap_flags=list(self.gap_flags),
            dup_records=list(self.dup_records),
            drop_records=list(self.drop_records),
            failed_windows=list(self.failed_windows),
            stats={
                "committed_windows": self.next_seq,
                "gap_count": gaps,
                "avg_queue_delay_ms": round(avg_q, 3),
                "final_target_delay_ms": self.controller.target_delay_ms,
                "dup_dropped": len(self.dup_records),
                "late_dropped": len(self.drop_records),
                "output_samples": int(self.output.shape[0]),
                "declared_samples": self.sample.duration_samples,
            },
        )
