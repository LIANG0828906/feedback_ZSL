from __future__ import annotations

import asyncio
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

from connectlab.attribution import (
    FAULT_SYN_NOT_REACHED_UPSTREAM,
    attribute,
    build_coverage,
)
from connectlab.db import Database, DuplicateBatchError
from connectlab.events import EventStore
from connectlab.injection import InjectionPlan


class EventStoreTests(unittest.IsolatedAsyncioTestCase):
    async def test_close_is_idempotent_and_open_after_close_fails(self) -> None:
        store = EventStore()
        event = await store.add(
            batch_id="b",
            request_id="r",
            hop="proxy",
            connection_id="p-1",
            correlation_id="corr-p-1",
            kind="connect_success",
            initiator="proxy",
        )
        self.assertEqual(event["kind"], "connect_success")
        await store.add(
            batch_id="b",
            request_id="r",
            hop="proxy",
            connection_id="p-1",
            correlation_id="corr-p-1",
            kind="close",
            initiator="proxy",
        )
        duplicate = await store.add(
            batch_id="b",
            request_id="r",
            hop="proxy",
            connection_id="p-1",
            correlation_id="corr-p-1",
            kind="close",
            initiator="peer",
        )
        self.assertTrue(duplicate["duplicate_suppressed"])
        with self.assertRaises(ValueError):
            await store.add(
                batch_id="b",
                request_id="r2",
                hop="proxy",
                connection_id="p-1",
                correlation_id="corr-p-1",
                kind="connect_success",
                initiator="proxy",
            )
        snapshot = await store.snapshot("b")
        self.assertEqual(
            [item["kind"] for item in snapshot],
            ["connect_success", "close"],
        )


class InjectionTests(unittest.TestCase):
    def test_ratio_quota_and_path_scope(self) -> None:
        plans = [
            SimpleNamespace(path="short" if index % 2 else "keepalive", request_id=f"r{index}")
            for index in range(10)
        ]
        profile = {
            "activated_at": "2026-01-01T00:00:00.000Z",
            "duration_ms": 600_000,
            "expires_at": "2026-01-01T00:10:00.000Z",
            "hop": "proxy_to_upstream",
            "id": "test",
            "params": {},
            "path": "short",
            "ratio": 0.4,
            "seed": 318,
            "type": "syn_drop",
        }
        plan = InjectionPlan(
            profile,
            "batch",
            plans,
            now_ms=1_767_225_600_000,
        )
        self.assertEqual(len(plan.eligible_request_ids), 5)
        self.assertEqual(len(plan.matched_request_ids), 2)
        self.assertTrue(
            all(
                request_id in plan.eligible_request_ids
                for request_id in plan.matched_request_ids
            )
        )


class CoverageAndAttributionTests(unittest.TestCase):
    def test_missing_upstream_is_explicit(self) -> None:
        events = [
            {"hop": "client", "kind": "request_sent", "request_id": "r"},
            {"hop": "proxy", "kind": "request_received", "request_id": "r"},
        ]
        coverage, missing_hops, missing_events = build_coverage(events, "r", "reset")
        self.assertEqual(coverage["upstream"]["status"], "missing_hop")
        self.assertEqual(missing_hops, ["upstream"])
        self.assertIn({"event": "request_received", "hop": "upstream"}, missing_events)

    def test_syn_event_has_attribution_precedence(self) -> None:
        events = [
            {
                "connection_id": "p-up-1",
                "details": {"syn_ack_received": False},
                "event_id": 1,
                "hop": "proxy",
                "initiator": "local_fault_gate",
                "kind": "syn_dropped",
                "request_id": "r",
                "ts": "now",
            },
            {
                "connection_id": "c-1",
                "details": {},
                "event_id": 2,
                "hop": "client",
                "initiator": "peer",
                "kind": "reset",
                "request_id": "r",
                "ts": "now",
            },
        ]
        result = attribute(request_id="r", outcome="reset", events=events)
        self.assertEqual(result["fault_type"], FAULT_SYN_NOT_REACHED_UPSTREAM)
        self.assertEqual(result["evidence_event_id"], 1)
        self.assertEqual(result["edge"], "proxy_to_upstream")


class DatabaseTests(unittest.TestCase):
    def test_duplicate_batch_does_not_overwrite(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            database = Database(Path(directory) / "test.sqlite3")
            batch = {
                "batch_id": "same",
                "status": "completed",
                "started_at": "start",
                "finished_at": "finish",
                "target_profile": "local-a",
                "target": {"proxy_port": 1, "upstream_port": 2},
                "batch_config": {"count": 0},
                "injection": {"id": "none"},
                "summary": {"resets": 0},
                "request_count": 0,
                "completed_count": 0,
                "unfinished_count": 0,
                "error": None,
            }
            database.commit_batch(batch=batch, requests=[], events=[])
            with self.assertRaises(DuplicateBatchError):
                database.commit_batch(
                    batch={**batch, "summary": {"resets": 99}},
                    requests=[],
                    events=[],
                )
            stored = database.get_batch("same")
            self.assertEqual(stored["summary"], {"resets": 0})


if __name__ == "__main__":
    unittest.main()
