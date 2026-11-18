from __future__ import annotations

import json
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
from pathlib import Path

from ordergroup.backends import SimulatedKafkaBackend
from ordergroup.config import Config
from ordergroup.coordinator import Coordinator
from ordergroup.generator import GenerationOptions, generate_events
from ordergroup.server import create_server


class HTTPTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        root = Path(self.tempdir.name)
        config = Config.load(None)
        config.data["backend"]["kind"] = "simulator"
        config.data["backend"]["simulator_dir"] = str(root / "sim")
        config.data["storage"]["path"] = str(root / "coordinator.sqlite3")
        config.data["rebalance"]["revoke_ack_timeout_ms"] = 0
        generate_events(
            GenerationOptions(
                count=36,
                seed=290,
                topic="order-events",
                partitions=12,
                backend="simulator",
                bootstrap_servers="127.0.0.1:9092",
                output_dir=root / "sim",
            )
        )
        backend = SimulatedKafkaBackend(root / "sim", 12)
        self.coordinator = Coordinator(config, backend)
        self.server = create_server(self.coordinator, "127.0.0.1", 0, quiet=True)
        self.thread = threading.Thread(
            target=self.server.serve_forever, daemon=True
        )
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"
        for _ in range(50):
            try:
                with urllib.request.urlopen(self.base + "/healthz", timeout=0.5):
                    break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.05)
        else:
            self.fail("test HTTP server did not become ready")

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.coordinator.stop()
        self.tempdir.cleanup()

    def post(self, path: str, body: dict | None = None) -> dict:
        data = json.dumps(body or {}).encode("utf-8")
        request = urllib.request.Request(
            self.base + path,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.loads(response.read())

    def get(self, path: str) -> tuple[int, str, str]:
        with urllib.request.urlopen(self.base + path, timeout=5) as response:
            return (
                response.status,
                response.headers.get("Content-Type", ""),
                response.read().decode("utf-8"),
            )

    def test_http_join_status_metrics_and_lag(self) -> None:
        joined = self.post(
            "/v1/members/join",
            {
                "member_id": "http-worker",
                "session_timeout_ms": 5000,
                "heartbeat_interval_ms": 500,
            },
        )
        self.assertTrue(joined["ok"])
        self.assertEqual(len(joined["owned_partitions"]), 12)
        status_code, content_type, body = self.get("/v1/status")
        self.assertEqual(status_code, 200)
        self.assertIn("application/json", content_type)
        status = json.loads(body)
        self.assertEqual(status["member_count"], 1)
        self.assertEqual(status["members"][0]["member_id"], "http-worker")
        metrics_code, metrics_type, metrics = self.get("/metrics")
        self.assertEqual(metrics_code, 200)
        self.assertIn("text/plain", metrics_type)
        self.assertIn("order_group_committed_offset", metrics)
        lag = self.post("/v1/lag/verify")
        self.assertEqual(lag["total_lag"], 36)
        self.assertEqual(len(lag["partitions"]), 12)


if __name__ == "__main__":
    unittest.main()

