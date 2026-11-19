"""Deterministic WAN-link emulation used by the egress TCP proxies.

The emulation operates on fixed-size segments inside each proxied connection:

* bandwidth is enforced with an egress-shared token bucket;
* RTT drives a smoothed RTT/RTO model (RFC 6298 style);
* loss decisions are a pure seeded function of the egress segment number, so
  re-running the same configuration replays the exact same loss sequence;
* a Reno-style state machine approximates the congestion window, including
  slow start, congestion avoidance, fast retransmit and RTO recovery;
* milestone-based fault windows (delivered MB) inject transient spikes and
  sustained degradation without depending on wall-clock scheduling.

Real TCP preserves byte order, so a "lost" segment withholds that segment and
the following queued bytes until the emulated recovery completes, then
delivers everything in order.
"""

from __future__ import annotations

import json
import threading
import time
from dataclasses import dataclass, field

from .rng import uniform


def median(values: list[float]) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    middle = len(ordered) // 2
    if len(ordered) % 2:
        return round(float(ordered[middle]), 4)
    return round((ordered[middle - 1] + ordered[middle]) / 2.0, 4)


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round((pct / 100.0) * (len(ordered) - 1))))
    return round(float(ordered[index]), 4)


@dataclass
class EgressLinkState:
    """Shared, thread-safe state for all connections of one egress."""

    egress_id: str
    egress_index: int
    seed: int
    base_rtt_ms: float
    base_loss_pct: float
    bandwidth_mbps: float
    injections: tuple
    lock: threading.Lock = field(default_factory=threading.Lock)
    delivered_bytes: int = 0
    tokens: float = 0.0
    last_refill: float = 0.0
    active_connections: int = 0
    total_segments: int = 0
    lost_segments: int = 0
    emitted_bytes: int = 0

    @property
    def rate_bytes_per_s(self) -> float:
        return self.bandwidth_mbps * 1_000_000.0 / 8.0

    def delivered_mb(self) -> float:
        return self.delivered_bytes / 1_000_000.0

    def current_profile(self) -> tuple[float, float]:
        """Return the active (rtt_ms, loss_pct) after applying injections."""
        rtt_ms, loss_pct = self.base_rtt_ms, self.base_loss_pct
        current_mb = self.delivered_mb()
        for injection in self.injections:
            if injection.egress_id != self.egress_id:
                continue
            if injection.start_mb <= current_mb < injection.start_mb + injection.duration_mb:
                if injection.rtt_ms is not None:
                    rtt_ms = float(injection.rtt_ms)
                if injection.loss_pct is not None:
                    loss_pct = float(injection.loss_pct)
        return rtt_ms, loss_pct

    def take_bandwidth(self, size: int) -> float:
        """Block until ``size`` bytes are available; return waited seconds."""
        rate = self.rate_bytes_per_s
        with self.lock:
            now = time.monotonic()
            if self.last_refill == 0.0:
                self.last_refill = now
                self.tokens = rate
            waited = 0.0
            while True:
                self.tokens = min(rate, self.tokens + (now - self.last_refill) * rate)
                self.last_refill = now
                if self.tokens >= size:
                    self.tokens -= size
                    return waited
                deficit = size - self.tokens
                wait_s = deficit / rate
                time.sleep(wait_s)
                waited += wait_s
                now = time.monotonic()


