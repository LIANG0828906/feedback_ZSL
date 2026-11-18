"""Small dependency-free HTTP service around :class:`ArchiveService`."""

from __future__ import annotations

import json
import sys
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from . import __version__
from .errors import ArchiveError, invalid_request
from .service import ArchiveService


MAX_BODY_BYTES = 2 * 1024 * 1024


def _json_bytes(payload: Any) -> bytes:
    return (
        json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        + "\n"
    ).encode("utf-8")


def _query_int(query: dict[str, list[str]], name: str) -> int | None:
    values = query.get(name)
    if not values or values[0] == "":
        return None
    try:
        return int(values[0])
    except (TypeError, ValueError) as exc:
        raise invalid_request(f"{name} must be an integer") from exc


def make_handler(service: ArchiveService) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = f"wal-archive/{__version__}"
        sys_version = ""

        def log_message(self, fmt: str, *args: Any) -> None:
            sys.stderr.write(
                "%s - - [%s] %s\n"
                % (self.address_string(), self.log_date_time_string(), fmt % args)
            )

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
            self.send_header("X-Content-Type-Options", "nosniff")
            if extra_headers:
                for key, value in extra_headers.items():
                    self.send_header(key, value)
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(body)

        def _send_json(self, status: int, payload: Any) -> None:
            self._send(
                status,
                _json_bytes(payload),
                "application/json; charset=utf-8",
            )

        def _send_error(self, exc: ArchiveError) -> None:
            self._send_json(exc.http_status, {"error": exc.as_dict()})

        def _body(self) -> dict[str, Any]:
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError as exc:
                raise invalid_request("invalid Content-Length") from exc
            if length > MAX_BODY_BYTES:
                raise invalid_request("request body is too large")
            raw = self.rfile.read(length) if length else b""
            if not raw:
                return {}
            try:
                payload = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise invalid_request("request body must be valid JSON") from exc
            if not isinstance(payload, dict):
                raise invalid_request("request body must be a JSON object")
            return payload

        def _handle(self) -> None:
            split = urlsplit(self.path)
            path = unquote(split.path)
            query = parse_qs(split.query, keep_blank_values=True)
            try:
                if path == "/":
                    self._send_json(
                        200,
                        {
                            "service": "wal-archive",
                            "version": __version__,
                            "endpoints": [
                                "GET /healthz",
                                "POST /v1/scan",
                                "GET /v1/archive",
                                "GET /v1/conflicts",
                                "POST /v1/recovery-points",
                                "GET /v1/recovery-points",
                                "GET /v1/recovery-points/{id}",
                                "POST /v1/validate",
                                "GET /v1/report",
                                "GET /v1/status",
                                "GET /metrics",
                            ],
                        },
                    )
                    return
                if path == "/healthz" and self.command in ("GET", "HEAD"):
                    self._send_json(200, {"status": "ok", "version": __version__})
                    return
                if path == "/metrics" and self.command in ("GET", "HEAD"):
                    body = service.metrics().encode("utf-8")
                    self._send(
                        200,
                        body,
                        "text/plain; version=0.0.4; charset=utf-8",
                    )
                    return
                if path == "/v1/scan" and self.command in ("GET", "POST"):
                    self._send_json(200, service.scan())
                    return
                if path == "/v1/archive" and self.command in ("GET", "HEAD"):
                    instance = (query.get("instance") or [None])[0] or None
                    timeline = _query_int(query, "timeline")
                    self._send_json(
                        200,
                        service.archive_view(instance=instance, timeline=timeline),
                    )
                    return
                if path == "/v1/conflicts" and self.command in ("GET", "HEAD"):
                    timeline = _query_int(query, "timeline")
                    segno = _query_int(query, "segno")
                    self._send_json(
                        200, service.find_conflicts(timeline=timeline, segno=segno)
                    )
                    return
                if path == "/v1/recovery-points":
                    if self.command in ("GET", "HEAD"):
                        instance = (query.get("instance") or [None])[0] or None
                        timeline = _query_int(query, "timeline")
                        self._send_json(
                            200,
                            service.list_recoveries(
                                instance=instance, timeline=timeline
                            ),
                        )
                        return
                    if self.command == "POST":
                        body = self._body()
                        instance = body.get("instance")
                        timeline = body.get("timeline")
                        target = body.get("target_time")
                        source = body.get("source_instance")
                        refresh = body.get("refresh", True)
                        if not isinstance(instance, str):
                            raise invalid_request("instance is required")
                        if isinstance(timeline, bool) or not isinstance(timeline, int):
                            raise invalid_request("timeline must be an integer")
                        if not isinstance(target, str):
                            raise invalid_request("target_time is required")
                        if source is not None and not isinstance(source, str):
                            raise invalid_request("source_instance must be a string")
                        if not isinstance(refresh, bool):
                            raise invalid_request("refresh must be a boolean")
                        self._send_json(
                            200,
                            service.resolve_recovery(
                                instance=instance,
                                timeline=timeline,
                                target_time=target,
                                source_instance=source,
                                refresh=refresh,
                            ),
                        )
                        return
                    raise ArchiveError(
                        "WAL017",
                        "E_METHOD_NOT_ALLOWED",
                        "method not allowed",
                        http_status=405,
                    )
                if path.startswith("/v1/recovery-points/") and self.command in (
                    "GET",
                    "HEAD",
                ):
                    recovery_id = path[len("/v1/recovery-points/") :]
                    if not recovery_id or "/" in recovery_id:
                        raise invalid_request("invalid recovery point id")
                    self._send_json(200, service.get_recovery(recovery_id))
                    return
                if path == "/v1/validate" and self.command == "POST":
                    body = self._body()
                    instance = body.get("instance")
                    timeline = body.get("timeline")
                    segno = body.get("segno")
                    digest = body.get("digest")
                    refresh = body.get("refresh", True)
                    if not isinstance(instance, str):
                        raise invalid_request("instance is required")
                    if isinstance(timeline, bool) or not isinstance(timeline, int):
                        raise invalid_request("timeline must be an integer")
                    if isinstance(segno, bool) or not isinstance(segno, int):
                        raise invalid_request("segno must be an integer")
                    if digest is not None and not isinstance(digest, str):
                        raise invalid_request("digest must be a string")
                    if not isinstance(refresh, bool):
                        raise invalid_request("refresh must be a boolean")
                    self._send_json(
                        200,
                        service.validate_segment(
                            instance=instance,
                            timeline=timeline,
                            segno=segno,
                            digest=digest,
                            refresh=refresh,
                        ),
                    )
                    return
                if path == "/v1/report" and self.command in ("GET", "HEAD"):
                    self._send_json(200, service.report())
                    return
                if path == "/v1/status" and self.command in ("GET", "HEAD"):
                    instance = (query.get("instance") or [None])[0]
                    if not instance:
                        raise invalid_request("instance query parameter is required")
                    timeline = _query_int(query, "timeline")
                    self._send_json(
                        200,
                        service.status(instance=instance, timeline=timeline),
                    )
                    return
                code, name = ("WAL018", "E_NOT_FOUND")
                raise ArchiveError(
                    code, name, "endpoint not found", http_status=404
                )
            except ArchiveError as exc:
                self._send_error(exc)
            except BrokenPipeError:
                return
            except Exception:
                traceback.print_exc(file=sys.stderr)
                self._send_error(
                    ArchiveError(
                        "WAL019",
                        "E_INTERNAL",
                        "internal server error",
                        http_status=500,
                    )
                )

        do_GET = _handle
        do_HEAD = _handle
        do_POST = _handle
        do_PUT = _handle
        do_PATCH = _handle
        do_DELETE = _handle

    return Handler


def serve(service: ArchiveService, host: str, port: int) -> None:
    server = ThreadingHTTPServer((host, port), make_handler(service))
    server.daemon_threads = True
    try:
        server.serve_forever(poll_interval=0.25)
    finally:
        server.server_close()
