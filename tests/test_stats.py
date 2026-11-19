"""Rolling window percentiles, error rates and circuit breaker behaviour."""

from gateway.stats import CircuitBreaker, RollingStats, percentile


def test_percentile_interpolation():
    values = sorted([10.0, 20.0, 30.0, 40.0])
    assert percentile(values, 0.5) == 25.0
    assert percentile(values, 0.99) > 39.0
    assert percentile([], 0.99) == 0.0


def test_rolling_window_segregates_versions_and_tenants():
    stats = RollingStats(window_seconds=60.0)
    for index in range(10):
        stats.record("v4", "tenant-a", 100.0, is_error=False, is_timeout=False)
    stats.record("v3", "tenant-b", 5.0, is_error=True, is_timeout=False)

    v4 = stats.version_summary("v4")
    v3 = stats.version_summary("v3")
    assert v4["count"] == 10 and v4["error_count"] == 0
    assert v3["count"] == 1 and v3["error_rate"] == 1.0
    assert stats.tenant_summary("tenant-a")["count"] == 10
    assert stats.tenant_summary("tenant-b")["error_count"] == 1


def test_window_evicts_old_samples():
    stats = RollingStats(window_seconds=0.05)
    stats.record("v4", "t", 1.0, False, False)
    import time

    time.sleep(0.08)
    assert stats.version_summary("v4")["count"] == 0


def test_breaker_trips_on_p99_once_with_metadata():
    breaker = CircuitBreaker("v4", p99_threshold_ms=50.0, error_rate_threshold=0.5, min_samples=5)
    quiet = {"count": 4, "p99_ms": 999.0, "error_rate": 0.0, "timeout_count": 0}
    assert breaker.evaluate(quiet) is None  # below min sample size

    summary = {"count": 8, "p99_ms": 120.0, "error_rate": 0.1, "timeout_count": 2}
    record = breaker.evaluate(summary)
    assert record and record["sample_count"] == 8
    assert any("p99" in reason for reason in record["reasons"])
    assert breaker.evaluate(summary) is None  # no duplicate trips
    assert breaker.tripped and breaker.record()["canary_version"] == "v4"


def test_breaker_trips_on_error_rate():
    breaker = CircuitBreaker("v4", p99_threshold_ms=500.0, error_rate_threshold=0.5, min_samples=4)
    record = breaker.evaluate(
        {"count": 10, "p99_ms": 10.0, "error_rate": 0.6, "timeout_count": 0}
    )
    assert record is not None
    assert any("error_rate" in reason for reason in record["reasons"])
