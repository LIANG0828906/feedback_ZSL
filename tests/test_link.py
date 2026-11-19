from edge_sync.config import load_config
from edge_sync.link import EgressLinkState, LinkEmulator, median, percentile


def _state(small_config, egress_index=0):
    config = load_config(small_config)
    egress = config.egresses[egress_index]
    return EgressLinkState(
        egress_id=egress.egress_id,
        egress_index=egress_index,
        seed=config.seed,
        base_rtt_ms=egress.rtt_ms,
        base_loss_pct=egress.loss_pct,
        bandwidth_mbps=egress.bandwidth_mbps,
        injections=config.injections,
    )


def test_loss_never_destroys_bytes(small_config):
    state = _state(small_config)
    state.base_loss_pct = 30.0
    emulator = LinkEmulator(state)
    payload = bytes((index % 251 for index in range(200_000)))
    # Feed in small writes too, so a tail loss on one call must be recovered
    # at the head of the next call while preserving strict byte ordering.
    delivered_parts = []
    for offset in range(0, len(payload), 7_777):
        delivered_parts.extend(emulator.process(payload[offset : offset + 7_777]))
    # A final empty round drains any segment withheld for an RTO recovery.
    delivered_parts.extend(emulator.process(b""))
    delivered = b"".join(delivered_parts)
    assert delivered == payload


def test_loss_is_deterministic(small_config):
    first = LinkEmulator(_state(small_config))
    second = LinkEmulator(_state(small_config))
    payload = b"z" * 100_000
    first.process(payload)
    second.process(payload)
    assert first.retrans_segments == second.retrans_segments
    assert first.rtt_samples == second.rtt_samples


def test_stats_header_is_stable_json(small_config):
    emulator = LinkEmulator(_state(small_config))
    emulator.process(b"a" * 10_000)
    header = emulator.stats_header()
    assert "X-Link-Stats" in header
    assert header["X-Link-Stats"].startswith("{")


def test_percentiles_and_median():
    values = [float(i) for i in range(1, 10)]
    assert median(values) == 5.0
    assert percentile(values, 50) == 5.0
    assert percentile(values, 90) == 8.0
    assert median([]) is None
