from __future__ import annotations

import json
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

from duty_handover.generator import generate_all
from duty_handover.http_api import create_server
from duty_handover.service import build_service


class HTTPAPITests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        root = Path(self.temp.name)
        data = root / "data"
        generate_all(data, seed=304)
        self.handover = json.loads((data / "handover.json").read_text(encoding="utf-8"))
        service = build_service(data_dir=data, db_path=root / "api.db")
        self.server = create_server(service, "127.0.0.1", 0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        host, port = self.server.server_address[:2]
        self.base = f"http://{host}:{port}"

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def request(
        self, method: str, path: str, payload: dict | None = None
    ) -> tuple[int, bytes, str]:
        data = None
        headers = {}
        if payload is not None:
            data = json.dumps(payload).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.base + path, data=data, headers=headers, method=method
        )
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, response.read(), response.headers.get(
                    "Content-Type", ""
                )
        except urllib.error.HTTPError as exc:
            try:
                return exc.code, exc.read(), exc.headers.get("Content-Type", "")
            finally:
                exc.close()

    def test_http_acceptance_path(self) -> None:
        status, body, content_type = self.request(
            "POST", "/api/handovers", {"handover": self.handover}
        )
        self.assertEqual(status, 201)
        self.assertIn("application/json", content_type)
        submitted = json.loads(body)
        handover_id = submitted["handover_id"]
        self.assertEqual(
            submitted["snapshot"]["decision"]["summary"]["overall_grade"], "P2"
        )

        status, body, _ = self.request(
            "POST", "/api/handovers", {"handover": self.handover}
        )
        self.assertEqual(status, 409)
        self.assertEqual(
            json.loads(body)["error"]["code"], "DUPLICATE_HANDOVER_TIMESTAMP"
        )

        status, body, _ = self.request(
            "GET", f"/api/status?handover_id={handover_id}"
        )
        self.assertEqual(status, 200)
        status_payload = json.loads(body)
        self.assertEqual(
            status_payload["replay"]["first_divergence_at"],
            "2026-11-18T15:30:00Z",
        )
        self.assertEqual(
            status_payload["latest_rejected_submission"]["error_code"],
            "DUPLICATE_HANDOVER_TIMESTAMP",
        )

        status, body, _ = self.request(
            "GET", f"/api/handovers/{handover_id}/replay?until=2026-11-18T16:00:00Z"
        )
        self.assertEqual(status, 200)
        replay = json.loads(body)
        self.assertEqual(replay["first_divergence_trigger_metric"], "error_budget_remaining_pct")
        self.assertLess(replay["point_count"], 100)

        status, body, _ = self.request(
            "GET", f"/api/handovers/{handover_id}/replay?until=2026-11-19T00:00:00Z"
        )
        self.assertEqual(status, 422)
        self.assertEqual(
            json.loads(body)["error"]["code"], "INSUFFICIENT_REPLAY_SAMPLES"
        )

        status, body, content_type = self.request("GET", "/metrics")
        self.assertEqual(status, 200)
        self.assertIn("text/plain", content_type)
        metrics = body.decode("utf-8")
        self.assertIn('handover_burn_rate_short{service="payment-gateway"}', metrics)
        self.assertIn("handover_snapshot_total 1", metrics)

        status, body, _ = self.request("GET", "/api/windows?service=order-api")
        self.assertEqual(status, 400)

        status, body, _ = self.request(
            "GET",
            "/api/windows?service=order-api&start=2026-11-18T16:30:00Z&end=2026-11-18T16:49:00Z",
        )
        self.assertEqual(status, 200)
        window = json.loads(body)
        self.assertEqual(window["latency_breach_minutes"], 20)
        self.assertEqual(window["availability_breach_minutes"], 0)
        self.assertGreater(window["burn_rate"], 600)

        raw = body.decode("utf-8")
        self.assertNotIn("/home/", raw)
        self.assertNotIn(str(self.temp.name), raw)


if __name__ == "__main__":
    unittest.main()
