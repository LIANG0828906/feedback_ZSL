from __future__ import annotations

import hmac
import json
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from .errors import AppError, invalid_request
from .faults import ReplicationFaultController
from .service import OrderService


class Application:
    def __init__(
        self,
        service: OrderService,
        faults: ReplicationFaultController,
    ) -> None:
        self.service = service
        self.faults = faults


class ApiHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "ordersync/1"
    sys_version = ""
    application: Application

    def log_message(self, format: str, *args: Any) -> None:
        return

    def do_GET(self) -> None:
        self._dispatch("GET")

    def do_POST(self) -> None:
        self._dispatch("POST")

    def _dispatch(self, method: str) -> None:
        try:
            parsed = urlsplit(self.path)
            path = parsed.path.rstrip("/") or "/"
            query = parse_qs(parsed.query, keep_blank_values=True)
            if method == "GET":
                payload = self._get(path, query)
            else:
                payload = self._post(path)
            self._send_json(200, payload)
        except AppError as exc:
            self._send_json(exc.http_status, exc.payload())
        except (BrokenPipeError, ConnectionResetError):
            return
        except Exception:
            self._send_json(
                500,
                {
                    "error": {
                        "code": "E_INTERNAL",
                        "message": "Internal service error",
                    }
                },
            )

    def _get(
        self, path: str, query: dict[str, list[str]]
    ) -> dict[str, Any]:
        if path == "/health/live":
            return {"status": "alive"}
        if path in {"/health/topology", "/consistency/status"}:
            return self.application.service.status()
        if path == "/report":
            return self.application.service.metrics.snapshot()
        if path == "/orders":
            self._require_query(query, "tenant_id")
            self._require_query(query, "status")
            return self.application.service.list_orders(
                tenant_id=self._query_one(query, "tenant_id"),
                status=self._query_one(query, "status"),
                page=self._query_int(query, "page", 1),
                page_size=self._query_int(
                    query,
                    "page_size",
                    self.application.service.config.server.default_page_size,
                ),
                consistency=self._query_one(
                    query, "consistency", "strong"
                ),
                headers=self.headers,
            )
        if path.startswith("/orders/"):
            suffix = unquote(path[len("/orders/") :])
            if suffix.endswith("/compare"):
                order_no = suffix[: -len("/compare")].rstrip("/")
                if not order_no:
                    raise invalid_request("order number is required")
                return self.application.service.compare_order(order_no)
            if not suffix:
                raise invalid_request("order number is required")
            return self.application.service.read_order(
                suffix,
                self._query_one(query, "consistency", "strong"),
                self.headers,
            )
        raise AppError("E_NOT_FOUND", "Route not found", 404)

    def _post(self, path: str) -> dict[str, Any]:
        if path.startswith("/admin/"):
            self._require_admin()
        if path == "/orders":
            body = self._read_json()
            return self.application.service.write_order(body, self.headers)
        if path == "/admin/switch":
            return self.application.service.controlled_switch()
        if path == "/admin/faults/replication":
            body = self._read_json()
            action = str(body.get("action", "")).strip().lower()
            if action in {"disconnect", "pause"}:
                return self.application.faults.disconnect_replication()
            if action in {"reconnect", "resume"}:
                return self.application.faults.reconnect_replication()
            raise invalid_request(
                "action must be disconnect or reconnect"
            )
        if path == "/admin/report/reset":
            self.application.service.metrics.reset()
            return {"reset": True}
        raise AppError("E_NOT_FOUND", "Route not found", 404)

    def _require_admin(self) -> None:
        expected = self.application.service.config.server.admin_token
        if not expected:
            return
        provided = self.headers.get("X-Admin-Token", "")
        if not hmac.compare_digest(provided, expected):
            raise AppError("E_FORBIDDEN", "Admin token is invalid", 403)

    def _read_json(self) -> dict[str, Any]:
        content_type = self.headers.get("Content-Type", "")
        if "application/json" not in content_type.casefold():
            raise invalid_request("Content-Type must be application/json")
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            raise invalid_request("Content-Length is invalid") from None
        maximum = self.application.service.config.server.max_body_bytes
        if length < 1 or length > maximum:
            raise invalid_request("request body size is invalid")
        raw = self.rfile.read(length)
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            raise invalid_request("request body is not valid JSON") from None
        if not isinstance(parsed, dict):
            raise invalid_request("request body must be a JSON object")
        return parsed

    @staticmethod
    def _require_query(
        query: dict[str, list[str]], name: str
    ) -> None:
        if name not in query or not query[name] or not query[name][0].strip():
            raise invalid_request(f"query parameter {name} is required")

    @staticmethod
    def _query_one(
        query: dict[str, list[str]], name: str, default: str | None = None
    ) -> str:
        values = query.get(name)
        if not values:
            if default is not None:
                return default
            raise invalid_request(f"query parameter {name} is required")
        return values[0]

    def _query_int(
        self, query: dict[str, list[str]], name: str, default: int
    ) -> int:
        raw = self._query_one(query, name, str(default))
        try:
            return int(raw)
        except ValueError:
            raise invalid_request(f"{name} must be an integer") from None

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        raw = json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self._add_business_headers(payload)
        self.end_headers()
        self.wfile.write(raw)

    def _add_business_headers(self, payload: dict[str, Any]) -> None:
        commit = payload.get("commit_watermark")
        if commit is not None:
            self.send_header("X-Commit-Watermark", str(commit))
        consistency = payload.get("consistency")
        if not isinstance(consistency, dict):
            return
        replica_watermark = consistency.get("replica_applied_watermark")
        if replica_watermark is not None:
            self.send_header(
                "X-Replica-Applied-Watermark", str(replica_watermark)
            )
        served_by = consistency.get("served_by")
        if served_by is not None:
            self.send_header("X-Consistency-Served-By", str(served_by))
        degraded = consistency.get("degraded")
        if degraded is not None:
            self.send_header(
                "X-Consistency-Degraded", "true" if degraded else "false"
            )


class ServiceHttpServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 64

    def __init__(
        self,
        server_address: tuple[str, int],
        application: Application,
        max_workers: int = 16,
    ) -> None:
        handler = type(
            "BoundApiHandler", (ApiHandler,), {"application": application}
        )
        self._executor = ThreadPoolExecutor(
            max_workers=max_workers, thread_name_prefix="ordersync-http"
        )
        super().__init__(server_address, handler)

    def process_request(self, request: Any, client_address: Any) -> None:
        self._executor.submit(
            self.process_request_thread, request, client_address
        )

    def server_close(self) -> None:
        super().server_close()
        self._executor.shutdown(wait=True, cancel_futures=True)

