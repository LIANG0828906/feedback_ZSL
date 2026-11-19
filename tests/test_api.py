from __future__ import annotations

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request

from app.config import TenantConfig
from app.database import Database
from app.server import WebhookApiServer


class ApiTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.database = Database(os.path.join(self.temp.name, "api.db"))
        self.database.initialize()
        self.database.sync_tenants(
            [
                TenantConfig(
                    tenant_id="tenant-a",
                    priority="high",
                    max_concurrency=2,
                    max_retries=1,
                    backoff_base_ms=10,
                    timeout_ms=500,
                    degrade_after_timeouts=2,
                    receiver_url="http://127.0.0.1:1",
                )
            ]
        )
        self.server = WebhookApiServer(("127.0.0.1", 0), self.database)
        self.thread = threading.Thread(
            target=self.server.serve_forever, daemon=True
        )
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_address[1]}"

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def request(
        self, path: str, *, method: str = "GET", body: object | None = None
    ) -> tuple[int, bytes]:
        data = (
            json.dumps(body, sort_keys=True).encode("utf-8")
            if body is not None
            else None
        )
        request = urllib.request.Request(
            self.base + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as exc:
            body = exc.read()
            exc.close()
            return exc.code, body

    def test_ingest_status_metrics_and_report(self) -> None:
        status, body = self.request(
            "/v1/events/batch",
            method="POST",
            body={
                "events": [
                    {
                        "event_id": "event-1",
                        "tenant_id": "tenant-a",
                        "payload": {"value": 1},
                    }
                ]
            },
        )
        self.assertEqual(200, status)
        self.assertEqual(1, json.loads(body)["inserted"])

        status, body = self.request("/v1/status")
        self.assertEqual(200, status)
        status_body = json.loads(body)
        self.assertEqual(1, status_body["tenants"][0]["queue_depth"])
        self.assertEqual(0, status_body["tenants"][0]["inflight"])
        self.assertIn("last_dispatch_at", status_body["tenants"][0])

        status, body = self.request("/metrics")
        self.assertEqual(200, status)
        metrics = body.decode()
        self.assertIn(
            'webhook_dispatch_success_total{tenant="tenant-a"} 0', metrics
        )
        self.assertIn(
            'webhook_tenant_queue_depth{tenant="tenant-a"} 1', metrics
        )

        status, body = self.request("/v1/report")
        self.assertEqual(200, status)
        report = json.loads(body)
        self.assertEqual(1, report["tenants"][0]["events_submitted"])
        self.assertNotIn("generated_at", report)

    def test_unknown_tenant_is_rejected_with_business_code(self) -> None:
        status, body = self.request(
            "/v1/events",
            method="POST",
            body={"event_id": "e", "tenant_id": "missing", "payload": {}},
        )
        self.assertEqual(422, status)
        error = json.loads(body)["error"]
        self.assertEqual("EVENT_TENANT_UNKNOWN", error["business_code"])

    def test_event_id_conflict_rolls_back_batch(self) -> None:
        first = {
            "event_id": "event-1",
            "tenant_id": "tenant-a",
            "payload": {"value": 1},
        }
        self.request("/v1/events", method="POST", body=first)
        status, body = self.request(
            "/v1/events/batch",
            method="POST",
            body={
                "events": [
                    {
                        "event_id": "event-2",
                        "tenant_id": "tenant-a",
                        "payload": {},
                    },
                    {
                        "event_id": "event-1",
                        "tenant_id": "tenant-a",
                        "payload": {"value": 2},
                    },
                ]
            },
        )
        self.assertEqual(409, status)
        self.assertEqual(
            "EVENT_ID_CONFLICT",
            json.loads(body)["error"]["business_code"],
        )
        report = self.database.report()["tenants"][0]
        self.assertEqual(1, report["events_submitted"])


if __name__ == "__main__":
    unittest.main()

