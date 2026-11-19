from __future__ import annotations

import os
import tempfile
import threading
import time
import unittest

from app.config import TenantConfig
from app.database import Database
from app.dispatcher import DispatchManager
from app.http_client import HttpOutcome
from app.metrics import render_metrics


def tenant(
    tenant_id: str,
    priority: str = "high",
    *,
    concurrency: int = 2,
    retries: int = 1,
    backoff: int = 0,
    threshold: int = 2,
) -> TenantConfig:
    return TenantConfig(
        tenant_id=tenant_id,
        priority=priority,
        max_concurrency=concurrency,
        max_retries=retries,
        backoff_base_ms=backoff,
        timeout_ms=200,
        degrade_after_timeouts=threshold,
        receiver_url=f"http://{tenant_id}:8080",
    )


class FakeClient:
    def __init__(self, outcomes: list[HttpOutcome] | None = None):
        self.outcomes = list(outcomes or [])
        self.calls: list[dict[str, object]] = []

    def __call__(self, receiver_url: str, **kwargs: object) -> HttpOutcome:
        self.calls.append({"receiver_url": receiver_url, **kwargs})
        if self.outcomes:
            return self.outcomes.pop(0)
        return HttpOutcome(
            success=True,
            status_code=200,
            body="{}",
            kind="success",
            message="",
            timed_out=False,
            permanent=False,
            latency_ms=1.0,
        )


class DispatchTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.database = Database(os.path.join(self.temp.name, "test.db"))
        self.database.initialize()

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_priority_and_per_tenant_cap_do_not_block_other_tenants(self) -> None:
        self.database.sync_tenants(
            [
                tenant("high", "high", concurrency=1),
                tenant("low", "low", concurrency=1),
            ]
        )
        self.database.ingest_events(
            [
                ("high-1", "high", {"n": 1}),
                ("high-2", "high", {"n": 2}),
                ("low-1", "low", {"n": 3}),
            ]
        )
        first = self.database.claim_next("worker", 5000)
        self.assertEqual("high", first.tenant_id)
        second = self.database.claim_next("worker", 5000)
        self.assertEqual("low", second.tenant_id)
        third = self.database.claim_next("worker", 5000)
        self.assertIsNone(third)
        self.assertTrue(self.database.complete_success(first))
        fourth = self.database.claim_next("worker", 5000)
        self.assertEqual("high-2", fourth.event_id)

    def test_transient_failure_retries_then_succeeds(self) -> None:
        self.database.sync_tenants([tenant("a", retries=1)])
        self.database.ingest_events([("event-1", "a", {"ok": True})])
        first = self.database.claim_next("worker", 5000)
        action = self.database.complete_failure(
            first,
            kind="http_503",
            message="HTTP 503",
            timed_out=False,
            permanent=False,
        )
        self.assertEqual("retry", action)
        second = self.database.claim_next("worker", 5000)
        self.assertIsNotNone(second)
        self.assertEqual(1, second.attempts)
        self.assertTrue(self.database.complete_success(second))
        report = self.database.report()
        row = report["tenants"][0]
        self.assertEqual(1, row["delivered"])
        self.assertEqual(0, row["dead_letters"])
        self.assertEqual(1, row["retries"])

    def test_4xx_is_dead_lettered_without_retry(self) -> None:
        self.database.sync_tenants([tenant("a", retries=5)])
        self.database.ingest_events([("event-1", "a", {})])
        claim = self.database.claim_next("worker", 5000)
        action = self.database.complete_failure(
            claim,
            kind="http_422",
            message="HTTP 422",
            timed_out=False,
            permanent=True,
        )
        self.assertEqual("dead_letter", action)
        dead = self.database.dead_letters()
        self.assertEqual(1, len(dead))
        self.assertTrue(dead[0]["permanent_failure"])
        self.assertEqual(1, dead[0]["attempts"])
        self.assertIsNone(self.database.claim_next("worker", 5000))

    def test_timeout_degradation_quarantines_queue_and_recovers(self) -> None:
        self.database.sync_tenants(
            [tenant("slow", "low", concurrency=1, retries=5, threshold=1)]
        )
        self.database.ingest_events(
            [
                ("event-1", "slow", {}),
                ("event-2", "slow", {}),
                ("event-3", "slow", {}),
            ]
        )
        first = self.database.claim_next("worker", 5000)
        self.database.complete_failure(
            first,
            kind="timeout",
            message="timeout after 200ms",
            timed_out=True,
            permanent=False,
        )
        self.assertFalse(self.database.status()["tenants"][0]["degraded"])
        second = self.database.claim_next("worker", 5000)
        self.database.complete_failure(
            second,
            kind="timeout",
            message="timeout after 200ms",
            timed_out=True,
            permanent=False,
        )
        status = self.database.status()
        self.assertTrue(status["tenants"][0]["degraded"])
        self.assertGreaterEqual(status["tenants"][0]["queue_depth"], 1)
        self.assertIsNone(self.database.claim_next("worker", 5000))
        self.assertTrue(self.database.recover_tenant("slow"))
        self.assertIsNotNone(self.database.claim_next("worker", 5000))

    def test_restart_recovers_unfinished_dispatch_without_losing_event(self) -> None:
        self.database.sync_tenants([tenant("a")])
        self.database.ingest_events([("event-1", "a", {"v": 1})])
        first = self.database.claim_next("dead-process", 1000)
        self.assertIsNotNone(first)

        restarted = Database(self.database.path)
        restarted.initialize()
        recovered = restarted.claim_next(
            "new-process", 5000, current_ms=int(time.time() * 1000) + 2000
        )
        self.assertIsNotNone(recovered)
        self.assertEqual("event-1", recovered.event_id)
        self.assertTrue(restarted.complete_success(recovered))
        self.assertIsNone(restarted.claim_next("new-process", 5000))
        self.assertEqual(1, restarted.report()["tenants"][0]["delivered"])

    def test_manager_dispatches_once_and_metrics_are_stable(self) -> None:
        self.database.sync_tenants([tenant("a")])
        self.database.ingest_events(
            [("event-1", "a", {"v": 1}), ("event-2", "a", {"v": 2})]
        )
        client = FakeClient()
        manager = DispatchManager(self.database, client=client)
        self.assertTrue(manager.process_one())
        self.assertTrue(manager.process_one())
        self.assertFalse(manager.process_one())
        self.assertEqual(2, len(client.calls))
        metrics = render_metrics(self.database)
        self.assertIn(
            'webhook_dispatch_success_total{tenant="a"} 2', metrics
        )
        self.assertIn('webhook_tenant_degraded{tenant="a"} 0', metrics)
        report = self.database.report()
        self.assertEqual(2, report["totals"]["delivered"])
        self.assertEqual(0, report["totals"]["dead_letters"])

    def test_two_instances_share_the_per_tenant_concurrency_cap(self) -> None:
        self.database.sync_tenants(
            [tenant("a", concurrency=3, retries=0)]
        )
        self.database.ingest_events(
            [(f"event-{index}", "a", {}) for index in range(20)]
        )
        claims: list[object] = []
        lock = threading.Lock()
        errors: list[BaseException] = []

        def instance(owner: str) -> None:
            database = Database(self.database.path)
            try:
                for _ in range(10):
                    claim = database.claim_next(owner, 5000)
                    if claim is not None:
                        with lock:
                            claims.append(claim)
            except BaseException as exc:
                errors.append(exc)

        threads = [
            threading.Thread(target=instance, args=(f"instance-{index}",))
            for index in range(2)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual([], errors)
        self.assertEqual(3, len(claims))
        self.assertEqual(3, self.database.status()["tenants"][0]["inflight"])


if __name__ == "__main__":
    unittest.main()