class LinkEmulator:
    """Per-connection upstream (client -> target) link model."""

    MSS = 1460

    def __init__(self, state: EgressLinkState):
        self.state = state
        # Per-connection sequence: loss/jitter decisions must be a pure
        # function of the bytes seen by this connection, otherwise thread
        # interleaving across connections would make runs non-reproducible.
        self._segment_seq = 0
        self.cwnd = 10.0
        self.ssthresh = 1000.0
        self.srtt_ms = state.base_rtt_ms
        self.rttvar_ms = max(1.0, state.base_rtt_ms / 2.0)
        self.rto_ms = max(200.0, self.srtt_ms + 4.0 * self.rttvar_ms)
        # Inbound application bytes not yet segmented, plus the head segment
        # currently withheld for an emulated (in-order) retransmission.
        self.pending = bytearray()
        self.held_segment: bytes | None = None
        self.rtt_samples: list[float] = []
        self.retrans_segments = 0
        self.rto_count = 0
        self.total_segments = 0
        self.processed_bytes = 0
        self.emulated_ms = 0.0
        self.pacing_ms = 0.0
        self.recovery_ms = 0.0

    def _effective_rtt(self, seq: int) -> float:
        rtt_ms, _ = self._current_profile()
        jitter = uniform(self.state.seed, 917, self.state.egress_index, seq)
        return round(rtt_ms * (0.92 + 0.16 * jitter), 4)

    def _is_lost(self, seq: int) -> bool:
        _, loss_pct = self._current_profile()
        if loss_pct <= 0:
            return False
        return uniform(self.state.seed, 41, self.state.egress_index, seq) < loss_pct / 100.0

    def _current_profile(self) -> tuple[float, float]:
        """Active profile based on egress cumulative delivered bytes.

        Injection windows are expressed as delivered-MB milestones of the
        whole egress, so a sustained fault survives across the many short
        per-shard connections.  RTT jitter and loss *decisions* remain pure
        per-connection functions of the local segment sequence, preserving
        reproducibility of each connection's signal record.
        """
        rtt_ms, loss_pct = self.state.base_rtt_ms, self.state.base_loss_pct
        with self.state.lock:
            position_mb = self.state.delivered_bytes / 1_000_000.0
        for injection in self.state.injections:
            if injection.egress_id != self.state.egress_id:
                continue
            if injection.start_mb <= position_mb < injection.start_mb + injection.duration_mb:
                if injection.rtt_ms is not None:
                    rtt_ms = float(injection.rtt_ms)
                if injection.loss_pct is not None:
                    loss_pct = float(injection.loss_pct)
        return rtt_ms, loss_pct

    def _update_rtt(self, sample_ms: float) -> None:
        delta = abs(self.srtt_ms - sample_ms)
        self.rttvar_ms = 0.75 * self.rttvar_ms + 0.25 * delta
        self.srtt_ms = 0.875 * self.srtt_ms + 0.125 * sample_ms
        self.rto_ms = max(200.0, self.srtt_ms + 4.0 * self.rttvar_ms)

    def _on_ack(self) -> None:
        if self.cwnd < self.ssthresh:
            self.cwnd += 1.0
        else:
            self.cwnd += 1.0 / max(1.0, self.cwnd)

    def _recover(self, fast: bool) -> None:
        self.ssthresh = max(2.0, self.cwnd / 2.0)
        if fast:
            wait_ms = self.srtt_ms
            self.cwnd = max(2.0, self.ssthresh)
        else:
            wait_ms = self.rto_ms
            self.rto_count += 1
            self.cwnd = 1.0
        self.retrans_segments += 1
        self.recovery_ms += wait_ms
        self.emulated_ms += wait_ms
        time.sleep(wait_ms / 1000.0)

    def _segment_once(self) -> tuple[bytes | None, bool]:
        """Segment one MSS from pending; returns (segment, is_final_byte)."""
        if not self.pending:
            return None, False
        size = min(self.MSS, len(self.pending))
        segment = bytes(self.pending[:size])
        del self.pending[:size]
        return segment, not self.pending

    def process(self, data: bytes) -> list[bytes]:
        """Accept inbound bytes and return the chunks that may be emitted.

        A loss withholds the segment until the emulated fast retransmit or RTO
        completes; subsequent bytes stay queued behind it, mirroring TCP's
        ordered delivery, so no application byte is ever lost.
        """
        self.pending.extend(data)
        emitted: list[bytes] = []
        while True:
            if self.held_segment is not None:
                following_segments = len(self.pending) // self.MSS
                self._recover(fast=following_segments >= 3)
                segment = self.held_segment
                self.held_segment = None
                emitted.append(segment)
                with self.state.lock:
                    self.state.delivered_bytes += len(segment)
                    self.state.emitted_bytes += len(segment)
                continue
            segment, _final = self._segment_once()
            if segment is None:
                break
            seq = self._segment_seq
            self._segment_seq += 1
            self.total_segments += 1
            self.processed_bytes += len(segment)
            with self.state.lock:
                self.state.total_segments += 1
            waited_s = self.state.take_bandwidth(len(segment))
            self.pacing_ms += waited_s * 1000.0
            self.emulated_ms += waited_s * 1000.0
            sample_rtt = self._effective_rtt(seq)
            self.rtt_samples.append(sample_rtt)
            self._update_rtt(sample_rtt)
            self.emulated_ms += sample_rtt
            self._on_ack()
            if self._is_lost(seq):
                with self.state.lock:
                    self.state.lost_segments += 1
                # Withhold in order; recovery decision happens at the head.
                self.held_segment = segment
                continue
            emitted.append(segment)
            with self.state.lock:
                self.state.delivered_bytes += len(segment)
                self.state.emitted_bytes += len(segment)
        return emitted

    def stats_header(self) -> dict[str, str]:
        payload = {
            "egress": self.state.egress_id,
            "rtt_median_ms": median(self.rtt_samples),
            "rtt_samples_ms": self.rtt_samples[-32:],
            "rto_ms": round(self.rto_ms, 4),
            "rto_count": self.rto_count,
            "retrans_segments": self.retrans_segments,
            "total_segments": self.total_segments,
            "cwnd_segments_approx": round(self.cwnd, 3),
            "mss": self.MSS,
            "emulated_ms": round(self.emulated_ms, 4),
            "pacing_ms": round(self.pacing_ms, 4),
            "recovery_ms": round(self.recovery_ms, 4),
            "source": "link_emulation",
        }
        return {"X-Link-Stats": json.dumps(payload, separators=(",", ":"), sort_keys=True)}


def downstream_delay_ms(state: EgressLinkState) -> float:
    rtt_ms, _ = state.current_profile()
    return rtt_ms / 2.0
