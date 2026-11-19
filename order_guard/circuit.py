from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import asdict, dataclass
from enum import Enum

from .config import CircuitConfig


class CircuitState(str, Enum):
    CLOSED = "closed"
    OPEN = "open"
    HALF_OPEN = "half_open"


@dataclass(frozen=True)
class CircuitTransition:
    from_state: str
    to_state: str
    reason: str
    observed_at: float
    snapshot: dict[str, object]


@dataclass(frozen=True)
class CircuitSnapshot:
    state: str
    consecutive_failures: int
    samples: int
    timeout_ratio: float
    opened_for_seconds: float | None
    half_open_inflight: int


class CircuitBreaker:
    def __init__(self, config: CircuitConfig, *, clock=time.monotonic) -> None:
        self.config = config
        self.clock = clock
        self._lock = threading.RLock()
        self._state = CircuitState.CLOSED
        self._opened_at: float | None = None
        self._half_open_inflight = 0
        self._events: deque[tuple[float, bool]] = deque()
        self._consecutive_failures = 0

    @property
    def state(self) -> CircuitState:
        with self._lock:
            return self._state

    def _purge(self, now: float) -> None:
        cutoff = now - self.config.window_seconds
        while self._events and self._events[0][0] < cutoff:
            self._events.popleft()

    def _timeout_ratio(self, now: float) -> float:
        self._purge(now)
        total = len(self._events)
        if not total:
            return 0.0
        return sum(1 for _, timed_out in self._events if timed_out) / total

    def snapshot(self, now: float | None = None) -> CircuitSnapshot:
        current = self.clock() if now is None else now
        with self._lock:
            self._purge(current)
            return CircuitSnapshot(
                state=self._state.value,
                consecutive_failures=self._consecutive_failures,
                samples=len(self._events),
                timeout_ratio=self._timeout_ratio(current),
                opened_for_seconds=(
                    max(0.0, current - self._opened_at)
                    if self._opened_at is not None
                    else None
                ),
                half_open_inflight=self._half_open_inflight,
            )

    def allow_request(
        self, now: float | None = None
    ) -> tuple[bool, str, CircuitTransition | None]:
        current = self.clock() if now is None else now
        with self._lock:
            transition: CircuitTransition | None = None
            if self._state == CircuitState.OPEN:
                assert self._opened_at is not None
                if current - self._opened_at < self.config.open_seconds:
                    return False, "circuit_open", None
                before = self._state
                self._state = CircuitState.HALF_OPEN
                self._half_open_inflight = 0
                transition = CircuitTransition(
                    from_state=before.value,
                    to_state=self._state.value,
                    reason="cooldown_elapsed_probe",
                    observed_at=current,
                    snapshot=asdict(self._snapshot_locked(current)),
                )
            if self._state == CircuitState.HALF_OPEN:
                if self._half_open_inflight >= self.config.half_open_max_calls:
                    return False, "half_open_probe_inflight", transition
                self._half_open_inflight += 1
            return True, "permitted", transition

    def record_success(
        self, now: float | None = None
    ) -> CircuitTransition | None:
        current = self.clock() if now is None else now
        with self._lock:
            if self._state == CircuitState.HALF_OPEN:
                self._half_open_inflight = max(0, self._half_open_inflight - 1)
                before = self._state
                self._state = CircuitState.CLOSED
                self._opened_at = None
                self._events.clear()
                self._consecutive_failures = 0
                return CircuitTransition(
                    from_state=before.value,
                    to_state=self._state.value,
                    reason="half_open_probe_succeeded",
                    observed_at=current,
                    snapshot=asdict(self._snapshot_locked(current)),
                )
            self._consecutive_failures = 0
            self._events.append((current, False))
            self._purge(current)
            return None

    def record_failure(
        self,
        *,
        timed_out: bool,
        reason: str,
        now: float | None = None,
    ) -> CircuitTransition | None:
        current = self.clock() if now is None else now
        with self._lock:
            if self._state == CircuitState.HALF_OPEN:
                self._half_open_inflight = max(0, self._half_open_inflight - 1)
                before = self._state
                self._state = CircuitState.OPEN
                self._opened_at = current
                return CircuitTransition(
                    from_state=before.value,
                    to_state=self._state.value,
                    reason=f"half_open_probe_failed:{reason}",
                    observed_at=current,
                    snapshot=asdict(self._snapshot_locked(current)),
                )
            if self._state == CircuitState.OPEN:
                return None

            self._events.append((current, timed_out))
            self._consecutive_failures += 1
            self._purge(current)
            timeout_ratio = self._timeout_ratio(current)
            should_open = (
                self._consecutive_failures >= self.config.failure_threshold
                or (
                    len(self._events) >= self.config.timeout_min_requests
                    and timeout_ratio >= self.config.timeout_ratio_threshold
                )
            )
            if not should_open:
                return None
            before = self._state
            self._state = CircuitState.OPEN
            self._opened_at = current
            trigger = (
                "consecutive_failures"
                if self._consecutive_failures >= self.config.failure_threshold
                else "timeout_ratio"
            )
            return CircuitTransition(
                from_state=before.value,
                to_state=self._state.value,
                reason=f"{trigger}:{reason}",
                observed_at=current,
                snapshot=asdict(self._snapshot_locked(current)),
            )

    def restore_state(self, state: str, *, now: float | None = None) -> None:
        """Roll back a published transition when its audit commit fails."""
        with self._lock:
            restored = CircuitState(state)
            self._state = restored
            self._half_open_inflight = 0
            if restored == CircuitState.OPEN:
                self._opened_at = self.clock() if now is None else now
            elif restored == CircuitState.CLOSED:
                self._opened_at = None
                self._consecutive_failures = 0
                self._events.clear()

    def _snapshot_locked(self, now: float) -> CircuitSnapshot:
        self._purge(now)
        return CircuitSnapshot(
            state=self._state.value,
            consecutive_failures=self._consecutive_failures,
            samples=len(self._events),
            timeout_ratio=(
                sum(1 for _, timed_out in self._events if timed_out) / len(self._events)
                if self._events
                else 0.0
            ),
            opened_for_seconds=(
                max(0.0, now - self._opened_at)
                if self._opened_at is not None
                else None
            ),
            half_open_inflight=self._half_open_inflight,
        )
