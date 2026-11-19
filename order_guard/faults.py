from __future__ import annotations

import random
import threading
import time
from dataclasses import asdict, dataclass


class DatabaseUnavailable(RuntimeError):
    pass


class DatabaseBusy(RuntimeError):
    pass


class DatabaseTimeout(RuntimeError):
    pass


class DatabaseFailure(RuntimeError):
    pass


@dataclass(frozen=True)
class FaultState:
    mode: str
    delay_ms: int
    error_rate: float
    remaining_seconds: float | None

    def to_dict(self) -> dict[str, object]:
        return asdict(self)


class FaultController:
    """Runtime fault injection used by the load generator, never by service code."""

    MODES = {"none", "slow", "unavailable", "busy"}

    def __init__(self, *, clock=time.monotonic) -> None:
        self.clock = clock
        self._lock = threading.RLock()
        self._mode = "none"
        self._delay_ms = 0
        self._error_rate = 0.0
        self._until: float | None = None
        self._generation = 0
        self._random = random.Random()

    def set(
        self,
        mode: str,
        *,
        delay_ms: int = 0,
        error_rate: float = 1.0,
        duration_seconds: float | None = None,
    ) -> FaultState:
        if mode not in self.MODES:
            raise ValueError(f"unsupported fault mode: {mode}")
        if delay_ms < 0:
            raise ValueError("delay_ms must be >= 0")
        if not 0.0 <= error_rate <= 1.0:
            raise ValueError("error_rate must be between 0 and 1")
        if duration_seconds is not None and duration_seconds < 0:
            raise ValueError("duration_seconds must be >= 0")
        with self._lock:
            self._generation += 1
            generation = self._generation
            self._mode = mode
            self._delay_ms = delay_ms
            self._error_rate = error_rate
            self._until = (
                self.clock() + duration_seconds
                if duration_seconds is not None
                else None
            )
            state = self._state_locked()
        if duration_seconds is not None:
            timer = threading.Timer(duration_seconds, self._expire, args=(generation,))
            timer.daemon = True
            timer.start()
        return state

    def clear(self) -> FaultState:
        with self._lock:
            self._generation += 1
            self._mode = "none"
            self._delay_ms = 0
            self._error_rate = 0.0
            self._until = None
            return self._state_locked()

    def _expire(self, generation: int) -> None:
        with self._lock:
            if generation != self._generation:
                return
            self._mode = "none"
            self._delay_ms = 0
            self._error_rate = 0.0
            self._until = None

    def _state_locked(self) -> FaultState:
        now = self.clock()
        if self._until is not None and now >= self._until:
            self._mode = "none"
            self._delay_ms = 0
            self._error_rate = 0.0
            self._until = None
        return FaultState(
            mode=self._mode,
            delay_ms=self._delay_ms,
            error_rate=self._error_rate,
            remaining_seconds=(
                max(0.0, self._until - now) if self._until is not None else None
            ),
        )

    def snapshot(self) -> FaultState:
        with self._lock:
            return self._state_locked()

    def before_database_operation(self) -> None:
        with self._lock:
            state = self._state_locked()
            mode = state.mode
            delay_ms = state.delay_ms
            error_rate = state.error_rate
            should_fail = mode == "busy" and self._random.random() < error_rate
        if mode == "none":
            return
        if mode == "slow":
            time.sleep(delay_ms / 1000.0)
            return
        if mode == "unavailable":
            if self._random.random() < error_rate:
                raise DatabaseUnavailable("injected database unavailability")
            return
        if mode == "busy" and should_fail:
            raise DatabaseBusy("injected sqlite busy event")
