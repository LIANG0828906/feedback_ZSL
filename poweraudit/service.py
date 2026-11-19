"""HTTP service exposing audits, plans, status, and Prometheus metrics."""

from __future__ import annotations

import json
import logging
import re
import threading
import uuid
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse

from .audit import Auditor
from .collector import Collector
from .config import AppConfig
from .constants import ErrorCode
from .database import (
    Database,
    DuplicateAuditBatch,
    DuplicatePlan,
    RecordNotFound,
)
from .inventory import Inventory
from .metrics import render_metrics
from .planner import PlanError, Planner

LOG = logging.getLogger(__name__)
BATCH_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message


def _json_bytes(payload: Any) -> bytes:
    return json.dumps(
        payload,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _utc_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds")


class PowerAuditService:
    def __init__(
        self,
        config: AppConfig,
        inventory: Inventory,
        database: Database | None = None,
    ) -> None:
        self.config = config
        self.inventory = inventory
        self.database = database or Database(config.database_path)
        self.collector = Collector(config, inventory)
        self.auditor = Auditor(config, inventory)
        self.planner = Planner(config)
        self._audit_lock = threading.Lock()

    def run_audit(self, batch_id: str | None = None) -> dict[str, Any]:
        if batch_id is None:
            batch_id = (
                "AUD-"
                + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
                + "-"
                + uuid.uuid4().hex[:8]
            )
        if not isinstance(batch_id, str) or not BATCH_RE.fullmatch(batch_id):
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "batch_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,127}",
            )
        if self.database.audit_exists(batch_id):
            raise ApiError(
                HTTPStatus.CONFLICT,
                ErrorCode.AUDIT_BATCH_EXISTS,
                f"audit batch already exists: {batch_id}",
            )
        with self._audit_lock:
            if self.database.audit_exists(batch_id):
                raise ApiError(
                    HTTPStatus.CONFLICT,
                    ErrorCode.AUDIT_BATCH_EXISTS,
                    f"audit batch already exists: {batch_id}",
                )
            collection = self.collector.collect()
            audit = self.auditor.build(batch_id, collection)
            try:
                self.database.save_audit(audit)
            except DuplicateAuditBatch as exc:
                raise ApiError(
                    HTTPStatus.CONFLICT,
                    ErrorCode.AUDIT_BATCH_EXISTS,
                    str(exc),
                ) from exc
        return audit

    def create_maintenance_plan(self, body: dict[str, Any]) -> dict[str, Any]:
        batch_id = _required_str(body, "batch_id")
        target_type = _required_str(body, "target_type").lower()
        target_id = _required_str(body, "target_id")
        acceptable = body.get("acceptable_server_ids", [])
        if not isinstance(acceptable, list) or not all(
            isinstance(item, str) and item for item in acceptable
        ):
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "acceptable_server_ids must be an array of strings",
            )
        try:
            audit = self.database.get_audit(batch_id)
        except RecordNotFound as exc:
            raise ApiError(
                HTTPStatus.NOT_FOUND,
                ErrorCode.AUDIT_NOT_FOUND,
                f"audit batch not found: {batch_id}",
            ) from exc
        try:
            plan = self.planner.maintenance_plan(
                audit,
                target_type,
                target_id,
                acceptable_server_ids=acceptable,
            )
        except PlanError as exc:
            status = (
                HTTPStatus.NOT_FOUND
                if exc.code == "UNKNOWN_DEVICE"
                else HTTPStatus.BAD_REQUEST
            )
            raise ApiError(status, exc.code, exc.message) from exc
        try:
            self.database.save_plan(plan)
        except DuplicatePlan as exc:
            raise ApiError(
                HTTPStatus.CONFLICT,
                exc.code,
                str(exc),
            ) from exc
        return plan

    def create_emergency_plan(self, body: dict[str, Any]) -> dict[str, Any]:
        batch_id = _required_str(body, "batch_id")
        ups_ids = body.get("ups_ids")
        if ups_ids is not None and (
            not isinstance(ups_ids, list)
            or not all(isinstance(item, str) and item for item in ups_ids)
        ):
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "ups_ids must be an array of strings",
            )
        try:
            audit = self.database.get_audit(batch_id)
        except RecordNotFound as exc:
            raise ApiError(
                HTTPStatus.NOT_FOUND,
                ErrorCode.AUDIT_NOT_FOUND,
                f"audit batch not found: {batch_id}",
            ) from exc
        try:
            plan = self.planner.emergency_plans(audit, ups_ids=ups_ids)
        except PlanError as exc:
            status = (
                HTTPStatus.NOT_FOUND
                if exc.code == "UNKNOWN_DEVICE"
                else HTTPStatus.UNPROCESSABLE_ENTITY
            )
            raise ApiError(status, exc.code, exc.message) from exc
        try:
            self.database.save_plan(plan)
        except DuplicatePlan as exc:
            raise ApiError(
                HTTPStatus.CONFLICT,
                exc.code,
                str(exc),
            ) from exc
        return plan

    def status(self, batch_id: str | None = None) -> dict[str, Any]:
        try:
            audit = (
                self.database.get_audit(batch_id)
                if batch_id is not None
                else self.database.latest_audit()
            )
        except RecordNotFound as exc:
            raise ApiError(
                HTTPStatus.NOT_FOUND,
                ErrorCode.AUDIT_NOT_FOUND,
                f"audit batch not found: {batch_id}",
            ) from exc
        if audit is None:
            raise ApiError(
                HTTPStatus.NOT_FOUND,
                ErrorCode.AUDIT_NOT_FOUND,
                "no audit batch has been committed",
            )
        plan = self.database.latest_plan_for_batch(audit["batch_id"])
        return {
            "generated_at": _utc_iso(),
            "batch_id": audit["batch_id"],
            "audit_completed_at": audit["completed_at"],
            "totals": audit["totals"],
            "topology_unknown_devices": sorted(
                audit["unknown_devices"], key=lambda item: item["device_id"]
            ),
            "highest_risk_devices": audit["risk_ranking"][:10],
            "latest_plan_summary": plan,
        }

    def metrics(self) -> str:
        audit = self.database.latest_audit()
        return render_metrics(
            audit,
            infeasible_plan_count=self.database.infeasible_plan_count(),
            plan_count_value=self.database.plan_count(),
        )


