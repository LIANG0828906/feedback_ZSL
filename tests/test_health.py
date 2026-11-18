from __future__ import annotations

import unittest

from inference_router.config import HealthSpec, LayerSpec, ReplicaSpec
from inference_router.health import HealthManager


def _replica(layer: str = "l1", group: str = "g1", replica_id: str = "r1") -> ReplicaSpec:
    return ReplicaSpec(
        layer=layer,
        replica_group=group,
        replica_id=replica_id,
        model_path="/tmp/does-not-matter.onnx",
        listen_address=f"127.0.0.1:{22000 + sum(map(ord, replica_id))}",
        input_dim=64,
        output_dim=64,
    )


def _health() -> HealthSpec:
    return HealthSpec(
        window_size=8,
        min_samples=4,
        probe_interval_ms=100,
        probe_timeout_ms=50,
        probe_failures_to_evict=2,
        probe_successes_to_recover=2,
    )


class HealthManagerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.events = []
        layer = LayerSpec("l1", "embedding", 10.0, 40.0, ())
        self.manager = HealthManager(
            [_replica()],
            {"l1": layer},
            _health(),
            self.events.append,
        )

    def tearDown(self) -> None:
        self.manager.close()

    def test_window_p95_evicts_and_recovery_drops_old_history(self) -> None:
        emitted = []
        for _ in range(4):
            emitted.extend(self.manager.record_success("r1", 20.0))
        self.assertEqual(len(emitted), 1)
        self.assertEqual(emitted[0]["details"]["reason"], "window_p95_exceeded")
        state = self.manager.states["r1"]
        self.assertFalse(state.healthy)
        self.assertEqual(len(state.latencies_ms), 0)

        self.manager.on_probe_result("r1", True)
        recovery_events = self.manager.on_probe_result("r1", True)
        self.assertEqual(len(recovery_events), 1)
        self.assertTrue(state.healthy)
        self.assertEqual(len(state.latencies_ms), 0)

        self.manager.record_success("r1", 1.0)
        self.assertEqual(state.p95(), 1.0)
        self.assertLess(state.p95(), 20.0)

    def test_consecutive_probe_failures_evict(self) -> None:
        self.assertEqual(self.manager.on_probe_result("r1", False, "down"), [])
        events = self.manager.on_probe_result("r1", False, "down")
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["details"]["reason"], "continuous_probe_failure")
        self.assertFalse(self.manager.states["r1"].healthy)

    def test_timeout_counters_and_exclusion(self) -> None:
        second = _replica(replica_id="r2")
        manager = HealthManager(
            [self.manager.states["r1"].spec, second],
            {"l1": LayerSpec("l1", "embedding", 10.0, 40.0, ())},
            _health(),
            self.events.append,
        )
        try:
            manager.record_timeout("r1", 40.0)
            self.assertEqual(manager.states["r1"].timeout_count, 1)
            selected = manager.select_in_layer("l1", excluded={"r1"})
            self.assertIsNotNone(selected)
            self.assertEqual(selected.spec.replica_id, "r2")
        finally:
            manager.close()


if __name__ == "__main__":
    unittest.main()
