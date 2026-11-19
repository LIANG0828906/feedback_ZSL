from __future__ import annotations

import json
import logging
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from .database import Database, IngestError
from .http_client import get_json, post_json
from .metrics import render_metrics


LOGGER = logging.getLogger(__name__)
MAX_BODY_BYTES = 2 * 1024 * 1024
MAX_BATCH_EVENTS = 2000


class ApiHandler(BaseHTTPRequestHandler):
    server: "WebhookApiServer"
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        LOGGER.debug("%s - %s", self.address_string(), fmt % args)

    def _body(self) -> Any:
        length_header = self.headers.get("Content-Length")
        try:
            length = int(length_header or "0")
        except ValueError as exc:
            raise ValueError("invalid Content-Length") from exc
        if length <= 0 or length > MAX_BODY_BYTES:
            raise ValueError("request body is empty or too large")
        raw = self.rfile.read(length)
        try:
            return json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ValueError("request body is not valid JSON") from exc

    def _write(
        self,
        status: int,
        body: bytes,
        content_type: str,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _json(self, status: int, value: Any) -> None:
        body = json.dumps(
            value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
        ).encode("utf-8")
        self._write(status, body, "application/json; charset=utf-8")

    def _error(self, status: int, code: str, message: str, **extra: Any) -> None:
        payload: dict[str, Any] = {
            "error": {"business_code": code, "message": message}
        }
        payload.update(extra)
        self._json(status, payload)

    def do_GET(self) -> None:
        parsed = urlsplit(self.path)
        path = parsed.path.rstrip("/") or "/"
        if path == "/healthz":
            degraded = self.server.database.degraded_tenants()
            self._json(
                200,
                {
                    "status": "degraded" if degraded else "ok",
                    "degraded_tenants": [
                        item["tenant_id"] for item in degraded
                    ],
                },
            )
            return
        if path == "/readyz":
            try:
                healthy = self.server.database.health()
            except Exception:
                healthy = False
            self._json(200 if healthy else 503, {"ready": healthy})
            return
        if path == "/metrics":
            body = render_metrics(self.server.database).encode("utf-8")
            self._write(
                200, body, "text/plain; version=0.0.4; charset=utf-8"
            )
            return
        if path == "/v1/status":
            self._json(200, self.server.database.status())
            return
        if path == "/v1/report":
            self._json(200, self.server.database.report())
            return
        if path == "/v1/dead-letters":
            query = parse_qs(parsed.query)
            try:
                limit = min(1000, max(1, int(query.get("limit", ["100"])[0])))
            except ValueError:
                self._error(400, "API_QUERY_INVALID", "limit must be an integer")
                return
            self._json(
                200, {"dead_letters": self.server.database.dead_letters(limit)}
            )
            return
        if path.startswith("/v1/events/"):
            event_id = unquote(path[len("/v1/events/") :])
            event = self.server.database.event_status(event_id)
            if event is None:
                self._error(404, "EVENT_NOT_FOUND", f"event {event_id} not found")
            else:
                self._json(200, event)
            return
        if path.startswith("/v1/receiver-stats/"):
            tenant_id = unquote(path[len("/v1/receiver-stats/") :])
            self._receiver_proxy(tenant_id, "/stats", method="GET")
            return
        self._error(404, "API_NOT_FOUND", f"no route for {path}")

    def do_POST(self) -> None:
        parsed = urlsplit(self.path)
        path = parsed.path.rstrip("/") or "/"
        try:
            if path == "/v1/events":
                self._ingest_single()
                return
            if path == "/v1/events/batch":
                self._ingest_batch()
                return
            if path.startswith("/v1/tenants/") and path.endswith("/control"):
                tenant_id = unquote(
                    path[len("/v1/tenants/") : -len("/control")]
                )
                body = self._body()
                self._receiver_control(tenant_id, body)
                return
            if path.startswith("/v1/tenants/") and path.endswith("/recover"):
                tenant_id = unquote(
                    path[len("/v1/tenants/") : -len("/recover")]
                )
                recovered = self.server.database.recover_tenant(tenant_id)
                self._json(
                    200,
                    {
                        "tenant_id": tenant_id,
                        "recovered": recovered,
                    },
                )
                return
            self._error(404, "API_NOT_FOUND", f"no route for {path}")
        except ValueError as exc:
            self._error(400, "API_BODY_INVALID", str(exc))
        except IngestError as exc:
            cleanup = {
                key: value
                for key, value in {
                    "event_id": exc.event_id,
                }.items()
                if value is not None
            }
            self._error(
                409 if exc.code == "EVENT_ID_CONFLICT" else 422,
                exc.code,
                exc.message,
                **cleanup,
            )
        except Exception:
            LOGGER.exception("request failed")
            self._error(500, "API_INTERNAL_ERROR", "internal server error")

    def _validate_event(self, value: Any) -> tuple[str, str, Any]:
        if not isinstance(value, dict):
            raise ValueError("each event must be an object")
        event_id = value.get("event_id")
        tenant_id = value.get("tenant_id")
        if not isinstance(event_id, str) or not event_id or len(event_id) > 200:
            raise ValueError("event_id must be a non-empty string up to 200 chars")
        if not isinstance(tenant_id, str) or not tenant_id:
            raise ValueError("tenant_id must be a non-empty string")
        if "payload" not in value:
            raise ValueError("payload is required")
        return event_id, tenant_id, value["payload"]

    def _ingest_single(self) -> None:
        event = self._validate_event(self._body())
        result = self.server.database.ingest_events([event])
        self._json(
            200,
            {
                "inserted": result.inserted,
                "duplicates": result.duplicates,
            },
        )

    def _ingest_batch(self) -> None:
        body = self._body()
        if not isinstance(body, dict) or not isinstance(body.get("events"), list):
            raise ValueError("batch body must contain an events array")
        if not body["events"]:
            raise ValueError("events array must not be empty")
        if len(body["events"]) > MAX_BATCH_EVENTS:
            raise ValueError(
                f"batch cannot contain more than {MAX_BATCH_EVENTS} events"
            )
        records = [self._validate_event(item) for item in body["events"]]
        result = self.server.database.ingest_events(records)
        self._json(
            200,
            {
                "inserted": result.inserted,
                "duplicates": result.duplicates,
            },
        )

    def _receiver_control(self, tenant_id: str, body: Any) -> None:
        tenant = self.server.database.tenant_config(tenant_id)
        if tenant is None:
            self._error(
                404, "TENANT_NOT_FOUND", f"tenant {tenant_id} is not configured"
            )
            return
        status, response, error = post_json(
            f"{tenant['receiver_url'].rstrip('/')}/control",
            body,
            timeout_ms=3000,
        )
        if status is None:
            self._error(
                502,
                "RECEIVER_CONTROL_FAILED",
                f"receiver control failed: {error}",
            )
            return
        self._json(status, response if response is not None else {})

    def _receiver_proxy(self, tenant_id: str, suffix: str, method: str) -> None:
        tenant = self.server.database.tenant_config(tenant_id)
        if tenant is None:
            self._error(
                404, "TENANT_NOT_FOUND", f"tenant {tenant_id} is not configured"
            )
            return
        if method != "GET":
            self._error(405, "METHOD_NOT_ALLOWED", "unsupported proxy method")
            return
        status, response, error = get_json(
            f"{tenant['receiver_url'].rstrip('/')}{suffix}", timeout_ms=3000
        )
        if status is None:
            self._error(
                502, "RECEIVER_QUERY_FAILED", f"receiver query failed: {error}"
            )
            return
        self._json(status, response if response is not None else {})


class WebhookApiServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128

    def __init__(self, address: tuple[str, int], database: Database):
        super().__init__(address, ApiHandler)
        self.database = database

