from __future__ import annotations

import json
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable

from prometheus_client import CONTENT_TYPE_LATEST, generate_latest


class StatusHTTPServer(ThreadingHTTPServer):
    allow_reuse_address = True
    daemon_threads = True


def serve_status_http(
    status_provider: Callable[[], str], host: str, port: int
) -> StatusHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        server_version = "TrainingStatus/1.0"

        def do_GET(self):
            path = self.path.split("?", 1)[0]
            if path == "/metrics":
                body = generate_latest()
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", CONTENT_TYPE_LATEST)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if path in ("/status", "/status.json"):
                body = status_provider().encode("utf-8")
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if path in ("/healthz", "/health"):
                body = json.dumps(
                    {"status": "ok"}, sort_keys=True, separators=(",", ":")
                ).encode("utf-8")
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            body = b'{"error":"not_found"}'
            self.send_response(HTTPStatus.NOT_FOUND)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format, *_args):
            return

    server = StatusHTTPServer((host, int(port)), Handler)
    import threading

    thread = threading.Thread(
        target=server.serve_forever,
        kwargs={"poll_interval": 0.2},
        name="status-http",
        daemon=True,
    )
    thread.start()
    return server
