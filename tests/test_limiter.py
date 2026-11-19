from __future__ import annotations

from order_guard.config import TenantConfig
from order_guard.limiter import TenantLimiterRegistry


class FakeClock:
    def __init__(self, value: float = 0.0) -> None:
        self.value = value

    def __call__(self) -> float:
        return self.value


def test_concurrency_and_rate_rejections_are_tenant_local() -> None:
    clock = FakeClock()
    registry = TenantLimiterRegistry(
        [
            TenantConfig("hot", rate_per_second=1, burst=1, max_inflight=1),
            TenantConfig("narrow", rate_per_second=1, burst=10, max_inflight=1),
            TenantConfig("good", rate_per_second=10, burst=3, max_inflight=2),
        ],
        clock=clock,
    )
    first = registry.acquire("hot")
    assert first.allowed is True
    second = registry.acquire("hot")
    assert second.allowed is False
    assert second.reason == "rate"

    assert registry.acquire("narrow").allowed is True
    narrow_second = registry.acquire("narrow")
    assert narrow_second.allowed is False
    assert narrow_second.reason == "concurrency"

    good = registry.acquire("good")
    assert good.allowed is True
    registry.release("hot")
    registry.release("narrow")

    clock.value += 0.01
    hot_rate = registry.acquire("hot")
    assert hot_rate.allowed is False
    assert hot_rate.reason == "rate"
    assert hot_rate.retry_after_ms >= 1

    clock.value += 1.0
    recovered = registry.acquire("hot")
    assert recovered.allowed is True


def test_disabled_or_unknown_tenants_do_not_consume_quota() -> None:
    registry = TenantLimiterRegistry(
        [
            TenantConfig(
                "disabled",
                rate_per_second=10,
                burst=2,
                max_inflight=1,
                enabled=False,
            )
        ]
    )
    assert registry.acquire("disabled").reason == "tenant_not_active"
    assert registry.acquire("missing").reason == "tenant_not_active"
    assert registry.total_inflight() == 0
