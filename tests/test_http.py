from __future__ import annotations

import json
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from pathlib import Path

from wal_archive.http_api import make_handler
from wal_archive.samplegen import generate_archive
from wal_archive.service import ArchiveService


class HttpTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.tmp.name) / "archive"
        generate_archive(cls.root, profile="small")
        cls.service = ArchiveService(cls.root, Path(cls.tmp.name) / "catalog.sqlite3")
        cls.server = ThreadingHTTPServer(
            ("127.0.0.1", 0), make_handler(cls.service)
        )
        cls.server.daemon_threads = True
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=2)
        cls.service.close()
        cls.tmp.cleanup()

    def _request(self, method: str, path: str, payload: dict | None = None):
        data = None
        headers = {}
        if payload is not None:
            data = json.dumps(payload).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}",
            data=data,
            headers=headers,
            method=method,
        )
        try:
            response = urllib.request.urlopen(request, timeout=10)
            return response.status, response.headers.get_content_type(), response.read()
        except urllib.error.HTTPError as exc:
            try:
                return exc.code, exc.headers.get_content_type(), exc.read()
            finally:
                exc.close()

    def test_http_workflow(self):
        status, _, body = self._request("POST", "/v1/scan")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["versions"], 40)

        status, _, body = self._request(
            "POST",
            "/v1/recovery-points",
            {
                "instance": "pg-55432",
                "timeline": 1,
                "target_time": "2026-01-01T00:00:14Z",
            },
        )
        self.assertEqual(status, 200)
        point = json.loads(body)["recovery_point"]
        self.assertEqual(point["chain_length"], 6)

        status, _, body = self._request(
            "POST",
            "/v1/recovery-points",
            {
                "instance": "pg-55432",
                "timeline": 1,
                "target_time": "2026-01-01T00:00:30Z",
            },
        )
        self.assertEqual(status, 409)
        error = json.loads(body)["error"]
        self.assertEqual(error["code"], "WAL011")
        self.assertEqual(error["details"]["gaps"], [7, 8])

        status, content_type, body = self._request(
            "GET", "/v1/status?instance=pg-55432&timeline=1"
        )
        self.assertEqual(status, 200)
        self.assertEqual(content_type, "application/json")
        self.assertEqual(json.loads(body)["chain_length"], 6)
        self.assertNotIn(str(self.root).encode(), body)

        status, content_type, body = self._request("GET", "/metrics")
        self.assertEqual(status, 200)
        self.assertIn("text/plain", content_type)
        self.assertIn(b"wal_archive_segments_total", body)

        status, _, body = self._request("GET", "/v1/report")
        self.assertEqual(status, 200)
        report = json.loads(body)
        self.assertEqual(len(report["recovery_points"]), 1)

    def test_http_validation_endpoint(self):
        status, _, body = self._request(
            "POST",
            "/v1/validate",
            {"instance": "pg-55432", "timeline": 1, "segno": 1},
        )
        self.assertEqual(status, 200)
        validations = json.loads(body)["validations"]
        self.assertTrue(validations)
        self.assertEqual(validations[0]["classification"], "complete")

    def test_http_not_found(self):
        status, _, body = self._request("GET", "/v1/recovery-points/rp-missing")
        self.assertEqual(status, 404)
        self.assertEqual(json.loads(body)["error"]["code"], "WAL018")


if __name__ == "__main__":
    unittest.main()
