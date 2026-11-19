from edge_sync.config import load_config
from edge_sync.supervisor import bring_up_stack
from edge_sync.workload import build_manifest


def test_no_fault_injection_has_full_success_and_no_switch(workspace):
    from tests.conftest import _write_config

    path = _write_config(workspace, "baseline", {"injections": []})
    config = load_config(path)
    stack = bring_up_stack(config)
    try:
        manifest = build_manifest(config)
        snapshot = stack.orchestrator.run(manifest)
        total = sum(task.shard_count for task in manifest)
        succeeded = sum(
            counts.get("succeeded", 0) for counts in snapshot["tasks"].values()
        )
        assert succeeded == total
        assert stack.store.fetch_all("SELECT COUNT(*) FROM switches")[0][0] == 0
        assert stack.store.fetch_all("SELECT COUNT(*) FROM shards WHERE status='failed'")[0][0] == 0
        # Healthy baseline keeps the configured low loss under thresholds.
        report = stack.orchestrator.report()
        assert all(egress["healthy"] for egress in report["egresses"].values())
    finally:
        stack.shutdown()
