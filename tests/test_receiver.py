from __future__ import annotations

import threading
import unittest

from app.http_client import post_event
from receiver.server import ReceiverServer, ReceiverState


class ReceiverTests(unittest.TestCase):
    def setUp(self) -> None:
        self.state = ReceiverState()
        self.state.tenant_id = "tenant-a"
        self.state.base_latency_ms = 0
        self.server = ReceiverServer(("127.0.0.1", 0), self.state)
        self.thread = threading.Thread(
            target=self.server.serve_forever, daemon=True
        )
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def post(self, timeout_ms: int = 1000):
        return post_event(
            self.url,
            event_id="event-1",
            tenant_id="tenant-a",
            payload={"value": 1},
            attempt=1,
            timeout_ms=timeout_ms,
        )

    def test_success_and_duplicate_idempotency(self) -> None:
        first = self.post()
        second = self.post()
        self.assertTrue(first.success)
        self.assertTrue(second.success)
        stats = self.state.snapshot()
        self.assertEqual(1, stats["accepted"])
        self.assertEqual(1, stats["duplicates"])

    def test_5xx_is_retryable(self) -> None:
        self.state.error_ratio = 1.0
        outcome = self.post()
        self.assertFalse(outcome.success)
        self.assertEqual(503, outcome.status_code)
        self.assertFalse(outcome.permanent)
        self.assertFalse(outcome.timed_out)

    def test_4xx_is_permanent(self) -> None:
        self.state.error_ratio = 0.0
        self.state.permanent_4xx_ratio = 1.0
        outcome = self.post()
        self.assertFalse(outcome.success)
        self.assertEqual(422, outcome.status_code)
        self.assertTrue(outcome.permanent)

    def test_slow_response_times_out(self) -> None:
        self.state.slow_ratio = 1.0
        self.state.slow_min_ms = 200
        self.state.slow_max_ms = 200
        outcome = self.post(timeout_ms=20)
        self.assertFalse(outcome.success)
        self.assertTrue(outcome.timed_out)
        self.assertEqual("timeout", outcome.kind)


if __name__ == "__main__":
    unittest.main()