def _required_str(body: dict[str, Any], key: str) -> str:
    value = body.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ApiError(
            HTTPStatus.BAD_REQUEST,
            ErrorCode.INVALID_REQUEST,
            f"{key} must be a non-empty string",
        )
    return value.strip()


class ApiHandler(BaseHTTPRequestHandler):
    server_version = "PowerAudit/1.0"
    protocol_version = "HTTP/1.1"
    service: PowerAuditService

    def log_message(self, fmt: str, *args: Any) -> None:
        LOG.info("%s - %s", self.address_string(), fmt % args)

    def _send(self, status: int, body: bytes, content_type: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _json(self, status: int, payload: Any) -> None:
        self._send(
            status,
            _json_bytes(payload),
            "application/json; charset=utf-8",
        )

    def _error(self, status: int, code: str, message: str) -> None:
        self._json(status, {"error": {"code": code, "message": message}})

    def _body(self) -> dict[str, Any]:
        length_header = self.headers.get("Content-Length")
        if length_header is None:
            return {}
        try:
            length = int(length_header)
        except ValueError as exc:
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "invalid Content-Length",
            ) from exc
        if length < 0 or length > 1_048_576:
            raise ApiError(
                HTTPStatus.REQUEST_ENTITY_TOO_LARGE,
                ErrorCode.INVALID_REQUEST,
                "request body too large",
            )
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw.decode("utf-8")) if raw else {}
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "request body must be valid UTF-8 JSON",
            ) from exc
        if not isinstance(body, dict):
            raise ApiError(
                HTTPStatus.BAD_REQUEST,
                ErrorCode.INVALID_REQUEST,
                "request body must be a JSON object",
            )
        return body

    def _segments(self) -> list[str]:
        path = urlparse(self.path).path
        return [unquote(segment) for segment in path.strip("/").split("/") if segment]

    def do_GET(self) -> None:
        try:
            self._do_get()
        except ApiError as exc:
            self._error(exc.status, exc.code, exc.message)
        except Exception:
            LOG.exception("unhandled GET error")
            self._error(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                ErrorCode.INTERNAL_ERROR,
                "internal server error",
            )

    def _do_get(self) -> None:
        parsed = urlparse(self.path)
        segments = self._segments()
        query = parse_qs(parsed.query)
        if parsed.path == "/healthz":
            self._json(HTTPStatus.OK, {"status": "ok"})
            return
        if parsed.path == "/metrics":
            body = self.service.metrics().encode("utf-8")
            self._send(
                HTTPStatus.OK,
                body,
                "text/plain; version=0.0.4; charset=utf-8",
            )
            return
        if segments == ["api", "v1", "status"]:
            batch_id = query.get("batch_id", [None])[0]
            self._json(HTTPStatus.OK, self.service.status(batch_id))
            return
        if segments == ["api", "v1", "audits"]:
            limit = _limit(query)
            self._json(
                HTTPStatus.OK,
                {"audits": self.service.database.list_audits(limit)},
            )
            return
        if len(segments) >= 4 and segments[:3] == ["api", "v1", "audits"]:
            batch_id = segments[3]
            try:
                audit = self.service.database.get_audit(batch_id)
            except RecordNotFound as exc:
                raise ApiError(
                    HTTPStatus.NOT_FOUND,
                    ErrorCode.AUDIT_NOT_FOUND,
                    f"audit batch not found: {batch_id}",
                ) from exc
            if len(segments) == 4:
                self._json(HTTPStatus.OK, audit)
                return
            if segments[4] == "exposure":
                self._json(
                    HTTPStatus.OK,
                    {
                        "batch_id": batch_id,
                        "risk_ranking": audit["risk_ranking"],
                        "unknown_devices": audit["unknown_devices"],
                    },
                )
                return
            if segments[4] == "servers" and len(segments) == 6:
                server_id = segments[5]
                for server in audit["servers"]:
                    if server["server_id"] == server_id:
                        self._json(HTTPStatus.OK, server)
                        return
                raise ApiError(
                    HTTPStatus.NOT_FOUND,
                    ErrorCode.NOT_FOUND,
                    f"server not found in batch: {server_id}",
                )
        if segments == ["api", "v1", "plans"]:
            batch_id = query.get("batch_id", [None])[0]
            plan_type = query.get("plan_type", [None])[0]
            self._json(
                HTTPStatus.OK,
                {
                    "plans": self.service.database.list_plans(
                        batch_id=batch_id,
                        plan_type=plan_type,
                        limit=_limit(query),
                    )
                },
            )
            return
        if len(segments) >= 4 and segments[:3] == ["api", "v1", "plans"]:
            plan_id = segments[3]
            try:
                plan = self.service.database.get_plan(plan_id)
            except RecordNotFound as exc:
                raise ApiError(
                    HTTPStatus.NOT_FOUND,
                    ErrorCode.PLAN_NOT_FOUND,
                    f"plan not found: {plan_id}",
                ) from exc
            if len(segments) == 5 and segments[4] == "servers":
                raise ApiError(
                    HTTPStatus.BAD_REQUEST,
                    ErrorCode.INVALID_REQUEST,
                    "server_id is required",
                )
            if len(segments) == 6 and segments[4] == "servers":
                server_id = segments[5]
                position = Planner.server_position(plan, server_id)
                if position is not None:
                    self._json(HTTPStatus.OK, position)
                    return
                if plan["plan_type"] == "maintenance":
                    for item in plan["servers"]:
                        if item["server_id"] == server_id:
                            self._json(
                                HTTPStatus.OK,
                                {
                                    "plan_id": plan["plan_id"],
                                    "batch_id": plan["batch_id"],
                                    "server_id": server_id,
                                    "position": plan["servers"].index(item) + 1,
                                    "total": len(plan["servers"]),
                                    **item,
                                },
                            )
                            return
                raise ApiError(
                    HTTPStatus.NOT_FOUND,
                    ErrorCode.NOT_FOUND,
                    f"server not present in plan: {server_id}",
                )
            self._json(HTTPStatus.OK, plan)
            return
        if segments == ["api", "v1", "config"]:
            self._json(HTTPStatus.OK, self.service.config.public_dict())
            return
        raise ApiError(HTTPStatus.NOT_FOUND, ErrorCode.NOT_FOUND, "not found")

    def do_POST(self) -> None:
        try:
            self._do_post()
        except ApiError as exc:
            self._error(exc.status, exc.code, exc.message)
        except Exception:
            LOG.exception("unhandled POST error")
            self._error(
                HTTPStatus.INTERNAL_SERVER_ERROR,
                ErrorCode.INTERNAL_ERROR,
                "internal server error",
            )

    def _do_post(self) -> None:
        segments = self._segments()
        if segments == ["api", "v1", "audits"]:
            body = self._body()
            batch_id = body.get("batch_id")
            if batch_id is not None and not isinstance(batch_id, str):
                raise ApiError(
                    HTTPStatus.BAD_REQUEST,
                    ErrorCode.INVALID_REQUEST,
                    "batch_id must be a string",
                )
            audit = self.service.run_audit(batch_id)
            self._json(HTTPStatus.CREATED, audit)
            return
        if segments == ["api", "v1", "plans"]:
            plan = self.service.create_maintenance_plan(self._body())
            self._json(HTTPStatus.CREATED, plan)
            return
        if segments == ["api", "v1", "emergency-plans"]:
            plan = self.service.create_emergency_plan(self._body())
            self._json(HTTPStatus.CREATED, plan)
            return
        raise ApiError(
            HTTPStatus.NOT_FOUND,
            ErrorCode.NOT_FOUND,
            "not found",
        )


def _limit(query: dict[str, list[str]]) -> int:
    raw = query.get("limit", ["50"])[0]
    try:
        return max(1, min(int(raw), 500))
    except (TypeError, ValueError):
        return 50


class PowerAuditHttpServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def create_http_server(
    service: PowerAuditService,
    host: str | None = None,
    port: int | None = None,
) -> PowerAuditHttpServer:
    handler = type(
        "BoundApiHandler",
        (ApiHandler,),
        {"service": service},
    )
    return PowerAuditHttpServer(
        (host if host is not None else service.config.http_host,
         port if port is not None else service.config.http_port),
        handler,
    )
