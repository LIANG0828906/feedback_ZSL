from __future__ import annotations

import time
import unittest

from ordersync.config import ReplicationConfig
from ordersync.replication import ReplicaMonitor

from .helpers import FakeProbe


class MonitorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.probe = FakeProbe()
        self.config = ReplicationConfig(
            mode="log_shipping",
            poll_interval_ms=100,
            watermark_wait_timeout_ms=80,
            lag_threshold_ms=1000,
            lag_window_seconds=30,
            sync_stall_ms=300,
        )
        self.monitor = ReplicaMonitor(self.probe, self.config)

    def test_lag_degrades_then_recovers_on_next_sample(self) -> None:
        self.probe.primary_watermark = 5
        self.probe.replica_watermark = 3
        self.monitor.record_write(5, time.monotonic() - 2.0)
        snapshot = self.monitor.collect_once()
        self.assertTrue(snapshot.replication_connected)
        self.assertEqual(snapshot.degraded_reason, "replication_lag")

        self.probe.replica_watermark = 5
        snapshot = self.monitor.collect_once()
        self.assertTrue(snapshot.replication_connected)
        self.assertIsNone(snapshot.degraded_reason)
        self.assertEqual(snapshot.watermark_gap, 0)

    def test_replica_failure_degrades_and_recovers(self) -> None:
        self.probe.primary_watermark = 4
        self.probe.replica_watermark = 4
        self.monitor.collect_once()
        self.probe.replica_ok = False
        snapshot = self.monitor.collect_once()
        self.assertEqual(snapshot.degraded_reason, "replica_unavailable")
        self.assertFalse(snapshot.replication_connected)
        self.probe.replica_ok = True
        snapshot = self.monitor.collect_once()
        self.assertIsNone(snapshot.degraded_reason)
        self.assertTrue(snapshot.replication_connected)

    def test_wait_returns_only_when_replica_is_reachable_and_caught_up(self) -> None:
        self.probe.primary_watermark = 4
        self.probe.replica_watermark = 2
        self.monitor.collect_once()
        snapshot = self.monitor.wait_for_watermark(3, timeout_ms=20)
        self.assertLess(snapshot.replica_watermark or 0, 3)
        self.probe.replica_watermark = 4
        self.monitor.collect_once()
        snapshot = self.monitor.wait_for_watermark(3, timeout_ms=20)
        self.assertGreaterEqual(snapshot.replica_watermark or 0, 3)

    def test_watermark_observations_are_reported(self) -> None:
        self.probe.primary_watermark = 6
        self.probe.replica_watermark = 5
        self.monitor.record_write(6)
        self.monitor.collect_once()
        observations = self.monitor.observation_report()
        self.assertEqual(observations["watermark_gap_samples"], [1])
        self.assertEqual(observations["visibility_latency_samples_ms"], [])
        self.probe.replica_watermark = 6
        self.monitor.collect_once()
        observations = self.monitor.observation_report()
        self.assertEqual(observations["watermark_gap_samples"], [1, 0])
        self.assertEqual(observations["visibility_latency_samples_ms"], [0])

    def test_swap_roles_reverses_probe_direction(self) -> None:
        self.probe.primary_watermark = 9
        self.probe.replica_watermark = 8
        self.monitor.collect_once()
        self.monitor.swap_roles()
        self.assertEqual(self.probe.swaps, 1)
        snapshot = self.monitor.snapshot()
        self.assertEqual(snapshot.primary_watermark, 8)
        self.assertEqual(snapshot.replica_watermark, 9)


if __name__ == "__main__":
    unittest.main()

