from __future__ import annotations

import json
import logging
import threading
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlparse

from .common import BusinessError, SCHEMA_VERSION
from .service import BackupChainService


LOGGER = logging.getLogger("backup-chain-http")
MAX_BODY = 1024 * 1024


def _json_bytes(value: Any) -> bytes:
    return (
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2)
        + "\n"
    ).encode("utf-8")


class BackupChainHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        service: BackupChainService,
    ) -> None:
        self.service = service
        self._service_lock = threading.Lock()
        super().__init__(server_address, BackupChainHandler)


class BackupChainHandler(BaseHTTPRequestHandler):
    server: BackupChainHTTPServer
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        LOGGER.info("%s - %s", self.address_string(), fmt % args)

    def _send(
        self,
        status: int,
        body: bytes,
        content_type: str,
        *,
        extra_headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if extra_headers:
            for key, value in extra_headers.items():
                self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _send_json(self, status: int, value: Any) -> None:
        self._send(
            status,
            _json_bytes(value),
            "application/json; charset=utf-8",
        )

    def _error(self, exc: BusinessError) -> None:
        code = exc.code
        if code.endswith("NOT_FOUND") or code == "BC_AUDIT_NOT_FOUND":
            status = HTTPStatus.NOT_FOUND
        elif code in {
            "BC_AUDIT_IN_PROGRESS",
            "BC_DRILL_IN_PROGRESS",
            "BC_OPERATION_IN_PROGRESS",
            "BC_RESTORE_POINT_UNAVAILABLE",
            "BC_DRILL_STAGING_MISSING",
            "BC_DRILL_OUTPUT_INCOMPLETE",
        }:
            status = HTTPStatus.CONFLICT
        else:
            status = HTTPStatus.BAD_REQUEST
        self._send_json(
            int(status),
            {
                "error": exc.as_dict(),
                "schema_version": SCHEMA_VERSION,
            },
        )

    def _read_body(self) -> dict[str, Any]:
        raw_length = self.headers.get("Content-Length", "0")
        try:
            length = int(raw_length)
        except ValueError as exc:
            raise BusinessError(
                "BC_REQUEST_INVALID", "Content-Length must be an integer"
            ) from exc
        if length < 0 or length > MAX_BODY:
            raise BusinessError(
                "BC_REQUEST_TOO_LARGE",
                f"request body must be at most {MAX_BODY} bytes",
            )
        if length == 0:
            return {}
        body = self.rfile.read(length)
        try:
            value = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BusinessError(
                "BC_REQUEST_INVALID", f"request body is not valid JSON: {exc}"
            ) from exc
        if not isinstance(value, dict):
            raise BusinessError(
                "BC_REQUEST_INVALID", "request body must be a JSON object"
            )
        return value

    def _query(self) -> tuple[str, dict[str, list[str]]]:
        parsed = urlparse(self.path)
        return parsed.path, parse_qs(parsed.query, keep_blank_values=True)

    def do_HEAD(self) -> None:
        self.do_GET()

    def do_GET(self) -> None:
        path, query = self._query()
        try:
            if path == "/healthz":
                self._send_json(
                    HTTPStatus.OK,
                    {
                        "service": "backup-chain-audit",
                        "status": "ok",
                    },
                )
                return
            if path == "/metrics":
                body = self.server.service.metrics_text().encode("utf-8")
                self._send(
                    HTTPStatus.OK,
                    body,
                    "text/plain; version=0.0.4; charset=utf-8",
                )
                return
            if path == "/api/v1/chains":
                chains = self.server.service.store.list_chains()
                self._send_json(
                    HTTPStatus.OK,
                    {
                        "chains": sorted(
                            chains, key=lambda item: item["chain_id"]
                        ),
                        "schema_version": SCHEMA_VERSION,
                    },
                )
                return
            if path == "/api/v1/audit":
                audit_id = query.get("audit_id", [None])[-1]
                chain_id = query.get("chain_id", [None])[-1]
                audit = self.server.service._require_audit(
                    chain_id=chain_id,
                    audit_id=int(audit_id) if audit_id not in (None, "") else None,
                )
                self._send_json(
                    HTTPStatus.OK,
                    {"audit": audit, "schema_version": SCHEMA_VERSION},
                )
                return
            if path == "/api/v1/status":
                raw_point = query.get("restore_point", [None])[-1]
                raw_audit = query.get("audit_id", [None])[-1]
                chain_id = query.get("chain_id", [None])[-1]
                status = self.server.service.status(
                    chain_id=chain_id,
                    restore_point=(
                        int(raw_point) if raw_point not in (None, "") else None
                    ),
                    audit_id=(
                        int(raw_audit) if raw_audit not in (None, "") else None
                    ),
                )
                self._send_json(HTTPStatus.OK, status)
                return
            if path == "/api/v1/drills":
                drill_id = query.get("drill_id", [None])[-1]
                if not drill_id:
                    raise BusinessError(
                        "BC_DRILL_NOT_FOUND", "drill_id query parameter is required"
                    )
                drill = self.server.service.store.get_drill(drill_id)
                if drill is None:
                    raise BusinessError(
                        "BC_DRILL_NOT_FOUND",
                        "drill does not exist",
                        drill_id=drill_id,
                    )
                self._send_json(
                    HTTPStatus.OK,
                    {
                        "drill": self.server.service._drill_public(drill),
                        "schema_version": SCHEMA_VERSION,
                    },
                )
                return
            self._send_json(
                HTTPStatus.NOT_FOUND,
                {
                    "error": {
                        "code": "BC_ROUTE_NOT_FOUND",
                        "context": {"path": path},
                        "message": "route not found",
                    },
                    "schema_version": SCHEMA_VERSION,
                },
            )
        except BusinessError as exc:
            self._error(exc)
        except (TypeError, ValueError) as exc:
            self._error(
                BusinessError(
                    "BC_QUERY_INVALID", f"invalid query parameter: {exc}"
                )
            )
        except Exception:
            LOGGER.exception("unexpected GET failure")
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {
                    "error": {
                        "code": "BC_INTERNAL_ERROR",
                        "context": {},
                        "message": "internal server error",
                    },
                    "schema_version": SCHEMA_VERSION,
                },
            )

    def do_POST(self) -> None:
        path, _query = self._query()
        try:
            if path == "/api/v1/audit":
                self._read_body()
                result = self.server.service.audit()
                self._send_json(HTTPStatus.OK, result)
                return
            if path == "/api/v1/drills":
                body = self._read_body()
                raw_point = body.get("restore_point")
                if isinstance(raw_point, bool) or not isinstance(raw_point, int):
                    raise BusinessError(
                        "BC_REQUEST_INVALID",
                        "restore_point must be an integer",
                    )
                raw_audit = body.get("audit_id")
                if raw_audit is not None and (
                    isinstance(raw_audit, bool) or not isinstance(raw_audit, int)
                ):
                    raise BusinessError(
                        "BC_REQUEST_INVALID", "audit_id must be an integer"
                    )
                result = self.server.service.run_drill(
                    restore_point=raw_point, audit_id=raw_audit
                )
                self._send_json(HTTPStatus.OK, result)
                return
            self._send_json(
                HTTPStatus.NOT_FOUND,
                {
                    "error": {
                        "code": "BC_ROUTE_NOT_FOUND",
                        "context": {"path": path},
                        "message": "route not found",
                    },
                    "schema_version": SCHEMA_VERSION,
                },
            )
        except BusinessError as exc:
            self._error(exc)
        except Exception:
            LOGGER.exception("unexpected POST failure")
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {
                    "error": {
                        "code": "BC_INTERNAL_ERROR",
                        "context": {},
                        "message": "internal server error",
                    },
                    "schema_version": SCHEMA_VERSION,
                },
            )


def serve(
    service: BackupChainService,
    *,
    host: str,
    port: int,
) -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    server = BackupChainHTTPServer((host, port), service)
    LOGGER.info("serving on http://%s:%s", host, port)
    try:
        server.serve_forever(poll_interval=0.25)
    except KeyboardInterrupt:
        LOGGER.info("HTTP server interrupted")
    finally:
        server.server_close()
