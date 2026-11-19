from __future__ import annotations

import hashlib
from pathlib import Path

from orders_migration.ledger import Ledger


def test_dual_write_state_and_trace_share_a_transaction(
    tmp_path: Path,
) -> None:
    ledger = Ledger(tmp_path / "audit.db")
    try:
        ledger.record_dual_intent(
            write_id="w1",
            order_id=10,
            tenant_id=2,
            revision=3,
            payload_hash=hashlib.sha256(b"x").hexdigest(),
            legacy_before=None,
            shard_before=None,
        )
        assert ledger.dual_write("w1")["status"] == "PREPARING"
        ledger.complete_dual(
            "w1",
            legacy_commit_site="legacy:1",
            shard_commit_site="shard2:1",
        )
        row = ledger.dual_write("w1")
        assert row["status"] == "COMMITTED"
        trace = ledger.trace_order(10)
        assert [event["event_type"] for event in trace["events"]] == [
            "DUAL_WRITE_PREPARING",
            "DUAL_WRITE_COMMITTED",
        ]
    finally:
        ledger.close()


def test_completed_check_scope_is_reused_not_overwritten(
    tmp_path: Path,
) -> None:
    ledger = Ledger(tmp_path / "audit.db")
    try:
        first, created = ledger.create_check_run(
            run_id="c1",
            scope_key="scope-a",
            scope={"tenantId": 2},
        )
        assert created is True
        ledger.append_check_batch(
            run_id="c1",
            cursor_order_id=10,
            total_checked=10,
            mismatches=[
                {
                    "orderId": 10,
                    "tenantId": 2,
                    "fieldName": "amount",
                    "legacyValue": "1.00",
                    "shardValue": "2.00",
                }
            ],
        )
        ledger.complete_check("c1", cursor_order_id=10, total_checked=10)
        second, created = ledger.create_check_run(
            run_id="c2",
            scope_key="scope-a",
            scope={"tenantId": 2},
        )
        assert created is False
        assert second["run_id"] == first["run_id"]
        assert second["mismatch_count"] == 1
    finally:
        ledger.close()


def test_rollback_tenant_completion_is_durable(tmp_path: Path) -> None:
    ledger = Ledger(tmp_path / "audit.db")
    try:
        ledger.create_rollback_run(
            run_id="r1", scope={"tenants": [2, 3]}, pending=[2, 3]
        )
        ledger.record_rollback_batch(
            run_id="r1",
            tenant_id=2,
            actions=[
                {
                    "orderId": 1,
                    "action": "COPY_SHARD_TO_LEGACY",
                    "source": "shard",
                    "detail": {},
                }
            ],
            cursor_order_id=1,
        )
        ledger.finish_rollback_tenant("r1", 2, 1)
        run = ledger.rollback_run("r1")
        assert run["completed_tenants_json"] == "[2]"
        assert run["pending_tenants_json"] == "[3]"
    finally:
        ledger.close()

