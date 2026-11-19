from __future__ import annotations

import json
import unittest

from ordersync.metrics import MetricsCollector
from ordersync.replication import ReplicaMonitor

from .helpers import FakeProbe, base_config


class MetricsTests(unittest.TestCase):
    def _collector(self) -> MetricsCollector:
        probe = FakeProbe()
        probe.primary_watermark = 3
        probe.replica_watermark = 2
        monitor = ReplicaMonitor(probe, base_config().replication)
        monitor.collect_once()
        return MetricsCollector(monitor)

    def test_report_is_stably_sorted_and_has_no_environment_identifiers(self) -> None:
        metrics = self._collector()
        metrics.record_read("strong", 4, success=True)
        metrics.record_read(
            "eventual",
            5,
            success=False,
            consistency_error=True,
            degraded_primary=True,
        )
        metrics.record_write(7, success=False, code="E_WRITE_CONFLICT")
        metrics.record_inconsistency(
            "ORD-000002",
            [
                {
                    "field": "orders.status",
                    "primary": "CONFIRMED",
                    "replica": "CANCELLED",
                }
            ],
            classification="replica_behind",
            primary_watermark=3,
            replica_watermark=2,
        )
        metrics.record_switch(12, success=True)
        report = metrics.snapshot()
        rendered = json.dumps(report, sort_keys=True)
        self.assertNotIn("localhost", rendered)
        self.assertNotIn("/home/", rendered)
        self.assertEqual(
            report["inconsistencies"]["samples"][0]["order_no"],
            "ORD-000002",
        )
        self.assertEqual(report["reads"]["eventual"]["consistency_errors"], 1)
        self.assertEqual(report["writes"]["failures_by_code"]["E_WRITE_CONFLICT"], 1)
        self.assertEqual(report["switches"]["durations_ms"], [12])

    def test_same_events_produce_same_non_timing_report(self) -> None:
        first = self._collector()
        second = self._collector()
        for metrics in (first, second):
            metrics.record_read("session", 3, success=True)
            metrics.record_write(9, success=True)
            metrics.record_switch(30, success=True)
        self.assertEqual(first.snapshot(), second.snapshot())


if __name__ == "__main__":
    unittest.main()

