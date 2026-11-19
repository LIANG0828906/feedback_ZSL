"""Small dependency-free JSON/HTTP API and Prometheus endpoint."""

from __future__ import annotations

import json
import logging
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from .errors import BusinessError, PlanValidationError
from .models import parse_timestamp, stable_json
from .plan import load_plan, parse_plan
from .prometheus import render_metrics
from .service import Orchestrator


LOGGER = logging.getLogger(__name__)


class OrchestratorHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        orchestrator: Orchestrator,
    ) -> None:
        self.orchestrator = orchestrator
        super().__init__(server_address, OrchestratorRequestHandler)


class OrchestratorRequestHandler(BaseHTTPRequestHandler):
    server_version = "ReleaseOrchestrator/1.0"

    @property
    def orchestrator(self) -> Orchestrator:
        return self.server.orchestrator  # type: ignore[attr-defined]

    def log_message(self, format: str, *args: Any) -> None:
        LOGGER.info("%s - %s", self.address_string(), format % args)

    def _send_bytes(
        self, status: int, body: bytes, content_type: str
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, status: int, payload: Any) -> None:
        body = stable_json(payload).encode("utf-8")
        self._send_bytes(
            status, body, "application/json; charset=utf-8"
        )

    def _read_json(self) -> Any:
        content_length = int(self.headers.get("Content-Length", "0") or "0")
        if content_length > 2 * 1024 * 1024:
            raise BusinessError(
                "REQUEST_TOO_LARGE",
                "request body exceeds 2 MiB",
                status_code=413,
            )
        raw = self.rfile.read(content_length) if content_length else b"{}"
        try:
            return json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BusinessError(
                "REQUEST_JSON_INVALID",
                f"request body is not valid JSON: {exc}",
                status_code=400,
            ) from exc

    def _error(self, error: BusinessError) -> None:
        self._send_json(error.status_code, error.to_dict())

    def _send_release_result(
        self, status: dict[str, Any], success_code: int = 200
    ) -> None:
        if status.get("business_error"):
            payload = {
                "ok": False,
                "error": status["business_error"],
                "release": status,
            }
            self._send_json(HTTPStatus.CONFLICT, payload)
            return
        self._send_json(success_code, status)

    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)
        try:
            if path in {"/", "/health", "/healthz"}:
                self._send_json(
                    HTTPStatus.OK,
                    {
                        "ok": True,
                        "service": "release-orchestrator",
                        "status": "healthy",
                    },
                )
                return
            if path == "/metrics":
                self._send_bytes(
                    HTTPStatus.OK,
                    render_metrics(self.orchestrator),
                    "text/plain; version=0.0.4; charset=utf-8",
                )
                return
            if path == "/api/releases":
                self._send_json(
                    HTTPStatus.OK,
                    {"releases": self.orchestrator.list_releases()},
                )
                return
            if path == "/api/status":
                release_id = (query.get("release_id") or [""])[0]
                self._send_json(
                    HTTPStatus.OK,
                    self.orchestrator.get_status(release_id),
                )
                return
            parts = [unquote(part) for part in path.split("/") if part]
            if len(parts) >= 3 and parts[:2] == ["api", "releases"]:
                release_id = parts[2]
                if len(parts) == 3 or (
                    len(parts) == 4 and parts[3] == "status"
                ):
                    self._send_json(
                        HTTPStatus.OK,
                        self.orchestrator.get_status(release_id),
                    )
                    return
                if len(parts) == 4 and parts[3] == "audit":
                    self._send_json(
                        HTTPStatus.OK,
                        {
                            "release_id": release_id,
                            "records": self.orchestrator.get_audit(release_id),
                        },
                    )
                    return
                if len(parts) == 4 and parts[3] == "report":
                    self._send_json(
                        HTTPStatus.OK,
                        self.orchestrator.release_report(release_id),
                    )
                    return
                if len(parts) == 5 and parts[3] == "rollback":
                    if parts[4] in {"status", "check", "verify"}:
                        self._send_json(
                            HTTPStatus.OK,
                            self.orchestrator.get_status(release_id)["rollback"],
                        )
                        return
            self._send_json(
                HTTPStatus.NOT_FOUND,
                {
                    "ok": False,
                    "error": {
                        "code": "ROUTE_NOT_FOUND",
                        "message": f"no route for GET {path}",
                    },
                },
            )
        except BusinessError as error:
            self._error(error)
        except Exception as exc:  # pragma: no cover - defensive server guard
            LOGGER.exception("unhandled GET error")
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {
                    "ok": False,
                    "error": {
                        "code": "INTERNAL_ERROR",
                        "message": str(exc),
                    },
                },
            )

    def do_POST(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        try:
            body = self._read_json()
            if path == "/api/plans/validate":
                if isinstance(body, dict) and "plan" in body:
                    plan_value = body["plan"]
                elif isinstance(body, dict) and "plan_path" in body:
                    plan_value = body["plan_path"]
                else:
                    plan_value = body
                if isinstance(plan_value, dict):
                    plan = parse_plan(plan_value)
                elif isinstance(plan_value, str):
                    plan = load_plan(plan_value)
                else:
                    raise BusinessError(
                        "REQUEST_SCHEMA_INVALID",
                        "plan or plan_path is required",
                        status_code=400,
                    )
                self._send_json(
                    HTTPStatus.OK,
                    {
                        "ok": True,
                        "valid": True,
                        "plan": plan.to_dict(),
                    },
                )
                return
            if path == "/api/releases":
                if not isinstance(body, dict):
                    raise BusinessError(
                        "REQUEST_SCHEMA_INVALID",
                        "request body must be an object",
                        status_code=400,
                    )
                if body.get("sample_dir"):
                    status = self.orchestrator.start_from_sample_dir(
                        str(body["sample_dir"]),
                        metrics_path=body.get("metrics_path"),
                        version_reports_path=body.get("version_reports_path"),
                    )
                else:
                    plan_value = body.get("plan", body.get("plan_path"))
                    if (
                        plan_value is None
                        and isinstance(body, dict)
                        and "release_id" in body
                        and "batches" in body
                    ):
                        plan_value = body
                    if plan_value is None:
                        raise BusinessError(
                            "REQUEST_SCHEMA_INVALID",
                            "plan, plan_path, or sample_dir is required",
                            status_code=400,
                        )
                    status = self.orchestrator.start_release(
                        plan_value,
                        metrics_path=body.get("metrics_path"),
                        version_reports_path=body.get("version_reports_path"),
                        sample_dir=body.get("base_dir"),
                    )
                self._send_json(HTTPStatus.CREATED, status)
                return
            parts = [unquote(part) for part in path.split("/") if part]
            if len(parts) >= 4 and parts[:2] == ["api", "releases"]:
                release_id = parts[2]
                action = parts[3]
                if action in {"evaluate", "advance"}:
                    status = self.orchestrator.evaluate_next(release_id)
                    self._send_release_result(status)
                    return
                if action == "run":
                    max_batches = body.get("max_batches") if isinstance(body, dict) else None
                    status = self.orchestrator.run_release(
                        release_id,
                        max_batches=int(max_batches)
                        if max_batches is not None
                        else None,
                    )
                    self._send_release_result(status)
                    return
                if action == "rollback" and len(parts) >= 5 and parts[4] in {
                    "verify",
                    "check",
                }:
                    status = self.orchestrator.verify_rollback(release_id)
                    self._send_json(HTTPStatus.OK, status)
                    return
            self._send_json(
                HTTPStatus.NOT_FOUND,
                {
                    "ok": False,
                    "error": {
                        "code": "ROUTE_NOT_FOUND",
                        "message": f"no route for POST {path}",
                    },
                },
            )
        except PlanValidationError as error:
            self._error(error)
        except BusinessError as error:
            self._error(error)
        except Exception as exc:  # pragma: no cover - defensive server guard
            LOGGER.exception("unhandled POST error")
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                {
                    "ok": False,
                    "error": {
                        "code": "INTERNAL_ERROR",
                        "message": str(exc),
                    },
                },
            )


def create_server(
    host: str,
    port: int,
    orchestrator: Orchestrator,
) -> OrchestratorHTTPServer:
    return OrchestratorHTTPServer((host, port), orchestrator)


def serve(
    host: str,
    port: int,
    orchestrator: Orchestrator,
) -> None:
    server = create_server(host, port, orchestrator)
    LOGGER.info(
        "release orchestrator listening on http://%s:%s",
        server.server_address[0],
        server.server_address[1],
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
