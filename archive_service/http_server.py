from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import shutil
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from .errors import ApiError
from .service import ArchiveService, DOMAINS
from .util import remove_quietly, spool_stream


VERSION_ROUTE = re.compile(r"^/api/v1/versions/([^/]+)(?:/(metadata|admission))?$")
OBJECT_VERSION_ROUTE = re.compile(
    r"^/api/v1/objects/(?P<key>.+)/versions/(?P<version>[^/]+)"
    r"(?:/(?P<action>metadata|admission))?$"
)
ADMIN_ROUTE = re.compile(
    r"^/api/v1/admin/versions/([^/]+)/(retention|domain-difference)$"
)
JSON_CONTENT_TYPE = "application/json; charset=utf-8"
PROMETHEUS_CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8"


def _header_list(value: str | None) -> list[str]:
    if not value:
        return []
    return [item.strip() for item in value.split(",") if item.strip()]


def _query_one(query: dict[str, list[str]], name: str, default: str | None = None) -> str | None:
    values = query.get(name)
    return values[-1] if values else default


def _query_bool(query: dict[str, list[str]], name: str, default: bool = False) -> bool:
    value = _query_one(query, name)
    return ArchiveService.parse_bool(value, default=default) if value is not None else default


def _decode_json_object(raw: bytes) -> dict[str, Any]:
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ApiError(
            "INVALID_JSON",
            400,
            "request body is not valid JSON",
            {"detail": str(exc)},
        )
    if not isinstance(value, dict):
        raise ApiError("INVALID_JSON", 400, "JSON request body must be an object")
    return value


def _policy_fields(payload: dict[str, Any]) -> dict[str, Any]:
    nested = payload.get("retention_days") or payload.get("values") or {}
    if not isinstance(nested, dict):
        nested = {}
    regular = payload.get("regular_days", nested.get("regular"))
    audit = payload.get("audit_days", nested.get("audit"))
    legal = payload.get(
        "legal_days",
        payload.get("legal_hold_days", nested.get("legal", nested.get("legal_hold"))),
    )
    if regular is None or audit is None or legal is None:
        raise ApiError(
            "INVALID_RETENTION_POLICY",
            400,
            "regular_days, audit_days and legal_days are required",
            {
                "accepted_legal_aliases": ["legal_days", "legal_hold_days"],
                "accepted_nested_field": "retention_days",
            },
        )
    return {
        "regular_days": regular,
        "audit_days": audit,
        "legal_days": legal,
        "effective_at": payload.get("effective_at"),
        "as_of": payload.get("as_of"),
    }


