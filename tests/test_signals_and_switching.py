import json

from edge_sync import ERR_SIGNAL_UNSUPPORTED
from edge_sync.config import load_config
from edge_sync.tcp_probe import TcpSignals
from edge_sync.workload import build_manifest


def test_delta_handles_missing_fields():
    before = TcpSignals(platform="linux", retrans_segments=2, rto_count=None)
    before.missing = ["rto_count"]
    before.missing_codes = [ERR_SIGNAL_UNSUPPORTED]
    after = TcpSignals(platform="linux", retrans_segments=5, rto_count=None)
    after.missing = ["rto_count"]
    after.missing_codes = [ERR_SIGNAL_UNSUPPORTED]
    delta = after.delta(before)
    assert delta["retrans_delta"] == 3
    assert delta["rto_count_delta"] is None
    assert ERR_SIGNAL_UNSUPPORTED in delta["missing_codes"]


def test_unsupported_platform_is_marked_not_fabricated(monkeypatch):
    import edge_sync.tcp_probe as tcp_probe

    monkeypatch.setattr(tcp_probe.sys, "platform", "plan9")
    signals = tcp_probe.probe(object())
    assert signals.rtt_ms is None
    assert signals.cwnd_segments is None
    assert ERR_SIGNAL_UNSUPPORTED in signals.missing_codes


def test_every_attempt_carries_os_and_link_signals(running_stack):
    config, stack, manifest = running_stack
    stack.orchestrator.run(manifest)
    rows = stack.store.fetch_all("SELECT signal_json, link_json FROM attempts")
    assert rows
    for signal_raw, link_raw in rows:
        signal = json.loads(signal_raw)
        link = json.loads(link_raw)
        assert signal["platform"] in {"linux", "windows"}
        assert link["source"] == "link_emulation"
        assert "rtt_samples_ms" in link and "cwnd_segments_approx" in link


def test_switch_only_moves_pending_shards(running_stack):
    config, stack, manifest = running_stack
    stack.orchestrator.run(manifest)
    switches = stack.store.fetch_all(
        "SELECT egress, target_egress, shard_count FROM switches"
    )
    assert switches
    for bad_egress, target_egress, count in switches:
        assert target_egress != bad_egress
        assert count >= 1
        # No successful shard is ever recorded twice at the target.
        duplicated = stack.store.fetch_all(
            "SELECT COUNT(*) FROM attempts WHERE duplicate=1 AND success=1"
        )
        assert duplicated[0][0] == 0


def test_post_switch_shards_use_target_egress(workspace):
    from tests.conftest import _write_config
    from edge_sync.supervisor import bring_up_stack

    # Heavy, long VPN degradation guarantees a switch with pending work.
    overrides = {
        "injections": [
            {"egress": "vpn", "at_delivered_mb": 0.05, "duration_mb": 50, "loss_pct": 25, "rtt_ms": 260}
        ]
    }
    path = _write_config(workspace, "post-switch", overrides)
    config = load_config(path)
    stack = bring_up_stack(config)
    try:
        manifest = build_manifest(config)
        stack.orchestrator.run(manifest)
        switched = stack.store.fetch_all(
            "SELECT target_egress FROM switches WHERE egress='vpn' ORDER BY event_seq"
        )
        assert switched
        target = switched[0][0]
        # The egress that absorbed switched shards must have successful uploads.
        counts = stack.store.fetch_all(
            "SELECT final_egress, COUNT(*) FROM shards WHERE status='succeeded' GROUP BY final_egress"
        )
        distribution = {egress: count for egress, count in counts}
        assert distribution.get(target, 0) > 0
        assert stack.store.fetch_all("SELECT COUNT(*) FROM shards WHERE status='failed'")[0][0] == 0
    finally:
        stack.shutdown()
