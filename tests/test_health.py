from edge_sync.config import load_config
from edge_sync.health import HealthMonitor, Observation


def _obs(rtt=None, rate=0.0, emulated_ms=100.0):
    return Observation(rtt_median_ms=rtt, retrans_rate=rate, emulated_ms=emulated_ms, wall_ms=1_000)


def test_short_spike_does_not_switch(small_config):
    config = load_config(small_config)
    monitor = HealthMonitor(config)
    # Two bad samples stay below min_bad_samples=3 even if each is large.
    for _ in range(2):
        assert monitor.observe("vpn", _obs(rtt=260, emulated_ms=80)) is None
    assert monitor.is_healthy("vpn")
    # A third sample that alone would cross the jitter tolerance still needs
    # the streak requirement, so reset exposure and keep it short.
    assert monitor.observe("vpn", _obs(rtt=260, emulated_ms=80)) is None
    assert monitor.is_healthy("vpn")


def test_sustained_degradation_switches(small_config):
    config = load_config(small_config)
    monitor = HealthMonitor(config)
    trigger = None
    for _ in range(config.health.min_bad_samples):
        trigger = monitor.observe("vpn", _obs(rtt=260, emulated_ms=120))
    assert trigger is not None
    assert trigger["egress"] == "vpn"
    assert trigger["reason_metric"] == "rtt_median_ms"
    assert trigger["threshold_value"] == config.health.rtt_median_ms
    assert not monitor.is_healthy("vpn")


def test_retrans_rate_threshold_switches(small_config):
    config = load_config(small_config)
    monitor = HealthMonitor(config)
    trigger = None
    for _ in range(config.health.min_bad_samples):
        trigger = monitor.observe("cellular", _obs(rate=0.25, emulated_ms=120))
    assert trigger is not None
    assert trigger["reason_metric"] == "retrans_rate"


def test_recovery_requires_cooldown_and_good_samples(small_config):
    config = load_config(small_config)
    monitor = HealthMonitor(config)
    for _ in range(4):
        monitor.observe("vpn", _obs(rtt=260, emulated_ms=120))
    assert not monitor.is_healthy("vpn")
    # Immediately observing good samples is still inside the cooldown window.
    assert monitor.observe("vpn", _obs(rtt=10, emulated_ms=10)) is None
    snapshot = monitor.monitors["vpn"]
    snapshot.cooldown_until_ms = 0
    for _ in range(config.health.min_bad_samples):
        result = monitor.observe("vpn", _obs(rtt=10, emulated_ms=10))
    assert result is not None and result["reason_metric"] == "recovered"
    assert monitor.is_healthy("vpn")
