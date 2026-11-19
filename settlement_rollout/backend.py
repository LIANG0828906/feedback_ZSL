from __future__ import annotations

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlparse


def _json_bytes(value: dict[str, Any]) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def normalize_faults(raw: dict[str, Any] | None) -> dict[str, Any]:
    raw = raw or {}
    latency_ms = float(raw.get("latency_ms", 0.0))
    error_percent = float(raw.get("error_percent", 0.0))
    error_status = int(raw.get("error_status", 500))
    health_fail_ms = int(raw.get("health_fail_ms", 0))
    if latency_ms < 0:
        raise ValueError("latency_ms must be >= 0")
    if error_percent < 0 or error_percent > 100:
        raise ValueError("error_percent must be between 0 and 100")
    if error_status < 500 or error_status > 599:
        raise ValueError("error_status must be a 5xx status")
    if health_fail_ms < 0:
        raise ValueError("health_fail_ms must be >= 0")
    return {
        "latency_ms": round(latency_ms, 6),
        "error_percent": round(error_percent, 6),
        "error_status": error_status,
        "health_fail_ms": health_fail_ms,
    }


class VersionBackend:
    def __init__(
        self,
        instance_id: str,
        version: str,
        faults: dict[str, Any] | None = None,
    ) -> None:
        self.instance_id = instance_id
        self.version = version
        self.faults = normalize_faults(faults)
        self.started_monotonic = time.monotonic()
        self.request_count = 0
        self._lock = threading.Lock()

    def next_request(self) -> tuple[int, bool]:
        with self._lock:
            self.request_count += 1
            sequence = self.request_count
        threshold = int(round(self.faults["error_percent"] * 100))
        bucket = ((sequence - 1) * 7919) % 10000
        return sequence, bucket < threshold

    def healthy(self) -> bool:
        elapsed_ms = (time.monotonic() - self.started_monotonic) * 1000.0
        return elapsed_ms >= self.faults["health_fail_ms"]


class BackendHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "SettlementBackend/1.0"

    @property
    def backend(self) -> VersionBackend:
        return self.server.backend  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def _send_json(self, status: int, value: dict[str, Any]) -> None:
        body = _json_bytes(value)
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.send_header("X-Instance-Version", self.backend.version)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        path = urlparse(self.path).path
        if path == "/health":
            if self.backend.healthy():
                self._send_json(
                    200,
                    {
                        "ok": True,
                        "instance_id": self.backend.instance_id,
                        "version": self.backend.version,
                    },
                )
            else:
                self._send_json(
                    503,
                    {
                        "ok": False,
                        "code": "HEALTH_CHECK_FAILED",
                        "instance_id": self.backend.instance_id,
                        "version": self.backend.version,
                    },
                )
            return
        if path == "/settle":
            sequence, should_error = self.backend.next_request()
            latency_ms = float(self.backend.faults["latency_ms"])
            if latency_ms > 0:
                time.sleep(latency_ms / 1000.0)
            status = (
                int(self.backend.faults["error_status"]) if should_error else 200
            )
            payload = {
                "ok": not should_error,
                "instance_id": self.backend.instance_id,
                "version": self.backend.version,
                "request_seq": sequence,
                "simulated_latency_ms": latency_ms,
            }
            body = _json_bytes(payload)
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.send_header("X-Instance-Version", self.backend.version)
            self.send_header("X-Simulated-Latency-Ms", str(latency_ms))
            self.end_headers()
            self.wfile.write(body)
            return
        if path == "/admin/status":
            self._send_json(
                200,
                {
                    "ok": True,
                    "instance_id": self.backend.instance_id,
                    "version": self.backend.version,
                    "healthy": self.backend.healthy(),
                    "request_count": self.backend.request_count,
                    "faults": self.backend.faults,
                },
            )
            return
        self._send_json(404, {"ok": False, "code": "NOT_FOUND"})

    def do_POST(self) -> None:  # noqa: N802
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b"{}"
        if path == "/admin/faults":
            try:
                payload = json.loads(raw.decode("utf-8"))
                self.backend.faults = normalize_faults(payload)
            except (ValueError, json.JSONDecodeError) as exc:
                self._send_json(
                    400,
                    {
                        "ok": False,
                        "code": "INVALID_FAULTS",
                        "message": str(exc),
                    },
                )
                return
            self._send_json(200, {"ok": True, "faults": self.backend.faults})
            return
        if path == "/admin/shutdown":
            self._send_json(200, {"ok": True})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        self._send_json(404, {"ok": False, "code": "NOT_FOUND"})


class BackendServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 64

    def __init__(
        self,
        address: tuple[str, int],
        backend: VersionBackend,
    ) -> None:
        super().__init__(address, BackendHandler)
        self.backend = backend


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Settlement version backend")
    parser.add_argument("--instance-id", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--port", required=True, type=int)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--faults-json", default="{}")
    args = parser.parse_args(argv)
    faults = json.loads(args.faults_json)
    backend = VersionBackend(args.instance_id, args.version, faults)
    server = BackendServer((args.host, args.port), backend)
    try:
        server.serve_forever(poll_interval=0.1)
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
