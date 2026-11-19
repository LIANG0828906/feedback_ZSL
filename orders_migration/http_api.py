from __future__ import annotations

import json
import logging
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlparse

from .config import Config
from .errors import AppError
from .service import MigrationService


LOG = logging.getLogger("orders_migration.http")


class ApiServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address: tuple[str, int], service: MigrationService,
                 config: Config):
        self.service = service
        self.config = config
        super().__init__(address, ApiHandler)


class ApiHandler(BaseHTTPRequestHandler):
    server: ApiServer
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        LOG.debug("%s - %s", self.address_string(), fmt % args)

    def do_GET(self) -> None:
        self._dispatch("GET")

    def do_POST(self) -> None:
        self._dispatch("POST")

    def _dispatch(self, method: str) -> None:
        try:
            status, body = self._route(method)
        except AppError as exc:
            self.server.service.record_error(exc.code)
            status, body = exc.http_status, exc.as_dict()
        except Exception as exc:
            LOG.exception("unhandled API failure")
            self.server.service.record_error("E_INTERNAL")
            status = 500
            body = {
                "ok": False,
                "errorCode": "E_INTERNAL",
                "message": "internal service error",
                "details": {"type": type(exc).__name__},
            }
        self._send_json(status, body)

    def _route(self, method: str) -> tuple[int, dict[str, Any]]:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        service = self.server.service

        if method == "GET" and path in {"/healthz", "/readyz"}:
            return 200, service.health()
        if method == "GET" and path == "/topology":
            return 200, service.topology_report()
        if method == "GET" and path == "/metrics":
            return 200, service.metrics()
        if method == "GET" and path == "/api/v1/tenants":
            return 200, {"ok": True, "tenants": service._all_tenants()}

        if method == "POST" and path == "/api/v1/orders":
            payload = self._read_json()
            idempotency = self.headers.get("X-Idempotency-Key")
            return 200, service.write_order(payload, idempotency)

        if method == "GET" and path.startswith("/api/v1/orders/"):
            order_id = _int_path(path, "/api/v1/orders/")
            tenant_value = query.get("tenantId", query.get("tenant_id", [None]))[0]
            if tenant_value is None:
                raise AppError(
                    "E_VALIDATION",
                    "tenantId query parameter is required",
                    {"orderId": order_id},
                    400,
                )
            return 200, service.get_order(order_id, int(tenant_value))

        if method == "POST" and path == "/api/v1/migrations":
            payload = self._read_json()
            tenant_id = _required_int(payload, "tenantId")
            max_batches = payload.get("maxBatches")
            return 200, service.migrate_tenant(
                tenant_id,
                int(max_batches) if max_batches is not None else None,
            )
        if method == "POST" and path.startswith("/api/v1/migrations/"):
            tenant_id = _int_path(path, "/api/v1/migrations/")
            payload = self._read_json(optional=True)
            max_batches = payload.get("maxBatches")
            return 200, service.migrate_tenant(
                tenant_id,
                int(max_batches) if max_batches is not None else None,
            )
        if method == "GET" and path.startswith("/api/v1/migrations/"):
            tenant_id = _int_path(path, "/api/v1/migrations/")
            return 200, service.migration_status(tenant_id)

        if method == "POST" and path == "/api/v1/checks":
            payload = self._read_json()
            return 200, service.check(
                order_id_start=int(payload.get("orderIdStart", 0)),
                order_id_end=int(payload.get("orderIdEnd", 9_223_372_036_854_775_807)),
                tenant_id=(
                    int(payload["tenantId"])
                    if payload.get("tenantId") is not None
                    else None
                ),
                force=bool(payload.get("force", False)),
            )
        if method == "POST" and path.startswith("/api/v1/checks/") and path.endswith("/resume"):
            run_id = path.split("/")[-2]
            return 200, service.check(run_id=run_id)
        if method == "GET" and path.startswith("/api/v1/checks/"):
            run_id = _path_tail(path, "/api/v1/checks/")
            return 200, service.check_status(run_id)

        if method == "POST" and path == "/api/v1/rollbacks":
            payload = self._read_json(optional=True)
            tenants = payload.get("tenants")
            if tenants is not None and not isinstance(tenants, list):
                raise AppError(
                    "E_VALIDATION", "tenants must be an array", http_status=400
                )
            return 200, service.rollback(tenants=tenants)
        if method == "POST" and path.startswith("/api/v1/rollbacks/") and path.endswith("/resume"):
            run_id = path.split("/")[-2]
            return 200, service.rollback(run_id=run_id)
        if method == "GET" and path.startswith("/api/v1/rollbacks/"):
            run_id = _path_tail(path, "/api/v1/rollbacks/")
            return 200, service.rollback_status(run_id)

        if method == "GET" and path.startswith("/api/v1/trace/"):
            order_id = _int_path(path, "/api/v1/trace/")
            return 200, {"ok": True, **service.trace(order_id)}

        if method == "POST" and path == "/admin/faults":
            if not self.server.config.fault_injection_enabled:
                raise AppError(
                    "E_FORBIDDEN",
                    "fault injection is disabled",
                    http_status=403,
                )
            expected = self.server.config.admin_token
            if expected and self.headers.get("X-Admin-Token") != expected:
                raise AppError(
                    "E_FORBIDDEN",
                    "invalid admin token",
                    http_status=403,
                )
            payload = self._read_json()
            return 200, {
                "ok": True,
                **service.set_fault(
                    str(payload["stage"]), int(payload.get("times", 1))
                ),
            }

        raise AppError(
            "E_NOTFOUND",
            "unknown route",
            {"method": method, "path": path},
            404,
        )

    def _read_json(self, optional: bool = False) -> dict[str, Any]:
        length_header = self.headers.get("Content-Length", "0")
        try:
            length = int(length_header)
        except ValueError as exc:
            raise AppError(
                "E_VALIDATION", "invalid Content-Length", http_status=400
            ) from exc
        if length > 2 * 1024 * 1024:
            raise AppError(
                "E_VALIDATION", "request body exceeds 2 MiB", http_status=413
            )
        if length == 0:
            if optional:
                return {}
            raise AppError(
                "E_VALIDATION", "JSON request body is required", http_status=400
            )
        raw = self.rfile.read(length)
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise AppError(
                "E_VALIDATION", "request body is not valid JSON", http_status=400
            ) from exc
        if not isinstance(value, dict):
            raise AppError(
                "E_VALIDATION", "request body must be a JSON object", http_status=400
            )
        return value

    def _send_json(self, status: int, body: dict[str, Any]) -> None:
        encoded = json.dumps(
            body, ensure_ascii=False, separators=(",", ":")
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        self.wfile.write(encoded)


def _int_path(path: str, prefix: str) -> int:
    tail = _path_tail(path, prefix)
    try:
        return int(tail)
    except ValueError as exc:
        raise AppError(
            "E_VALIDATION", "path identifier must be an integer", http_status=400
        ) from exc


def _path_tail(path: str, prefix: str) -> str:
    tail = path[len(prefix) :]
    if not tail or "/" in tail:
        raise AppError("E_NOTFOUND", "unknown route", http_status=404)
    return tail


def _required_int(payload: dict[str, Any], name: str) -> int:
    if name not in payload:
        raise AppError(
            "E_VALIDATION", f"{name} is required", http_status=400
        )
    try:
        return int(payload[name])
    except (TypeError, ValueError) as exc:
        raise AppError(
            "E_VALIDATION", f"{name} must be an integer", http_status=400
        ) from exc


def create_server(config: Config, service: MigrationService) -> ApiServer:
    return ApiServer((config.host, config.port), service, config)
