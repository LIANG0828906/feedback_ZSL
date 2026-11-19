from __future__ import annotations

import time
import unittest

from ordersync.errors import AppError
from ordersync.db import DatabaseUnavailableError, PoolExhaustedError
from ordersync.metrics import MetricsCollector
from ordersync.replication import ReplicaMonitor
from ordersync.service import OrderService

from .helpers import (
    FakeGateway,
    FakeProbe,
    base_config,
    make_order,
    topology_snapshot,
    valid_write,
)


class ServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.config = base_config()
        self.primary = FakeGateway()
        self.replica = FakeGateway()
        self.probe = FakeProbe()
        self.monitor = ReplicaMonitor(self.probe, self.config.replication)
        self.metrics = MetricsCollector(self.monitor)
        self.service = OrderService(
            self.config,
            self.primary,
            self.replica,
            self.monitor,
            self.metrics,
            topology_snapshot(),
        )

    def _sync_replica(self) -> None:
        self.probe.primary_watermark = self.primary.watermark
        self.probe.replica_watermark = self.primary.watermark
        self.monitor.collect_once()

    def test_write_returns_one_comparable_watermark_for_order_and_lines(self) -> None:
        response = self.service.write_order(
            valid_write("ORD-1"), {"X-Session-Id": "s1"}
        )
        watermark = int(response["commit_watermark"])
        self.assertEqual(watermark, 1)
        self.assertEqual(response["session_id"], "s1")
        stored = self.primary.orders["ORD-1"]
        self.assertEqual(stored["write_watermark"], watermark)
        self.assertEqual(
            stored["lines"][0]["write_watermark"], watermark
        )
        self.assertEqual(self.service.sessions.watermark("s1"), watermark)

    def test_session_read_requires_replica_watermark(self) -> None:
        self.service.write_order(valid_write("ORD-1"), {"X-Session-Id": "s1"})
        self.replica.orders["ORD-1"] = make_order("ORD-1", 1)
        self.probe.primary_watermark = 1
        self.probe.replica_watermark = 0
        self.monitor.collect_once()
        with self.assertRaises(AppError) as raised:
            self.service.read_order(
                "ORD-1", "session", {"X-Session-Id": "s1"}
            )
        self.assertEqual(raised.exception.code, "E_CONSISTENCY")
        self.assertEqual(
            raised.exception.details["replica_applied_watermark"], "0"
        )
        self.assertEqual(raised.exception.details["required_watermark"], "1")

        self.probe.replica_watermark = 1
        self.monitor.collect_once()
        response = self.service.read_order(
            "ORD-1", "session", {"X-Session-Id": "s1"}
        )
        self.assertEqual(response["consistency"]["served_by"], "replica")
        self.assertEqual(
            response["consistency"]["replica_applied_watermark"], "1"
        )

    def test_eventual_read_falls_back_with_visible_reason(self) -> None:
        self.primary.orders["ORD-1"] = make_order("ORD-1", 1)
        self.primary.watermark = 1
        self.probe.primary_watermark = 1
        self.probe.replica_watermark = 0
        self.monitor.record_write(1, time.monotonic() - 2)
        self.monitor.collect_once()
        response = self.service.read_order(
            "ORD-1", "eventual", {}
        )
        self.assertEqual(response["consistency"]["served_by"], "primary")
        self.assertTrue(response["consistency"]["degraded"])
        self.assertEqual(
            response["consistency"]["degraded_reason"], "replication_lag"
        )
        report = self.metrics.snapshot()
        self.assertEqual(
            report["reads"]["eventual"]["degraded_primary_reads"], 1
        )

    def test_strong_read_ignores_replica_failure(self) -> None:
        self.primary.orders["ORD-1"] = make_order("ORD-1", 1)
        self.probe.replica_ok = False
        self.monitor.collect_once()
        response = self.service.read_order("ORD-1", "strong", {})
        self.assertEqual(response["consistency"]["served_by"], "primary")
        self.assertFalse(response["consistency"]["degraded"])

    def test_connection_pool_exhaustion_is_not_masked_as_read_failure(self) -> None:
        self.primary.read_error = PoolExhaustedError("full")
        with self.assertRaises(AppError) as raised:
            self.service.read_order("ORD-1", "strong", {})
        self.assertEqual(raised.exception.code, "E_CONN_POOL")

    def test_invalid_consistency_returns_stable_code(self) -> None:
        with self.assertRaises(AppError) as raised:
            self.service.read_order("ORD-1", "linearizable", {})
        self.assertEqual(raised.exception.code, "E_CONSISTENCY")
        self.assertEqual(
            raised.exception.details["reason"], "invalid_consistency_level"
        )

    def test_write_conflict_is_counted_without_changing_session(self) -> None:
        self.primary.write_error = AppError(
            "E_WRITE_CONFLICT", "duplicate", 409
        )
        with self.assertRaises(AppError) as raised:
            self.service.write_order(
                valid_write("ORD-1"), {"X-Session-Id": "s1"}
            )
        self.assertEqual(raised.exception.code, "E_WRITE_CONFLICT")
        self.assertEqual(self.service.sessions.watermark("s1"), 0)
        report = self.metrics.snapshot()
        self.assertEqual(report["writes"]["failures"], 1)
        self.assertEqual(
            report["writes"]["failures_by_code"]["E_WRITE_CONFLICT"], 1
        )

    def test_primary_outage_never_fabricates_a_replica_write(self) -> None:
        self.primary.write_error = DatabaseUnavailableError("down")
        with self.assertRaises(AppError) as raised:
            self.service.write_order(
                valid_write("ORD-DOWN"), {"X-Session-Id": "s1"}
            )
        self.assertEqual(raised.exception.code, "E_WRITE_UNAVAILABLE")
        self.assertEqual(self.replica.write_calls, [])
        self.assertEqual(self.service.sessions.watermark("s1"), 0)

    def test_controlled_switch_waits_then_reverses_routing(self) -> None:
        self.service.write_order(valid_write("ORD-1"), {"X-Session-Id": "s1"})
        self.replica.orders = dict(self.primary.orders)
        self.replica.watermark = self.primary.watermark
        self._sync_replica()
        old_primary = self.primary
        old_replica = self.replica

        response = self.service.controlled_switch()
        self.assertTrue(response["switched"])
        self.assertIs(self.service._primary(), old_replica)
        self.assertIs(self.service._replica(), old_primary)

        second = self.service.write_order(
            valid_write("ORD-2"), {"X-Session-Id": "s1"}
        )
        self.assertEqual(second["commit_watermark"], "2")
        self.assertIn("ORD-2", old_replica.orders)
        self.assertNotIn("ORD-2", old_primary.orders)
        report = self.metrics.snapshot()
        self.assertEqual(report["switches"]["succeeded"], 1)
        self.assertEqual(len(report["switches"]["durations_ms"]), 1)

    def test_controlled_switch_aborts_when_replica_cannot_catch_up(self) -> None:
        self.primary.watermark = 5
        self.probe.primary_watermark = 5
        self.probe.replica_watermark = 4
        self.monitor.collect_once()
        with self.assertRaises(AppError) as raised:
            self.service.controlled_switch()
        self.assertEqual(raised.exception.code, "E_SWITCH_ABORTED")
        self.assertIs(self.service._primary(), self.primary)
        self.assertFalse(self.service.write_gate.closed)
        report = self.metrics.snapshot()
        self.assertEqual(report["switches"]["aborted"], 1)


if __name__ == "__main__":
    unittest.main()

