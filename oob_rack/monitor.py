from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from .errors import OobError, PortInUseError
from .service import InspectorService


class MonitorHandler(BaseHTTPRequestHandler):
    server_version = "OobMonitor/1.0"
    sys_version = ""
    protocol_version = "HTTP/1.1"

    @property
    def service(self) -> InspectorService:
        return self.server.service  # type: ignore[attr-defined]

    def log_message(self, format, *args):
        return

    def _send_bytes(self, status: int, content_type: str, body: bytes) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, status: int, body: dict) -> None:
        encoded = json.dumps(
            body, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")
        self._send_bytes(status, "application/json; charset=utf-8", encoded)

    def _send_error(self, status: int, code: str, message: str) -> None:
        self._send_json(status, {"error": {"code": code, "message": message}})

    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        config = self.service.config_manager.config
        try:
            if path == "/":
                self._send_json(
                    200,
                    {
                        "service": "oob-rack-inspector",
                        "endpoints": [
                            "/status",
                            "/metrics",
                            "/health",
                            "/api/config",
                            "/api/inspections/{id}",
                            "/api/sensors",
                        ],
                    },
                )
                return
            if path in ("/status", "/report"):
                rack_id = query.get("rack", [config["rack_id"]])[0]
                self._send_json(200, self.service.storage.latest_status(rack_id))
                return
            if path == "/health":
                self._send_json(
                    200,
                    {
                        "status": "ok",
                        "config_generation": self.service.config_manager.generation,
                        "last_cycle": self.service.last_cycle,
                        "last_error": self.service.last_error,
                    },
                )
                return
            if path == "/metrics":
                snapshot = self.service.storage.metrics_snapshot(config)
                text = self.service.storage.render_prometheus(snapshot)
                self._send_bytes(
                    200, "text/plain; version=0.0.4; charset=utf-8", text.encode("utf-8")
                )
                return
            if path == "/api/config":
                self._send_json(
                    200,
                    {
                        "rack_id": config["rack_id"],
                        "config_generation": self.service.config_manager.generation,
                        "config_fingerprint": self.service.config_manager.fingerprint,
                        "devices": [
                            {
                                "device_id": device["device_id"],
                                "firmware_profile": device["firmware_profile"],
                                "firmware_version": device["firmware_version"],
                            }
                            for device in config["devices"]
                        ],
                        "firmware_profiles": {
                            name: {
                                "protocols": profile["protocols"],
                                "power_state_shape": profile["power_state_shape"],
                            }
                            for name, profile in config["firmware_profiles"].items()
                        },
                        "thresholds": config["thresholds"],
                    },
                )
                return
            if path.startswith("/api/inspections/"):
                identifier = path.rsplit("/", 1)[-1]
                try:
                    inspection_id = int(identifier)
                except ValueError:
                    self._send_error(400, "E_REQUEST", "inspection id must be an integer")
                    return
                detail = self.service.storage.inspection_detail(inspection_id)
                if detail is None:
                    self._send_error(404, "E_NOT_FOUND", "inspection not found")
                    return
                self._send_json(200, detail)
                return
            if path == "/api/sensors":
                device_id = query.get("device_id", [None])[0]
                sensor = query.get("sensor", [None])[0]
                if not device_id or not sensor:
                    self._send_error(
                        400, "E_REQUEST", "device_id and sensor query parameters are required"
                    )
                    return
                try:
                    limit = int(query.get("limit", ["1000"])[0])
                except ValueError:
                    self._send_error(400, "E_REQUEST", "limit must be an integer")
                    return
                rows = self.service.storage.sensor_series(
                    device_id=device_id,
                    sensor_name=sensor,
                    start=query.get("from", [None])[0],
                    end=query.get("to", [None])[0],
                    limit=limit,
                )
                self._send_json(
                    200,
                    {
                        "device_id": device_id,
                        "sensor": sensor,
                        "count": len(rows),
                        "observations": rows,
                    },
                )
                return
            self._send_error(404, "E_NOT_FOUND", "not found")
        except OobError as exc:
            self._send_json(exc.http_status, {"error": exc.as_dict()})
        except Exception as exc:
            self._send_json(
                500,
                {
                    "error": {
                        "code": "E_INTERNAL",
                        "message": f"{type(exc).__name__}: {exc}",
                    }
                },
            )

    def do_POST(self):
        path = urlparse(self.path).path.rstrip("/")
        try:
            if path == "/admin/reload":
                result = self.service.reload_config()
                self._send_json(200, result)
                return
            self._send_error(404, "E_NOT_FOUND", "not found")
        except OobError as exc:
            self._send_json(exc.http_status, {"error": exc.as_dict()})
        except Exception as exc:
            self._send_json(
                500,
                {
                    "error": {
                        "code": "E_INTERNAL",
                        "message": f"{type(exc).__name__}: {exc}",
                    }
                },
            )


class MonitorServer:
    def __init__(self, host: str, port: int, service: InspectorService):
        try:
            self.server = ThreadingHTTPServer((host, port), MonitorHandler)
        except OSError as exc:
            raise PortInUseError(
                f"cannot bind monitor HTTP {host}:{port}",
                details={"transport": "tcp", "host": host, "port": port},
            ) from exc
        self.server.daemon_threads = True
        self.server.service = service  # type: ignore[attr-defined]
        self.thread = threading.Thread(
            target=self.server.serve_forever, name="status-monitor", daemon=True
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
