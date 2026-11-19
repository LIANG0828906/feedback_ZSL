import json

from edge_sync.config import load_config
from edge_sync.supervisor import bring_up_stack
from edge_sync.workload import build_manifest


def test_rerun_same_shards_does_not_duplicate_commit(workspace):
    from tests.conftest import _write_config

    path = _write_config(workspace, "rerun")
    config = load_config(path)
    manifest = build_manifest(config)
    total = sum(task.shard_count for task in manifest)

    for _ in range(2):
        # keep=False preserves the target's authoritative commit log and the
        # service runtime rows, exercising the idempotent re-upload path.
        stack = bring_up_stack(config, reset=False)
        try:
            stack.orchestrator.run(manifest)
            failed = stack.store.fetch_all("SELECT COUNT(*) FROM shards WHERE status='failed'")[0][0]
            assert failed == 0
        finally:
            stack.shutdown()

    stack = bring_up_stack(config, reset=False)
    try:
        counts = stack.target_store.task_counts()
        assert sum(counts.values()) == total
        # The target primary key guarantees each (task, shard) is one row.
        duplicate_rows = stack.target_store._connection.execute(
            "SELECT COUNT(*) FROM (SELECT task_id, shard_seq FROM shard_commits "
            "GROUP BY task_id, shard_seq HAVING COUNT(*) > 1)"
        ).fetchone()[0]
        assert duplicate_rows == 0
    finally:
        stack.shutdown()


def test_failed_shard_records_stable_error_code(workspace):
    from tests.conftest import _write_config

    # Bring the stack up, then stop the target so proxies cannot connect;
    # bounded retries must end in a stable business error code.
    path = _write_config(workspace, "errors")
    config = load_config(path)
    stack = bring_up_stack(config)
    try:
        stack.target_server.shutdown()
        stack.target_server.server_close()
        manifest = build_manifest(config)
        stack.orchestrator.run(manifest)
        codes = {
            row[0]
            for row in stack.store.fetch_all(
                "SELECT DISTINCT error_code FROM attempts WHERE error_code IS NOT NULL"
            )
        }
        assert codes, "failed attempts must carry a stable business error code"
        assert all(isinstance(code, str) and code.isupper() for code in codes)
    finally:
        stack.shutdown()
