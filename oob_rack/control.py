from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from .emulator_state import FaultController
from .errors import OobError, PortInUseError


class ControlHandler(BaseHTTPRequestHandler):
    server_version = "OobControl/1.0"
    sys_version = ""
    protocol_version = "HTTP/1.1"

    @property
    def faults(self) -> FaultController:
        return self.server.faults  # type: ignore[attr-defined]

    def log_message(self, format, *args):
        return

    def _send(self, status: int, body: dict) -> None:
        encoded = json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(encoded)

    def _json_body(self) -> dict:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > 65536:
            raise ValueError("JSON body is required")
        body = json.loads(self.rfile.read(length).decode("utf-8"))
        if not isinstance(body, dict):
            raise ValueError("JSON object is required")
        return body

    def do_GET(self):
        path = urlparse(self.path)
        if path.path.rstrip("/") == "/faults":
            query = parse_qs(path.query)
            device_id = query.get("device_id", [None])[0]
            self._send(200, {"faults": self.faults.list(device_id)})
            return
        if path.path.rstrip("/") == "/health":
            self._send(200, {"status": "ok"})
            return
        self._send(404, {"error": {"code": "E_NOT_FOUND", "message": "not found"}})

    def do_POST(self):
        path = urlparse(self.path).path.rstrip("/")
        try:
            body = self._json_body()
            if path == "/faults":
                fault = self.faults.add(body)
                self._send(201, {"fault": fault})
                return
            if path == "/faults/clear":
                count = self.faults.clear(
                    fault_id=body.get("id"), device_id=body.get("device_id")
                )
                self._send(200, {"cleared": count})
                return
            self._send(404, {"error": {"code": "E_NOT_FOUND", "message": "not found"}})
        except (ValueError, json.JSONDecodeError) as exc:
            self._send(400, {"error": {"code": "E_REQUEST", "message": str(exc)}})
        except OobError as exc:
            self._send(exc.http_status, {"error": exc.as_dict()})

    def do_DELETE(self):
        path = urlparse(self.path)
        if path.path.rstrip("/") != "/faults":
            self._send(404, {"error": {"code": "E_NOT_FOUND", "message": "not found"}})
            return
        query = parse_qs(path.query)
        fault_id = query.get("id", [None])[0]
        device_id = query.get("device_id", [None])[0]
        try:
            count = self.faults.clear(
                fault_id=int(fault_id) if fault_id is not None else None,
                device_id=device_id,
            )
            self._send(200, {"cleared": count})
        except ValueError as exc:
            self._send(400, {"error": {"code": "E_REQUEST", "message": str(exc)}})


class ControlServer:
    def __init__(self, host: str, port: int, faults: FaultController):
        try:
            self.server = ThreadingHTTPServer((host, port), ControlHandler)
        except OSError as exc:
            raise PortInUseError(
                f"cannot bind control HTTP {host}:{port}",
                details={"transport": "tcp", "host": host, "port": port},
            ) from exc
        self.server.daemon_threads = True
        self.server.faults = faults  # type: ignore[attr-defined]
        self.thread = threading.Thread(
            target=self.server.serve_forever, name="fault-control", daemon=True
        )
        self.started = False

    def start(self) -> None:
        self.thread.start()
        self.started = True

    def stop(self) -> None:
        if self.started:
            self.server.shutdown()
        self.started = False
        self.server.server_close()
