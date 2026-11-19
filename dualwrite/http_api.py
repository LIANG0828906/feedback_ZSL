from __future__ import annotations

import json
import logging
import re
import threading
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from .audit import AuditStore
from .config import Config
from .errors import ConnectionLimitError
from .observability import Observability
from .sync import SyncEngine
from .trace import TraceService, normalize_row_type
from .validation import ValidationManager

LOGGER = logging.getLogger(__name__)


def stable_json(value) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        ensure_ascii=False,
        separators=(",", ":"),
        default=str,
    ).encode("utf-8")


class ApiContext:
    def __init__(
        self,
        config: Config,
        audit: AuditStore,
        sync_engine: SyncEngine,
        validation: ValidationManager,
        observability: Observability,
        trace: TraceService,
    ):
        self.config = config
        self.audit = audit
        self.sync_engine = sync_engine
        self.validation = validation
        self.observability = observability
        self.trace = trace


class ApiHandler(BaseHTTPRequestHandler):
    server_version = "DualWrite/1.0"
    context: ApiContext

    def log_message(self, format: str, *args) -> None:
        LOGGER.debug("%s - %s", self.address_string(), format % args)

    def _send(
        self,
        status: int,
        payload,
        content_type: str = "application/json; charset=utf-8",
    ) -> None:
        body = payload if isinstance(payload, bytes) else stable_json(payload)
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status: int, code: str, message: str) -> None:
        self._send(
            status,
            {
                "code": code,
                "message": message,
                "ok": False,
            },
        )

    def do_GET(self) -> None:
        try:
            self._route_get()
        except ConnectionLimitError as exc:
            self._error(HTTPStatus.SERVICE_UNAVAILABLE, "E_CONN", str(exc))
        except ValueError as exc:
            self._error(HTTPStatus.BAD_REQUEST, "E_INPUT", str(exc))
        except FileNotFoundError as exc:
            self._error(HTTPStatus.NOT_FOUND, "E_NOT_FOUND", str(exc))
        except Exception as exc:
            LOGGER.exception("GET %s failed", self.path)
            self._error(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                "E_INTERNAL",
                f"{type(exc).__name__}: {exc}",
            )

    def do_POST(self) -> None:
        try:
            self._route_post()
        except ConnectionLimitError as exc:
            self._error(HTTPStatus.SERVICE_UNAVAILABLE, "E_CONN", str(exc))
        except ValueError as exc:
            self._error(HTTPStatus.BAD_REQUEST, "E_INPUT", str(exc))
        except Exception as exc:
            LOGGER.exception("POST %s failed", self.path)
            self._error(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                "E_INTERNAL",
                f"{type(exc).__name__}: {exc}",
            )

    def _route_get(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        ctx = self.context

        if path in ("/healthz", "/health"):
            self._send(HTTPStatus.OK, {"ok": True, "service": "dualwrite"})
            return
        if path == "/metrics":
            self._send(
                HTTPStatus.OK,
                ctx.observability.metrics_text().encode("utf-8"),
                "text/plain; version=0.0.4; charset=utf-8",
            )
            return
        if path == "/v1/status":
            self._send(HTTPStatus.OK, ctx.observability.status_payload())
            return

        match = re.fullmatch(r"/v1/trace/(member|members|order|orders)/(\d+)", path)
        if match:
            limit = int(query.get("limit", ["30"])[0])
            self._send(
                HTTPStatus.OK,
                ctx.trace.trace(match.group(1), int(match.group(2)), limit),
            )
            return

        if path == "/v1/trace":
            row_type = query.get("row_type", [query.get("type", [""])[0]])[0]
            if not row_type or "row_id" not in query:
                raise ValueError("row_type and row_id are required")
            limit = int(query.get("limit", ["30"])[0])
            self._send(
                HTTPStatus.OK,
                ctx.trace.trace(row_type, int(query["row_id"][0]), limit),
            )
            return

        if path == "/v1/conflicts":
            table = query.get("table", [query.get("row_type", [None])[0]])[0]
            if table:
                table = normalize_row_type(table)
            row_id = query.get("row_id", [None])[0]
            rows = ctx.audit.query_conflicts(
                table_name=table,
                row_id=int(row_id) if row_id is not None else None,
                from_time=query.get("from", [None])[0],
                to_time=query.get("to", [None])[0],
                limit=int(query.get("limit", ["100"])[0]),
            )
            self._send(HTTPStatus.OK, {"conflicts": rows, "ok": True})
            return

        if path in ("/v1/validations/latest", "/v1/validation/latest"):
            latest = ctx.audit.latest_validation()
            if latest is None:
                self._send(HTTPStatus.OK, {"batch": None, "mismatches": [], "ok": True})
                return
            limit = int(query.get("limit", ["500"])[0])
            self._send(
                HTTPStatus.OK,
                {
                    "batch": latest,
                    "mismatches": ctx.audit.validation_mismatches(
                        latest["batch_id"], limit, query.get("category", [None])[0]
                    ),
                    "ok": True,
                },
            )
            return

        match = re.fullmatch(r"/v1/validations?/([0-9a-f]+)", path)
        if match:
            batch_id = match.group(1)
            batch = ctx.audit.validation_batch(batch_id)
            if batch is None:
                raise FileNotFoundError(f"validation batch {batch_id} not found")
            limit = int(query.get("limit", ["500"])[0])
            self._send(
                HTTPStatus.OK,
                {
                    "batch": batch,
                    "mismatches": ctx.audit.validation_mismatches(
                        batch_id, limit, query.get("category", [None])[0]
                    ),
                    "ok": True,
                },
            )
            return

        raise FileNotFoundError(self.path)

    def _route_post(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        length = min(1024 * 1024, int(self.headers.get("Content-Length", "0") or 0))
        body: dict = {}
        if length:
            raw = self.rfile.read(length)
            try:
                body = json.loads(raw.decode("utf-8")) if raw else {}
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise ValueError(f"invalid JSON body: {exc}") from exc
        if path in ("/v1/validate", "/v1/validations"):
            scope = str(body.get("scope", "all")).lower()
            batch_id = self.context.validation.start(scope)
            self._send(
                HTTPStatus.ACCEPTED,
                {
                    "batch_id": batch_id,
                    "ok": True,
                    "status": "running",
                },
            )
            return
        raise FileNotFoundError(self.path)


class LocalHttpServer:
    def __init__(self, config: Config, context: ApiContext):
        handler = type("BoundApiHandler", (ApiHandler,), {"context": context})
        self.httpd = ThreadingHTTPServer(
            (config.listen_host, config.listen_port), handler
        )
        self.httpd.daemon_threads = True
        self.httpd.request_queue_size = 64
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        self._thread = threading.Thread(
            target=self.httpd.serve_forever,
            name="dualwrite-http",
            daemon=True,
        )
        self._thread.start()

    def serve_forever(self) -> None:
        self.httpd.serve_forever()

    def stop(self) -> None:
        self.httpd.shutdown()
        self.httpd.server_close()
        if self._thread:
            self._thread.join(timeout=3)
