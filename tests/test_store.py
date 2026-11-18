from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from inference_router.store import RequestStore


class RequestStoreTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.store = RequestStore(str(Path(self.temp.name) / "router.sqlite3"))

    def tearDown(self) -> None:
        self.store.close()
        self.temp.cleanup()

    def test_claim_and_final_commit_are_idempotent(self) -> None:
        features = [float(index) / 10.0 for index in range(64)]
        state, row = self.store.claim_request("req-1", "sess-1", features)
        self.assertEqual(state, "new")
        self.assertIsNone(row)
        state, row = self.store.claim_request("req-1", "sess-1", features)
        self.assertEqual(state, "in_progress")
        self.assertEqual(row["session_id"], "sess-1")

        response = {
            "ok": True,
            "request_id": "req-1",
            "session_id": "sess-1",
            "output": [1.0, 2.0],
            "l1_replica_id": "l1-a",
            "l2_replica_id": "l2-a",
            "e2e_ms": 3.0,
            "retry_switches": 0,
        }
        calls = [
            {
                "layer_id": "l1",
                "replica_id": "l1-a",
                "session_id": "sess-1",
                "attempt": 1,
                "latency_ms": 1.0,
                "timed_out": False,
                "outcome": "success",
                "error_code": None,
            },
            {
                "layer_id": "l2",
                "replica_id": "l2-a",
                "session_id": "sess-1",
                "attempt": 1,
                "latency_ms": 2.0,
                "timed_out": False,
                "outcome": "success",
                "error_code": None,
            },
        ]
        events = [
            {
                "event_type": "retry_switch",
                "layer_id": "l2",
                "replica_id": "l2-a",
                "details": {"from_replica_id": "l2-b"},
                "created_at_ms": 1,
            }
        ]
        self.assertTrue(
            self.store.complete_request(
                "req-1", ok=True, response=response, calls=calls, events=events
            )
        )
        second = {**response, "output": [999.0], "l2_replica_id": "other"}
        self.assertFalse(
            self.store.complete_request(
                "req-1", ok=True, response=second, calls=[], events=[]
            )
        )
        stored = self.store.get_request("req-1")
        self.assertEqual(stored["status"], "succeeded")
        self.assertEqual(stored["response"]["output"], [1.0, 2.0])
        self.assertEqual(len(stored["calls"]), 2)
        self.assertEqual(len(stored["events"]), 1)
        for stored_value, expected in zip(stored["features"], features):
            self.assertAlmostEqual(stored_value, expected, places=6)

    def test_pending_request_listing(self) -> None:
        self.store.claim_request("pending", "sess-p", [0.0] * 64)
        pending = self.store.pending_requests()
        self.assertEqual([item["request_id"] for item in pending], ["pending"])


if __name__ == "__main__":
    unittest.main()
