"""Standard-library HTTP API."""

from __future__ import annotations

import json
import sys
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from .errors import HandoverError
from .service import HandoverService
from .store import stable_json


MAX_BODY_BYTES = 2 * 1024 * 1024


class HandoverHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        handler_class: type[BaseHTTPRequestHandler],
        *,
        service: HandoverService,
    ) -> None:
        self.service = service
        super().__init__(server_address, handler_class)


class RequestHandler(BaseHTTPRequestHandler):
    server: HandoverHTTPServer
    protocol_version = "HTTP/1.1"

    def log_message(self, format: str, *args: Any) -> None:
        sys.stderr.write(
            "%s - - [%s] %s\n"
            % (self.address_string(), self.log_date_time_string(), format % args)
        )

    def _send(self, status: int, body: bytes, content_type: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            return

    def _json(self, status: int, payload: Any) -> None:
        body = stable_json(payload).encode("utf-8")
        self._send(status, body, "application/json; charset=utf-8")

    def _error(self, error: HandoverError) -> None:
        self._json(error.http_status, {"error": error.to_dict()})

    def _read_json_body(self) -> dict[str, Any] | None:
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            return None
        try:
            length = int(raw_length)
        except ValueError as exc:
            raise HandoverError("INVALID_JSON", "Invalid Content-Length header.") from exc
        if length < 0 or length > MAX_BODY_BYTES:
            raise HandoverError(
                "REQUEST_TOO_LARGE",
                "The request body exceeds the 2 MiB limit.",
                http_status=413,
            )
        if length == 0:
            return None
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise HandoverError(
                "INVALID_JSON", "The request body is not valid JSON."
            ) from exc
        if not isinstance(payload, dict):
            raise HandoverError("INVALID_JSON", "The request body must be an object.")
        return payload

    @staticmethod
    def _handover_payload(payload: dict[str, Any] | None) -> dict[str, Any] | None:
        if payload is None:
            return None
        nested = payload.get("handover")
        if isinstance(nested, dict):
            return nested
        return payload

    def do_GET(self) -> None:  # noqa: N802
        try:
            parsed = urlsplit(self.path)
            path = parsed.path.rstrip("/") or "/"
            query = parse_qs(parsed.query)
            if path == "/":
                self._json(
                    200,
                    {
                        "service": "duty-handover-decision",
                        "version": "1.0.0",
                        "endpoints": [
                            "GET /healthz",
                            "GET /metrics",
                            "GET /api/policy",
                            "GET /api/handovers",
                            "GET /api/handovers/{handover_id}",
                            "GET /api/handovers/{handover_id}/replay",
                            "GET /api/status?handover_id=...",
                            "GET /api/windows?service=...&start=...&end=...",
                            "POST /api/decision",
                            "POST /api/handovers",
                            "POST /api/handovers/{handover_id}/replay",
                        ],
                    },
                )
                return
            if path == "/healthz":
                self._json(200, self.server.service.health())
                return
            if path == "/metrics":
                body = self.server.service.metrics_text().encode("utf-8")
                self._send(200, body, "text/plain; version=0.0.4; charset=utf-8")
                return
            if path == "/api/policy":
                self._json(200, self.server.service.policy)
                return
            if path == "/api/handovers":
                limit_text = query.get("limit", ["100"])[0]
                try:
                    limit = int(limit_text)
                except ValueError as exc:
                    raise HandoverError(
                        "INVALID_QUERY", "limit must be an integer."
                    ) from exc
                self._json(200, {"items": self.server.service.history(limit)})
                return
            if path == "/api/status":
                handover_id = query.get("handover_id", [None])[0]
                self._json(200, self.server.service.status(handover_id))
                return
            if path == "/api/windows":
                service = query.get("service", [""])[0]
                start = query.get("start", [""])[0]
                end = query.get("end", [""])[0]
                if not service or not start or not end:
                    raise HandoverError(
                        "INVALID_QUERY",
                        "service, start, and end query parameters are required.",
                    )
                self._json(200, self.server.service.window(service, start, end))
                return
            prefix = "/api/handovers/"
            if path.startswith(prefix):
                remainder = unquote(path[len(prefix) :])
                parts = [part for part in remainder.split("/") if part]
                if len(parts) == 1:
                    self._json(200, self.server.service.get_snapshot(parts[0]))
                    return
                if len(parts) == 2 and parts[1] == "replay":
                    until = query.get("until", [None])[0]
                    step_text = query.get("step_minutes", ["1"])[0]
                    try:
                        step = int(step_text)
                    except ValueError as exc:
                        raise HandoverError(
                            "INVALID_QUERY", "step_minutes must be an integer."
                        ) from exc
                    self._json(
                        200,
                        self.server.service.replay(
                            parts[0], until=until, step_minutes=step
                        ),
                    )
                    return
            raise HandoverError(
                "NOT_FOUND", "The requested route does not exist.", http_status=404
            )
        except HandoverError as exc:
            self._error(exc)
        except Exception:
            traceback.print_exc(file=sys.stderr)
            self._error(
                HandoverError(
                    "INTERNAL_ERROR",
                    "An internal error occurred.",
                    http_status=500,
                )
            )

    def do_POST(self) -> None:  # noqa: N802
        try:
            parsed = urlsplit(self.path)
            path = parsed.path.rstrip("/") or "/"
            payload = self._read_json_body()
            if path == "/api/decision":
                self._json(
                    200,
                    self.server.service.decide(
                        self._handover_payload(payload)
                    ),
                )
                return
            if path == "/api/handovers":
                result = self.server.service.submit(
                    self._handover_payload(payload)
                )
                self._json(201, result)
                return
            if path == "/api/replay":
                if payload is None:
                    raise HandoverError(
                        "HANDOVER_ID_REQUIRED", "handover_id is required."
                    )
                handover_id = payload.get("handover_id")
                if not isinstance(handover_id, str) or not handover_id:
                    raise HandoverError(
                        "HANDOVER_ID_REQUIRED", "handover_id is required."
                    )
                until = payload.get("until")
                if until is not None and not isinstance(until, str):
                    raise HandoverError(
                        "INVALID_QUERY", "until must be an ISO-8601 string."
                    )
                step = payload.get("step_minutes", 1)
                if isinstance(step, bool) or not isinstance(step, int):
                    raise HandoverError(
                        "INVALID_QUERY", "step_minutes must be an integer."
                    )
                self._json(
                    200,
                    self.server.service.replay(
                        handover_id, until=until, step_minutes=step
                    ),
                )
                return
            prefix = "/api/handovers/"
            if path.startswith(prefix):
                remainder = unquote(path[len(prefix) :])
                parts = [part for part in remainder.split("/") if part]
                if len(parts) == 2 and parts[1] == "replay":
                    until = payload.get("until") if payload else None
                    step = payload.get("step_minutes", 1) if payload else 1
                    if until is not None and not isinstance(until, str):
                        raise HandoverError(
                            "INVALID_QUERY", "until must be an ISO-8601 string."
                        )
                    if isinstance(step, bool) or not isinstance(step, int):
                        raise HandoverError(
                            "INVALID_QUERY", "step_minutes must be an integer."
                        )
                    self._json(
                        200,
                        self.server.service.replay(
                            parts[0], until=until, step_minutes=step
                        ),
                    )
                    return
            raise HandoverError(
                "NOT_FOUND", "The requested route does not exist.", http_status=404
            )
        except HandoverError as exc:
            self._error(exc)
        except Exception:
            traceback.print_exc(file=sys.stderr)
            self._error(
                HandoverError(
                    "INTERNAL_ERROR",
                    "An internal error occurred.",
                    http_status=500,
                )
            )

    def do_OPTIONS(self) -> None:  # noqa: N802
        self.send_response(204)
        self.send_header("Allow", "GET, POST, OPTIONS")
        self.send_header("Content-Length", "0")
        self.end_headers()


def create_server(
    service: HandoverService, host: str = "127.0.0.1", port: int = 8765
) -> HandoverHTTPServer:
    return HandoverHTTPServer((host, port), RequestHandler, service=service)
