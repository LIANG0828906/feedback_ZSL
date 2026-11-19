from __future__ import annotations

import pytest

from order_guard.config import SLIConfig
from order_guard.sli import SLIRegistry


def availability_config() -> SLIConfig:
    return SLIConfig(
        name="availability",
        kind="availability",
        window_seconds=10,
        target=0.9,
        target_ms=None,
        allowed_bad_ratio=0.01,
        min_samples=2,
        degrade_level=2,
        recover_budget_ratio=0.2,
    )


def latency_config() -> SLIConfig:
    return SLIConfig(
        name="latency",
        kind="p99_latency",
        window_seconds=10,
        target=None,
        target_ms=100,
        allowed_bad_ratio=0.01,
        min_samples=2,
        degrade_level=1,
        recover_budget_ratio=0.2,
    )


def test_availability_budget_exhaustion_and_value() -> None:
    registry = SLIRegistry({"availability": availability_config()})
    registry.record_query(success=True, latency_ms=5, observed_at=100)
    registry.record_query(success=False, latency_ms=5, observed_at=101)
    snapshot = registry.snapshot("availability", now=102)
    assert snapshot.availability == 0.5
    assert snapshot.error_budget_consumed == pytest.approx(5.0)
    assert snapshot.error_budget_remaining == 0.0
    assert snapshot.breached is True
    assert snapshot.to_dict()["value"] == 0.5


def test_latency_p99_and_sliding_window_purge() -> None:
    registry = SLIRegistry({"latency": latency_config()})
    registry.record_query(success=True, latency_ms=20, observed_at=100)
    registry.record_query(success=True, latency_ms=200, observed_at=101)
    snapshot = registry.snapshot("latency", now=102)
    assert snapshot.p99_latency_ms == 200
    assert snapshot.error_budget_consumed == 50.0
    assert snapshot.breached is True

    recovered = registry.snapshot("latency", now=112)
    assert recovered.samples == 0
    assert recovered.breached is False
    assert recovered.recovered is True
