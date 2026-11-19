from __future__ import annotations

from order_guard.circuit import CircuitBreaker, CircuitState
from order_guard.config import CircuitConfig


class FakeClock:
    def __init__(self) -> None:
        self.value = 0.0

    def __call__(self) -> float:
        return self.value


def circuit_config() -> CircuitConfig:
    return CircuitConfig(
        failure_threshold=2,
        timeout_ratio_threshold=0.5,
        timeout_min_requests=4,
        window_seconds=10,
        open_seconds=2,
        half_open_max_calls=1,
        open_degrade_level=2,
        code="ORDERS_DB_CIRCUIT_OPEN",
    )


def test_consecutive_failures_open_and_half_open_recovers() -> None:
    clock = FakeClock()
    breaker = CircuitBreaker(circuit_config(), clock=clock)
    assert breaker.allow_request(clock.value)[0] is True
    assert breaker.record_failure(timed_out=False, reason="error") is None
    assert breaker.record_failure(timed_out=False, reason="error") is not None
    assert breaker.state == CircuitState.OPEN
    assert breaker.allow_request(clock.value)[0] is False

    clock.value = 2.1
    allowed, reason, transition = breaker.allow_request(clock.value)
    assert allowed is True
    assert reason == "permitted"
    assert transition is not None
    assert transition.to_state == "half_open"
    assert breaker.state == CircuitState.HALF_OPEN
    assert breaker.record_success(clock.value).to_state == "closed"
    assert breaker.state == CircuitState.CLOSED


def test_failed_half_open_probe_reopens() -> None:
    clock = FakeClock()
    breaker = CircuitBreaker(circuit_config(), clock=clock)
    breaker.record_failure(timed_out=True, reason="timeout")
    breaker.record_failure(timed_out=True, reason="timeout")
    clock.value = 2.5
    allowed, _, transition = breaker.allow_request(clock.value)
    assert allowed is True and transition is not None
    reopened = breaker.record_failure(timed_out=True, reason="probe_timeout")
    assert reopened is not None
    assert reopened.to_state == "open"
    assert "half_open_probe_failed" in reopened.reason
    assert breaker.state == CircuitState.OPEN
