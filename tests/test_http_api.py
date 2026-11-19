from __future__ import annotations

import http.client
import json
import threading
import unittest

from ordersync.faults import ReplicationFaultController
from ordersync.http_api import Application, ServiceHttpServer
from ordersync.metrics import MetricsCollector
from ordersync.replication import ReplicaMonitor
from ordersync.service import OrderService

from .helpers import (
    FakeGateway,
    FakeProbe,
    base_config,
    make_order,
    topology_snapshot,
)


class HttpApiTests(unittest.TestCase):
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
        faults = ReplicationFaultController(
            self.service, self.monitor
        )
        self.httpd = ServiceHttpServer(
            ("127.0.0.1", 0),
            Application(self.service, faults),
        )
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(
            target=self.httpd.serve_forever, daemon=True
        )
        self.thread.start()

    def tearDown(self) -> None:
        self.httpd.shutdown()
        self.httpd.server_close()
        self.thread.join(timeout=5)
        self.monitor.stop()
        self.service.close()

    def _request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, dict, dict[str, str]]:
        connection = http.client.HTTPConnection(
            "127.0.0.1", self.port, timeout=5
        )
        request_headers = dict(headers or {})
        raw = None
        if body is not None:
            raw = json.dumps(body).encode("utf-8")
            request_headers["Content-Type"] = "application/json"
        connection.request(method, path, body=raw, headers=request_headers)
        response = connection.getresponse()
        payload = json.loads(response.read().decode("utf-8"))
        response_headers = dict(response.getheaders())
        connection.close()
        return response.status, payload, response_headers

    def test_write_and_session_read_over_http(self) -> None:
        order = {
            "order_no": "ORD-HTTP-1",
            "tenant_id": "T01",
            "status": "CONFIRMED",
            "lines": [
                {
                    "line_no": 1,
                    "sku": "SKU-1",
                    "quantity": 1,
                    "unit_price": "12.50",
                }
            ],
        }
        status, payload, headers = self._request(
            "POST", "/orders", order, {"X-Session-Id": "s-http"}
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["commit_watermark"], "1")
        self.assertEqual(headers["X-Commit-Watermark"], "1")

        self.replica.orders["ORD-HTTP-1"] = make_order("ORD-HTTP-1", 1)
        self.probe.primary_watermark = 1
        self.probe.replica_watermark = 1
        self.monitor.collect_once()
        status, payload, headers = self._request(
            "GET",
            "/orders/ORD-HTTP-1?consistency=session",
            headers={"X-Session-Id": "s-http"},
        )
        self.assertEqual(status, 200)
        self.assertEqual(payload["consistency"]["served_by"], "replica")
        self.assertEqual(headers["X-Consistency-Served-By"], "replica")
        self.assertEqual(headers["X-Consistency-Degraded"], "false")

    def test_report_endpoint_is_json_and_resettable(self) -> None:
        status, payload, _ = self._request("GET", "/report")
        self.assertEqual(status, 200)
        self.assertEqual(payload["schema_version"], 1)
        status, payload, _ = self._request("POST", "/admin/report/reset")
        self.assertEqual(status, 200)
        self.assertTrue(payload["reset"])
        status, payload, _ = self._request("GET", "/report")
        self.assertEqual(status, 200)
        self.assertEqual(payload["reads"]["strong"]["requests"], 0)

    def test_invalid_consistency_returns_business_error(self) -> None:
        status, payload, _ = self._request(
            "GET", "/orders/ORD-X?consistency=bogus"
        )
        self.assertEqual(status, 409)
        self.assertEqual(payload["error"]["code"], "E_CONSISTENCY")
        self.assertEqual(
            payload["error"]["reason"], "invalid_consistency_level"
        )


if __name__ == "__main__":
    unittest.main()

