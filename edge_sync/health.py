"""Egress health windows and conservative switching decisions.

A switch needs *both* a configurable number of consecutive bad samples and an
accumulated bad exposure longer than the jitter-tolerance duration.  Short
spikes therefore never move traffic; sustained degradation does.  The bad
exposure uses the deterministic link-emulation clock rather than wall time so
identical configurations and injection sequences produce identical switches.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
from threading import RLock
import time

from .config import Config, HealthConfig
from .link import median


@dataclass
class Observation:
    rtt_median_ms: float | None
    retrans_rate: float
    emulated_ms: float
    wall_ms: int


class EgressHealth:
    def __init__(self, egress_id: str, health: HealthConfig):
        self.egress_id = egress_id
        self.health = health
        self.window: deque[Observation] = deque(maxlen=health.window)
        self.lock = RLock()
        self.healthy = True
        self.bad_streak = 0
        self.bad_exposure_ms = 0.0
        self.cooldown_until_ms = 0
        self.last_trigger: dict | None = None
        self.good_streak = 0

    def _violation(self, observation: Observation) -> tuple[bool, str, float, float]:
        if (
            observation.rtt_median_ms is not None
            and observation.rtt_median_ms > self.health.rtt_median_ms
        ):
            return True, "rtt_median_ms", observation.rtt_median_ms, self.health.rtt_median_ms
        if observation.retrans_rate > self.health.retrans_rate:
            return True, "retrans_rate", observation.retrans_rate, self.health.retrans_rate
        return False, "", 0.0, 0.0

    def observe(self, observation: Observation) -> dict | None:
        """Add one shard observation; return a switch trigger when warranted."""
        with self.lock:
            self.window.append(observation)
            bad, metric, value, threshold = self._violation(observation)
            now_ms = observation.wall_ms
            if not self.healthy:
                return self._maybe_recover(now_ms)
            if not bad:
                self.bad_streak = 0
                self.bad_exposure_ms = 0.0
                return None
            self.bad_streak += 1
            self.bad_exposure_ms += observation.emulated_ms
            if (
                self.bad_streak >= self.health.min_bad_samples
                and self.bad_exposure_ms >= self.health.jitter_tolerance_ms
            ):
                trigger = {
                    "egress": self.egress_id,
                    "reason_metric": metric,
                    "observed_value": round(value, 4),
                    "threshold_value": threshold,
                    "bad_samples": self.bad_streak,
                    "emulated_bad_ms": round(self.bad_exposure_ms, 4),
                    "wall_ms": now_ms,
                }
                self.healthy = False
                self.cooldown_until_ms = now_ms + self.health.cooldown_ms
                self.last_trigger = trigger
                self.bad_streak = 0
                self.bad_exposure_ms = 0.0
                self.good_streak = 0
                return trigger
            return None

    def _maybe_recover(self, now_ms: int) -> dict | None:
        if now_ms < self.cooldown_until_ms:
            return None
        good = not self._violation(self.window[-1])[0] if self.window else False
        self.good_streak = self.good_streak + 1 if good else 0
        if self.good_streak >= self.health.min_bad_samples:
            self.healthy = True
            self.good_streak = 0
            return {
                "egress": self.egress_id,
                "reason_metric": "recovered",
                "observed_value": 0.0,
                "threshold_value": 0.0,
                "bad_samples": 0,
                "emulated_bad_ms": 0.0,
                "wall_ms": now_ms,
            }
        return None

    def snapshot(self) -> dict:
        self._refresh_recovery()
        with self.lock:
            samples = list(self.window)
            rtt_values = [item.rtt_median_ms for item in samples if item.rtt_median_ms is not None]
            rates = [item.retrans_rate for item in samples]
            return {
                "egress": self.egress_id,
                "healthy": self.healthy,
                "window_size": len(samples),
                "rtt_median_ms": median(rtt_values),
                "retrans_rate_median": median(rates) if rates else 0.0,
                "bad_streak": self.bad_streak,
                "bad_exposure_ms": round(self.bad_exposure_ms, 4),
                "cooldown_until_ms": self.cooldown_until_ms,
                "last_trigger": self.last_trigger,
            }

    def _refresh_recovery(self) -> None:
        """Re-evaluate recovery once the cooldown has elapsed."""
        with self.lock:
            if self.healthy:
                return
            now_ms = int(time.time() * 1000)
            if now_ms < self.cooldown_until_ms or not self.window:
                return
            recent = list(self.window)[-self.health.min_bad_samples:]
            if len(recent) >= self.health.min_bad_samples and all(
                not self._violation(item)[0] for item in recent
            ):
                self.healthy = True
                self.good_streak = 0


class HealthMonitor:
    def __init__(self, config: Config):
        self.config = config
        self.monitors = {
            egress.egress_id: EgressHealth(egress.egress_id, config.health) for egress in config.egresses
        }

    def observe(self, egress_id: str, observation: Observation) -> dict | None:
        return self.monitors[egress_id].observe(observation)

    def healthy_egresses(self) -> list[str]:
        return [egress_id for egress_id, monitor in self.monitors.items() if monitor.healthy]

    def is_healthy(self, egress_id: str) -> bool:
        return self.monitors[egress_id].healthy

    def choose_target(self, excluded: str) -> str:
        """Pick the healthiest alternative egress deterministically."""
        candidates = [
            egress for egress in self.config.egresses
            if egress.egress_id != excluded and self.is_healthy(egress.egress_id)
        ]
        if not candidates:
            candidates = [
                egress for egress in self.config.egresses if egress.egress_id != excluded
            ]
        ranked = sorted(
            candidates,
            key=lambda egress: (
                len(self.monitors[egress.egress_id].window),
                -egress.bandwidth_mbps,
                egress.egress_id,
            ),
        )
        return ranked[0].egress_id

    def snapshot(self) -> dict:
        return {egress_id: monitor.snapshot() for egress_id, monitor in self.monitors.items()}