class ArchiveRequestHandler(BaseHTTPRequestHandler):
    service: ArchiveService
    protocol_version = "HTTP/1.1"
    server_version = "CompliantArchive/1.0"
    sys_version = ""
    quiet = False

    # ------------------------------------------------------------------
    # HTTP plumbing
    # ------------------------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:
        if not self.quiet:
            super().log_message(fmt, *args)

    def _read_body(self, limit: int) -> bytes:
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            raise ApiError(
                "CONTENT_LENGTH_REQUIRED",
                411,
                "Content-Length is required",
            )
        try:
            length = int(raw_length)
        except ValueError:
            raise ApiError("INVALID_CONTENT_LENGTH", 400, "Content-Length must be an integer")
        if length < 0:
            raise ApiError("INVALID_CONTENT_LENGTH", 400, "Content-Length must be non-negative")
        if length > limit:
            raise ApiError(
                "REQUEST_BODY_TOO_LARGE",
                413,
                "request body exceeds the configured limit",
                {"limit": limit, "received": length},
            )
        return self.rfile.read(length)

    def _send_bytes(
        self,
        body: bytes,
        *,
        status: int = 200,
        content_type: str = JSON_CONTENT_TYPE,
        headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _send_json(self, payload: Any, *, status: int = 200) -> None:
        body = json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        self._send_bytes(body, status=status, content_type=JSON_CONTENT_TYPE)

    def _send_file(self, path: Path, metadata: dict[str, Any]) -> None:
        try:
            handle = open(path, "rb")
        except FileNotFoundError:
            raise ApiError(
                "OBJECT_CONTENT_UNAVAILABLE",
                502,
                "object content disappeared before it could be read",
                {"version_id": metadata.get("version_id")},
            )
        size = os.fstat(handle.fileno()).st_size
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(size))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Archive-Version-Id", str(metadata["version_id"]))
        self.send_header("X-Archive-Domain", str(metadata["domain"]))
        self.send_header(
            "X-Archive-Retention-Until", str(metadata["retention_until"])
        )
        self.send_header(
            "X-Archive-Content-Sha256", str(metadata["content_sha256"])
        )
        self.send_header(
            "X-Archive-Legal-Hold", "true" if metadata["legal_hold"] else "false"
        )
        self.end_headers()
        if self.command == "HEAD":
            handle.close()
            return
        try:
            with handle:
                shutil.copyfileobj(handle, self.wfile, length=1024 * 1024)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _error(self, error: ApiError) -> None:
        self._send_json(error.as_dict(), status=error.status)

    def _dispatch(self) -> None:
        try:
            self._route()
        except ApiError as exc:
            self._error(exc)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as exc:  # Keep the service alive and return a stable code.
            self.log_error("unhandled error: %s: %s", type(exc).__name__, exc)
            self._error(
                ApiError(
                    "INTERNAL_ERROR",
                    500,
                    "internal service error",
                    {"type": type(exc).__name__},
                )
            )

    def do_GET(self) -> None:  # noqa: N802
        self._dispatch()

    def do_HEAD(self) -> None:  # noqa: N802
        self._dispatch()

    def do_POST(self) -> None:  # noqa: N802
        self._dispatch()

    def do_PUT(self) -> None:  # noqa: N802
        self._dispatch()

    def do_DELETE(self) -> None:  # noqa: N802
        self._dispatch()

    def do_OPTIONS(self) -> None:  # noqa: N802
        self.send_response(204)
        self.send_header("Allow", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
        self.send_header("Content-Length", "0")
        self.end_headers()

    # ------------------------------------------------------------------
    # Routing
    # ------------------------------------------------------------------

    def _route(self) -> None:
        parsed = urlsplit(self.path)
        path = unquote(parsed.path)
        query = parse_qs(parsed.query, keep_blank_values=True)
        method = self.command

        if method == "GET" and path in {"/healthz", "/api/v1/health"}:
            self._send_json({"service": "compliant-archive", "status": "ok"})
            return
        if method == "GET" and path == "/metrics":
            text = self.service.metrics_text()
            self._send_bytes(
                text.encode("utf-8"),
                content_type=PROMETHEUS_CONTENT_TYPE,
            )
            return
        if method == "GET" and path == "/api/v1/status":
            self._send_json(self.service.status())
            return
        if path == "/api/v1/policies":
            if method == "GET":
                self._send_json(self.service.current_policy())
                return
            if method in {"POST", "PUT"}:
                payload = _decode_json_object(
                    self._read_body(max(self.service.max_object_bytes, 1024 * 1024))
                )
                fields = _policy_fields(payload)
                fields.pop("as_of", None)
                self._send_json(self.service.publish_policy(**fields), status=201)
                return
        if path in {"/api/v1/policy-impact", "/api/v1/policies/impact"} and method == "POST":
            payload = _decode_json_object(
                self._read_body(max(self.service.max_object_bytes, 1024 * 1024))
            )
            if "publication_id" in payload:
                try:
                    publication_id = int(payload["publication_id"])
                except (TypeError, ValueError):
                    raise ApiError(
                        "INVALID_POLICY_PUBLICATION_ID",
                        400,
                        "publication_id must be an integer",
                        {"received": payload["publication_id"]},
                    )
                self._send_json(
                    self.service.policy_impact_for_publication(
                        publication_id,
                        as_of=payload.get("as_of"),
                    )
                )
            else:
                fields = _policy_fields(payload)
                self._send_json(self.service.policy_impact(**fields))
            return
        if path in {"/api/v1/reconcile", "/api/v1/reconciliations"} and method == "POST":
            self._send_json(self.service.reconcile())
            return
        if path == "/api/v1/cleanup" and method == "POST":
            payload: dict[str, Any] = {}
            if self.headers.get("Content-Length") not in {None, "0"}:
                raw = self._read_body(1024 * 1024)
                if raw.strip():
                    payload = _decode_json_object(raw)
            failures = _header_list(self.headers.get("X-Simulate-Domain-Failure"))
            failures.extend(self._payload_domains(payload, query))
            dry_run = _query_bool(query, "dry_run", bool(payload.get("dry_run", False)))
            self._send_json(
                self.service.cleanup(fail_domains=failures, dry_run=dry_run)
            )
            return
        if path in {"/api/v1/objects", "/api/v1/object"} and method in {"POST", "PUT"}:
            self._handle_write(query)
            return
        if path == "/api/v1/objects/read" and method in {"GET", "HEAD"}:
            key = _query_one(query, "key", _query_one(query, "object_key"))
            version = _query_one(query, "version", _query_one(query, "version_id"))
            if not key or version is None:
                raise ApiError(
                    "OBJECT_KEY_AND_VERSION_REQUIRED",
                    400,
                    "key and version query parameters are required",
                )
            version_id = self.service.resolve_version_id(
                object_key=key, version=version
            )
            self._send_file(*self.service.read_version(version_id))
            return

        version_match = VERSION_ROUTE.match(path)
        if version_match:
            version_id = version_match.group(1)
            action = version_match.group(2)
            self._handle_version_route(method, version_id, action)
            return

        object_version_match = OBJECT_VERSION_ROUTE.match(path)
        if object_version_match:
            key = object_version_match.group("key")
            version_id = self.service.resolve_version_id(
                object_key=key, version=object_version_match.group("version")
            )
            self._handle_version_route(
                method, version_id, object_version_match.group("action")
            )
            return

        admin_match = ADMIN_ROUTE.match(path)
        if admin_match and method in {"POST", "PUT"}:
            version_id = admin_match.group(1)
            action = admin_match.group(2)
            payload = _decode_json_object(self._read_body(1024 * 1024))
            if action == "retention":
                result = self.service.override_retention(
                    version_id,
                    metadata_retention_until=payload.get("metadata_retention_until"),
                    lock_retention_until=payload.get("lock_retention_until"),
                    legal_hold=(
                        self.service.parse_bool(payload["legal_hold"])
                        if "legal_hold" in payload
                        else None
                    ),
                    lock_pinned=(
                        self.service.parse_bool(payload["lock_pinned"])
                        if "lock_pinned" in payload
                        else None
                    ),
                    written_at=payload.get("written_at"),
                )
            else:
                result = self.service.create_domain_difference(
                    version_id,
                    domain=str(payload.get("domain", "")),
                    mode=str(payload.get("mode", "")),
                )
            self._send_json(result)
            return

        raise ApiError(
            "ROUTE_NOT_FOUND",
            404,
            "route not found",
            {"method": method, "path": path},
        )

    def _handle_version_route(
        self,
        method: str,
        version_id: str,
        action: str | None,
    ) -> None:
        if method in {"GET", "HEAD"} and action is None:
            self._send_file(*self.service.read_version(version_id))
            return
        if method in {"GET", "HEAD"} and action == "metadata":
            self._send_json(self.service.version_metadata(version_id))
            return
        if method in {"GET", "POST"} and action == "admission":
            self._send_json(self.service.admission(version_id))
            return
        if method == "DELETE" and action is None:
            parsed = urlsplit(self.path)
            query = parse_qs(parsed.query, keep_blank_values=True)
            failures = _header_list(self.headers.get("X-Simulate-Domain-Failure"))
            failures.extend(_header_list(_query_one(query, "fail_domains")))
            failures.extend(_header_list(_query_one(query, "simulate_domain_failure")))
            result = self.service.delete_version(
                version_id, fail_domains=failures
            )
            self._send_json(result, status=200 if result["complete"] else 502)
            return
        raise ApiError(
            "METHOD_NOT_ALLOWED",
            405,
            "method is not allowed for this route",
            {"method": method, "version_id": version_id, "action": action},
        )

    @staticmethod
    def _payload_domains(payload: dict[str, Any], query: dict[str, list[str]]) -> list[str]:
        failures: list[str] = []
        for name in ("fail_domains", "simulate_domain_failure"):
            value = payload.get(name)
            if isinstance(value, str):
                failures.extend(_header_list(value))
            elif isinstance(value, list):
                failures.extend(str(item) for item in value)
            query_value = _query_one(query, name)
            if query_value:
                failures.extend(_header_list(query_value))
        return failures

    def _handle_write(self, query: dict[str, list[str]]) -> None:
        content_type = (
            (self.headers.get("Content-Type") or "")
            .split(";", 1)[0]
            .strip()
            .lower()
        )
        temporary: Path | None = None
        try:
            if content_type == "application/json":
                raw = self._read_body(self.service.max_object_bytes + 8 * 1024 * 1024)
                payload = _decode_json_object(raw)
                object_key = (
                    _query_one(query, "key")
                    or _query_one(query, "object_key")
                    or payload.get("object_key")
                    or payload.get("key")
                )
                category = (
                    _query_one(query, "category")
                    or payload.get("category")
                    or "regular"
                )
                legal_hold = _query_bool(
                    query,
                    "legal_hold",
                    self.service.parse_bool(
                        payload.get("legal_hold", payload.get("sealed")), default=False
                    ),
                )
                written_at = (
                    _query_one(query, "written_at")
                    or payload.get("written_at")
                    or self.headers.get("X-Archive-Written-At")
                )
                if "content_base64" in payload:
                    try:
                        content = base64.b64decode(
                            str(payload["content_base64"]), validate=True
                        )
                    except (ValueError, TypeError) as exc:
                        raise ApiError(
                            "INVALID_BASE64_CONTENT",
                            400,
                            "content_base64 is not valid base64",
                            {"detail": str(exc)},
                        )
                elif "content" in payload:
                    value = payload["content"]
                    content = (
                        value.encode("utf-8")
                        if isinstance(value, str)
                        else bytes(value)
                    )
                else:
                    content = b""
                if len(content) > self.service.max_object_bytes:
                    raise ApiError(
                        "OBJECT_TOO_LARGE",
                        413,
                        "object exceeds max_object_bytes",
                        {
                            "limit": self.service.max_object_bytes,
                            "received": len(content),
                        },
                    )
                temporary = self.service.new_temp_path()
                with open(temporary, "wb") as handle:
                    handle.write(content)
                    handle.flush()
                    os.fsync(handle.fileno())
                length = len(content)
                digest = hashlib.sha256(content).hexdigest()
                failures = _header_list(
                    self.headers.get("X-Simulate-Domain-Failure")
                )
                failures.extend(self._payload_domains(payload, query))
            else:
                object_key = (
                    _query_one(query, "key")
                    or _query_one(query, "object_key")
                    or self.headers.get("X-Archive-Object-Key")
                )
                category = _query_one(query, "category", "regular")
                legal_hold = _query_bool(query, "legal_hold", False)
                written_at = (
                    _query_one(query, "written_at")
                    or self.headers.get("X-Archive-Written-At")
                )
                raw_length = self.headers.get("Content-Length")
                if raw_length is None:
                    raise ApiError(
                        "CONTENT_LENGTH_REQUIRED",
                        411,
                        "Content-Length is required",
                    )
                try:
                    length = int(raw_length)
                except ValueError:
                    raise ApiError(
                        "INVALID_CONTENT_LENGTH",
                        400,
                        "Content-Length must be an integer",
                    )
                if length < 0:
                    raise ApiError(
                        "INVALID_CONTENT_LENGTH",
                        400,
                        "Content-Length must be non-negative",
                    )
                if length > self.service.max_object_bytes:
                    raise ApiError(
                        "OBJECT_TOO_LARGE",
                        413,
                        "object exceeds max_object_bytes",
                        {
                            "limit": self.service.max_object_bytes,
                            "received": length,
                        },
                    )
                temporary = self.service.new_temp_path()
                try:
                    length, digest = spool_stream(self.rfile, temporary, length)
                except ValueError as exc:
                    raise ApiError(
                        "INCOMPLETE_REQUEST_BODY",
                        400,
                        "request body ended before Content-Length",
                        {"detail": str(exc)},
                    )
                failures = _header_list(
                    self.headers.get("X-Simulate-Domain-Failure")
                )
                failures.extend(self._payload_domains({}, query))
            if not object_key:
                raise ApiError("OBJECT_KEY_REQUIRED", 400, "object_key is required")
            assert temporary is not None
            result, status = self.service.write_object(
                object_key=str(object_key),
                source_path=temporary,
                content_length=length,
                content_sha256=digest,
                category=str(category),
                legal_hold=legal_hold,
                written_at=written_at,
                fail_domains=failures,
            )
            self._send_json(result, status=status)
        finally:
            if temporary is not None:
                remove_quietly(temporary)


class ArchiveHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128

    def __init__(
        self,
        server_address: tuple[str, int],
        handler_class: type[ArchiveRequestHandler],
    ):
        self.handler_class = handler_class
        super().__init__(server_address, handler_class)


def make_server(
    service: ArchiveService,
    *,
    host: str = "127.0.0.1",
    port: int = 8080,
    quiet: bool = False,
) -> ArchiveHTTPServer:
    handler = type(
        "BoundArchiveRequestHandler",
        (ArchiveRequestHandler,),
        {"service": service, "quiet": quiet},
    )
    server = ArchiveHTTPServer((host, port), handler)
    server.service = service  # type: ignore[attr-defined]
    return server
