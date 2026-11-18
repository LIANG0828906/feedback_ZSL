from __future__ import annotations

import unittest

from inference_router.stats import RequestStats


class StatsTests(unittest.TestCase):
    def test_quantiles_and_only_actual_layer_calls(self) -> None:
        stats = RequestStats(window_size=100, p99_threshold_ms=50.0)
        for index in range(100):
            calls = [
                {
                    "layer_id": "l1",
                    "replica_id": "l1-a",
                    "latency_ms": 1.0,
                    "timed_out": False,
                    "outcome": "success",
                }
            ]
            if index % 2 == 0:
                calls.append(
                    {
                        "layer_id": "l2",
                        "replica_id": "l2-a",
                        "latency_ms": 120.0,
                        "timed_out": True,
                        "outcome": "timeout",
                    }
                )
            stats.record_request(
                {
                    "created_at_ms": index,
                    "e2e_ms": float(index + 1),
                    "ok": index % 2 == 0,
                    "calls": calls,
                    "retry_switches": 0,
                }
            )
        snapshot = stats.snapshot()
        self.assertEqual(snapshot["layers"]["l1"]["calls"], 100)
        self.assertEqual(snapshot["layers"]["l2"]["calls"], 50)
        self.assertEqual(snapshot["layers"]["l2"]["timeouts"], 50)
        self.assertEqual(snapshot["replicas"]["l2-a"]["calls"], 50)
        self.assertGreaterEqual(snapshot["e2e"]["p99_ms"], 99.0)
        self.assertTrue(snapshot["p99_exceeded"])
        self.assertEqual(snapshot["culprit"]["replica_id"], "l2-a")
        self.assertEqual(snapshot["culprit"]["timeouts"], 50)

    def test_empty_window(self) -> None:
        snapshot = RequestStats(100, 120.0).snapshot()
        self.assertEqual(snapshot["e2e"]["p99_ms"], 0.0)
        self.assertEqual(snapshot["layers"], {})
        self.assertIsNone(snapshot["culprit"])


if __name__ == "__main__":
    unittest.main()
