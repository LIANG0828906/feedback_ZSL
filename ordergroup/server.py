from __future__ import annotations

import json
import re
import threading
import traceback
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from .coordinator import Coordinator
from .errors import ApiError, INVALID_REQUEST, api_error


_MEMBER_ROUTE = re.compile(
    r"^/v1/members/(?P<member>[^/]+)/(?P<action>heartbeat|leave|commit|pause|resume|fetch|ack-revoke)$"
)
_OFFSET_ROUTE = re.compile(r"^/v1/offsets/(?P<partition>-?\d+)$")
_PARTITION_ROUTE = re.compile(r"^/v1/partitions/(?P<partition>-?\d+)$")


class CoordinatorHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        coordinator: Coordinator,
        quiet: bool = False,
    ) -> None:
        self.coordinator = coordinator
        self.quiet = quiet
        super().__init__(server_address, CoordinatorRequestHandler)


class CoordinatorRequestHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "OrderGroupCoordinator/1.0"

    @property
    def coordinator(self) -> Coordinator:
        return self.server.coordinator  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        if not getattr(self.server, "quiet", False):
            super().log_message(fmt, *args)

    def do_GET(self) -> None:
        self._dispatch("GET")

    def do_POST(self) -> None:
        self._dispatch("POST")

    def do_HEAD(self) -> None:
        self._send_raw(405, b"", "text/plain; charset=utf-8", head=True)

    def _dispatch(self, method: str) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        request_id = self.headers.get("X-Request-ID") or uuid.uuid4().hex
        try:
            if method == "GET" and path in ("/healthz", "/v1/health"):
                self._json(200, self.coordinator.health(), request_id)
                return
            if method == "GET" and path == "/metrics":
                body = self.coordinator.metrics().encode("utf-8")
                self._send_raw(200, body, "text/plain; version=0.0.4; charset=utf-8", request_id)
                return
            if method == "GET" and path in ("/v1/status", "/status"):
                limit = int(query.get("event_limit", ["50"])[0])
                self._json(200, self.coordinator.status(limit), request_id)
                return
            if method == "GET" and path == "/v1/assignments":
                self._json(200, self.coordinator.assignments_status(), request_id)
                return
            if method == "GET" and path == "/v1/offsets":
                self._json(200, self.coordinator.offset_status(), request_id)
                return
            offset_match = _OFFSET_ROUTE.match(path)
            if method == "GET" and offset_match:
                partition = int(offset_match.group("partition"))
                self._json(
                    200, self.coordinator.offset_status(partition), request_id
                )
                return
            if method == "GET" and path == "/v1/partitions":
                self._json(200, self.coordinator.partition_status(), request_id)
                return
            partition_match = _PARTITION_ROUTE.match(path)
            if method == "GET" and partition_match:
                partition = int(partition_match.group("partition"))
                body = self.coordinator.partition_status()
                if partition < 0 or partition >= self.coordinator.partition_count:
                    raise api_error(
                        INVALID_REQUEST,
                        f"partition must be between 0 and {self.coordinator.partition_count - 1}",
                    )
                body["partitions"] = [
                    item for item in body["partitions"] if item["partition"] == partition
                ]
                self._json(200, body, request_id)
                return
            if method == "GET" and path == "/v1/lag":
                self._json(200, self.coordinator.verify_lag(), request_id)
                return
            if method == "GET" and path == "/v1/events":
                limit = int(query.get("limit", ["100"])[0])
                self._json(
                    200,
                    {
                        "ok": True,
                        "events": self.coordinator.store.recent_events(
                            max(1, min(10000, limit))
                        ),
                    },
                    request_id,
                )
                return
            if method == "POST" and path in ("/v1/lag/verify", "/v1/reconcile"):
                self._read_json(allow_empty=True)
                self._json(200, self.coordinator.verify_lag(), request_id)
                return
            if method == "POST" and path in ("/v1/members/join", "/v1/join"):
                body = self._read_json()
                self._json(200, self.coordinator.join(body), request_id)
                return
            member_match = _MEMBER_ROUTE.match(path)
            if method == "POST" and member_match:
                member_id = unquote(member_match.group("member"))
                action = member_match.group("action")
                body = self._read_json(allow_empty=True)
                if action == "heartbeat":
                    result = self.coordinator.heartbeat(member_id, body)
                elif action == "leave":
                    result = self.coordinator.leave(member_id)
                elif action == "commit":
                    result = self.coordinator.commit(member_id, body)
                elif action == "pause":
                    result = self.coordinator.pause(member_id, body)
                elif action == "resume":
                    result = self.coordinator.resume(member_id, body)
                elif action == "fetch":
                    result = self.coordinator.fetch(member_id, body)
                else:
                    result = self.coordinator.ack_revoke(member_id, body)
                self._json(200, result, request_id)
                return
            raise api_error(INVALID_REQUEST, f"route {method} {path} not found")
        except ApiError as exc:
            self._json(exc.http_status, exc.to_dict(), request_id)
        except (BrokenPipeError, ConnectionResetError):
            return
        except Exception:
            if not getattr(self.server, "quiet", False):
                traceback.print_exc()
            self._json(
                500,
                {
                    "ok": False,
                    "error": {
                        "code": "INTERNAL_ERROR",
                        "numeric_code": 50000,
                        "message": "internal server error",
                    },
                },
                request_id,
            )

    def _read_json(self, allow_empty: bool = False) -> dict[str, Any]:
        raw_length = self.headers.get("Content-Length", "0")
        try:
            length = int(raw_length)
        except ValueError:
            raise api_error(INVALID_REQUEST, "invalid Content-Length") from None
        if length > 2 * 1024 * 1024:
            raise api_error(INVALID_REQUEST, "request body exceeds 2 MiB")
        if length == 0:
            if allow_empty:
                return {}
            raise api_error(INVALID_REQUEST, "JSON request body is required")
        raw = self.rfile.read(length)
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise api_error(
                INVALID_REQUEST, f"invalid JSON request body: {exc}"
            ) from exc
        if not isinstance(value, dict):
            raise api_error(INVALID_REQUEST, "JSON request body must be an object")
        return value

    def _json(
        self, status: int, body: dict[str, Any], request_id: str | None = None
    ) -> None:
        encoded = json.dumps(
            body, ensure_ascii=False, separators=(",", ":"), sort_keys=True
        ).encode("utf-8")
        self._send_raw(status, encoded, "application/json; charset=utf-8", request_id)

    def _send_raw(
        self,
        status: int,
        body: bytes,
        content_type: str,
        request_id: str | None = None,
        head: bool = False,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if request_id:
            self.send_header("X-Request-ID", request_id)
        self.end_headers()
        if not head:
            self.wfile.write(body)
            self.wfile.flush()


def create_server(
    coordinator: Coordinator,
    host: str,
    port: int,
    quiet: bool = False,
) -> CoordinatorHTTPServer:
    return CoordinatorHTTPServer((host, port), coordinator, quiet=quiet)


def serve(
    coordinator: Coordinator,
    host: str,
    port: int,
    quiet: bool = False,
) -> CoordinatorHTTPServer:
    server = create_server(coordinator, host, port, quiet=quiet)
    thread = threading.Thread(
        target=server.serve_forever,
        name="coordinator-http",
        daemon=True,
    )
    thread.start()
    return server

