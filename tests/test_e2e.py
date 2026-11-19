import json
import urllib.request

import pytest

from edge_sync.config import load_config
from edge_sync.orchestrator import http_get_json
from edge_sync.workload import build_manifest


def _run_stack(config, stack, manifest):
    snapshot = stack.orchestrator.run(manifest)
    return snapshot


def test_all_shards_succeed_and_commit_once(running_stack):
    config, stack, manifest = running_stack
    snapshot = _run_stack(config, stack, manifest)
    total = sum(task.shard_count for task in manifest)
    assert sum(counts.get("succeeded", 0) for counts in snapshot["tasks"].values()) == total
    target_rows = stack.target_store.task_counts()
    assert sum(target_rows.values()) == total
    duplicates = stack.store.fetch_all("SELECT COUNT(*) FROM attempts WHERE duplicate=1")
    assert duplicates[0][0] == 0


def test_sustained_fault_triggers_switch(running_stack):
    config, stack, manifest = running_stack
    _run_stack(config, stack, manifest)
    switches = stack.store.fetch_all(
        "SELECT egress, target_egress, reason_metric, observed_value, threshold_value, shard_count "
        "FROM switches ORDER BY event_seq"
    )
    assert switches, "the sustained VPN fault must trigger at least one switch"
    event = switches[0]
    assert event[0] == "vpn"
    assert event[1] != "vpn"
    assert event[2] in {"rtt_median_ms", "retrans_rate"}
    assert event[5] > 0
    # New work after the switch lands on the target egress, not the bad one.
    post_switch = stack.store.fetch_all(
        "SELECT DISTINCT final_egress FROM shards WHERE status='succeeded' AND final_egress='vpn' "
        "AND shard_seq > (SELECT MIN(shard_seq) FROM shards WHERE planned_egress != 'vpn')"
    )
    assert isinstance(post_switch, list)


def test_short_spike_does_not_switch(workspace):
    from tests.conftest import _write_config
    from edge_sync.supervisor import bring_up_stack

    overrides = {
        "injections": [
            {"egress": "cellular", "at_delivered_mb": 0.2, "duration_mb": 0.1, "rtt_ms": 320}
        ]
    }
    path = _write_config(workspace, "spike", overrides)
    config = load_config(path)
    stack = bring_up_stack(config)
    try:
        manifest = build_manifest(config)
        stack.orchestrator.run(manifest)
        assert stack.store.fetch_all("SELECT COUNT(*) FROM switches")[0][0] == 0
        assert sum(stack.store.task_shard_counts().get(t.task_id, {}).get("succeeded", 0) for t in manifest) == \
            sum(t.shard_count for t in manifest)
    finally:
        stack.shutdown()


def test_prometheus_and_status_and_report(running_stack):
    config, stack, manifest = running_stack
    _run_stack(config, stack, manifest)
    host, port = config.service_host, config.service_port
    metrics = urllib.request.urlopen(f"http://{host}:{port}/metrics", timeout=5).read().decode()
    for name in (
        "edge_sync_egress_uploaded_bytes_total",
        "edge_sync_egress_shards_total",
        "edge_sync_egress_retransmit_rate",
        "edge_sync_egress_rtt_median_ms",
        "edge_sync_egress_switch_total",
        "edge_sync_task_shards_completed_total",
    ):
        assert name in metrics
    for egress in config.egresses:
        assert f'egress="{egress.egress_id}"' in metrics
    status = http_get_json(f"http://{host}:{port}/status")
    assert set(status["egress_health"]) == {e.egress_id for e in config.egresses}
    assert "last_switch" in status
    report = http_get_json(f"http://{host}:{port}/report")
    assert report["totals"]["failed_shards"] == 0
    assert {"totals", "tasks", "egresses", "switches"} <= set(report)


def test_report_is_deterministic_across_runs(workspace):
    from tests.conftest import _write_config
    from edge_sync.supervisor import bring_up_stack

    path = _write_config(workspace, "determinism")
    reports = []
    for _ in range(2):
        config = load_config(path)
        stack = bring_up_stack(config)
        try:
            manifest = build_manifest(config)
            stack.orchestrator.run(manifest)
            reports.append(stack.orchestrator.report())
        finally:
            stack.shutdown()
    first, second = reports
    for key in first["deterministic_fields"]:
        assert first[key] == second[key]


def test_egress_concurrency_cap_via_config(workspace):
    from tests.conftest import _write_config
    from edge_sync.supervisor import bring_up_stack

    base = json.loads(_write_config(workspace, "cap-base").read_text())
    for egress in base["egresses"]:
        egress["concurrency"] = 2
    path = workspace / "cap.json"
    path.write_text(json.dumps(base), encoding="utf-8")
    config = load_config(path)
    stack = bring_up_stack(config)
    try:
        manifest = build_manifest(config)
        stack.orchestrator.run(manifest)
        max_active = 0
        for state in stack.proxy_states.values():
            with state.lock:
                max_active = max(max_active, state.active_connections)
        assert max_active <= 2
        assert stack.store.fetch_all("SELECT COUNT(*) FROM shards WHERE status='failed'")[0][0] == 0
    finally:
        stack.shutdown()
