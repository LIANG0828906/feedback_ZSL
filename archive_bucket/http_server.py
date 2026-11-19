"""Threaded HTTP API for the local object bucket service."""

from __future__ import annotations

import json
import re
import traceback
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Mapping

from .errors import BadRequest, BucketError, NotFound, PayloadTooLarge
from .generator import MiB
from .service import ArchiveService

_OBJECT_REVISION_RE = re.compile(
    r"^/v1/buckets/([^/]+)/objects/(.+?)/revisions/([0-9]+)$"
)
_OBJECT_REVISIONS_RE = re.compile(
    r"^/v1/buckets/([^/]+)/objects/(.+?)/revisions$"
)
_OBJECT_RE = re.compile(r"^/v1/buckets/([^/]+)/objects/(.+)$")
_SNAPSHOT_ACTION_RE = re.compile(
    r"^/v1/buckets/([^/]+)/snapshots/([^/]+)/(restore|rollback)$"
)
_SNAPSHOT_RE = re.compile(r"^/v1/buckets/([^/]+)/snapshots/([^/]+)$")


def _stable_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


class BucketRequestHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "LocalObjectBucket/1.0"
    service: ArchiveService

    def log_message(self, fmt: str, *args: object) -> None:
        print(
            f"{self.log_date_time_string()} {self.address_string()} "
            + (fmt % args),
            flush=True,
        )

    def do_OPTIONS(self) -> None:
        self.send_response(204)
        self.send_header("Allow", "GET, POST, PUT, DELETE, OPTIONS")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self) -> None:
        self._dispatch("GET")

    def do_POST(self) -> None:
        self._dispatch("POST")

    def do_PUT(self) -> None:
        self._dispatch("PUT")

    def do_DELETE(self) -> None:
        self._dispatch("DELETE")

    def _dispatch(self, method: str) -> None:
        try:
            self._route(method)
        except BucketError as exc:
            self._error(
                exc.status,
                exc.code,
                exc.message,
                exc.details,
            )
        except (BrokenPipeError, ConnectionResetError):
            return
        except Exception as exc:  # noqa: BLE001 - preserve a structured HTTP error
            traceback.print_exc()
            self._error(500, "internal_error", "internal server error", {"type": type(exc).__name__})

    def _route(self, method: str) -> None:
        parsed = urllib.parse.urlsplit(self.path)
        path = parsed.path
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)

        if path == "/v1/health" and method == "GET":
            self._json(200, {"service": "local-object-bucket", "status": "ok"})
            return
        if path == "/v1/status" and method == "GET":
            self._json(200, self.service.status())
            return
        if path == "/metrics" and method == "GET":
            self._text(200, self.service.metrics(), "text/plain; version=0.0.4; charset=utf-8")
            return
        if path in {"/v1/faults", "/v1/admin/faults"}:
            if method == "GET":
                self._json(200, self.service.public_faults())
                return
            if method == "POST":
                self._json(200, self.service.update_faults(self._read_json()))
                return
        if path == "/v1/sync" and method == "POST":
            self._sync()
            return
        if path == "/v1/buckets/backup/sync" and method == "POST":
            self._sync()
            return
        if path in {"/v1/consistency/check", "/v1/consistency"}:
            if method == "POST":
                body = self._read_json()
                self._json(200, self.service.consistency_check(
                    snapshot_id=body.get("snapshot_id"),
                    commit_seq=body.get("commit_seq"),
                    verify_content=bool(body.get("verify_content", True)),
                ))
                return
            if method == "GET":
                if path.endswith("/check"):
                    snapshot_id = self._query_one(query, "snapshot_id")
                    raw_seq = self._query_one(query, "commit_seq")
                    verify = self._query_one(query, "verify_content")
                    self._json(200, self.service.consistency_check(
                        snapshot_id=snapshot_id,
                        commit_seq=int(raw_seq) if raw_seq not in {None, ""} else None,
                        verify_content=verify not in {"0", "false", "False"},
                    ))
                else:
                    result = self.service.latest_comparison()
                    if result is None:
                        raise BadRequest("no consistency check has been run")
                    self._json(200, result)
                return
        if path == "/v1/load/generate" and method == "POST":
            body = self._read_json()
            self._json(
                200,
                self.service.generate_dataset(
                    seed=body.get("seed"),
                    count=int(body.get("count", 200)),
                    target_bytes=int(body.get("target_bytes", 120 * MiB)),
                    revisions=int(body.get("revisions", 1)),
                    prefix=str(body.get("prefix", "archive")),
                    rate_per_second=float(body.get("rate_per_second", 0.0)),
                ),
            )
            return
        if path == "/v1/gc" and method == "POST":
            body = self._read_json()
            bucket_name = body.get("bucket")
            if bucket_name in {"main", "backup"}:
                self._json(
                    200,
                    self.service.gc_bucket(
                        bucket_name,
                        target_bytes=(
                            int(body["target_bytes"])
                            if body.get("target_bytes") is not None
                            else None
                        ),
                    ),
                )
            else:
                self._json(
                    200,
                    {
                        "backup": self.service.gc_bucket("backup"),
                        "main": self.service.gc_bucket("main"),
                    },
                )
            return

        match = _OBJECT_REVISION_RE.fullmatch(path)
        if match and method == "GET":
            bucket_name, encoded_key, raw_revision = match.groups()
            key = urllib.parse.unquote(encoded_key)
            meta, data = self.service.bucket(bucket_name).get_revision(
                key, int(raw_revision)
            )
            self._binary(meta, data)
            return

        match = _OBJECT_REVISIONS_RE.fullmatch(path)
        if match and method == "GET":
            bucket_name, encoded_key = match.groups()
            key = urllib.parse.unquote(encoded_key)
            self._json(200, self.service.bucket(bucket_name).list_revisions(key))
            return

        match = _OBJECT_RE.fullmatch(path)
        if match:
            bucket_name, encoded_key = match.groups()
            key = urllib.parse.unquote(encoded_key)
            store = self.service.bucket(bucket_name)
            if method == "GET":
                meta, data = store.get_latest(key)
                self._binary(meta, data)
                return
            if method == "PUT":
                data = self._read_body(self.service.max_object_bytes)
                expected_hash = self.headers.get("X-Content-SHA256")
                result = store.put(key, data, expected_sha256=expected_hash)
                result["gc"] = self.service.maybe_auto_gc(bucket_name)
                self._json(201, result)
                return
            if method == "DELETE":
                result = store.delete(key)
                result["gc"] = self.service.maybe_auto_gc(bucket_name)
                self._json(200, result)
                return

        match = _SNAPSHOT_ACTION_RE.fullmatch(path)
        if match and method == "POST":
            bucket_name, snapshot_id, _action = match.groups()
            if bucket_name != "main":
                raise BadRequest("rollback is supported only on the main bucket")
            self._json(
                200,
                self.service.main.restore_snapshot(urllib.parse.unquote(snapshot_id)),
            )
            return

        match = _SNAPSHOT_RE.fullmatch(path)
        if match:
            bucket_name, snapshot_id = match.groups()
            store = self.service.bucket(bucket_name)
            if method == "GET":
                self._json(200, store.get_snapshot(urllib.parse.unquote(snapshot_id)))
                return
            if method == "DELETE":
                self._json(200, store.delete_snapshot(urllib.parse.unquote(snapshot_id)))
                return
        if path == "/v1/buckets/main/rollback" and method == "POST":
            body = self._read_json()
            snapshot_id = body.get("snapshot_id")
            if not isinstance(snapshot_id, str) or not snapshot_id:
                raise BadRequest("snapshot_id is required")
            self._json(200, self.service.main.restore_snapshot(snapshot_id))
            return

        bucket_action = re.fullmatch(r"^/v1/buckets/([^/]+)/([A-Za-z0-9_-]+)$", path)
        if bucket_action:
            bucket_name = urllib.parse.unquote(bucket_action.group(1))
            action = bucket_action.group(2)
            store = self.service.bucket(bucket_name)
            if action in {"snapshots", "snapshot"} and method == "POST":
                body = self._read_json()
                self._json(201, store.create_snapshot(body.get("snapshot_id")))
                return
            if action in {"snapshots", "snapshot"} and method == "GET":
                self._json(200, store.list_snapshots())
                return
            if action in {"objects", "keys"} and method == "GET":
                prefix = self._query_one(query, "prefix") or ""
                raw_limit = self._query_one(query, "limit")
                self._json(
                    200,
                    store.list_visible(
                        prefix=prefix,
                        limit=int(raw_limit) if raw_limit else None,
                    ),
                )
                return
            if action == "view" and method == "GET":
                raw_seq = self._query_one(query, "commit_seq")
                self._json(
                    200,
                    store.view_at(int(raw_seq) if raw_seq else None),
                )
                return
            if action == "commits" and method == "GET":
                after_seq = int(self._query_one(query, "after_seq") or "0")
                limit = int(self._query_one(query, "limit") or "100")
                self._json(200, store.commit_events(after_seq, limit))
                return
            if action == "stats" and method == "GET":
                self._json(200, store.stats())
                return
            if action == "gc" and method == "POST":
                body = self._read_json()
                self._json(
                    200,
                    self.service.gc_bucket(
                        bucket_name,
                        target_bytes=(
                            int(body["target_bytes"])
                            if body.get("target_bytes") is not None
                            else None
                        ),
                    ),
                )
                return

        raise _not_found(path, method)

    def _sync(self) -> None:
        body = self._read_json()
        result = self.service.sync(
            source=body.get("source"),
            limit=int(body.get("limit", 100)),
            max_events=int(body.get("max_events", 10000)),
        )
        self._json(200, result)

    def _read_json(self) -> dict[str, Any]:
        data = self._read_body(16 * MiB)
        if not data:
            return {}
        try:
            value = json.loads(data.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BadRequest("request body must be valid JSON") from exc
        if not isinstance(value, dict):
            raise BadRequest("request JSON must be an object")
        return value

    def _read_body(self, maximum: int) -> bytes:
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            raise BadRequest("Content-Length is required")
        try:
            length = int(raw_length)
        except ValueError as exc:
            raise BadRequest("invalid Content-Length") from exc
        if length < 0:
            raise BadRequest("invalid Content-Length")
        if length > maximum:
            raise PayloadTooLarge(
                "request body exceeds configured limit",
                maximum_bytes=maximum,
                content_length=length,
            )
        chunks: list[bytes] = []
        remaining = length
        while remaining:
            chunk = self.rfile.read(min(remaining, 1024 * 1024))
            if not chunk:
                raise BadRequest("request body ended early")
            chunks.append(chunk)
            remaining -= len(chunk)
        return b"".join(chunks)

    @staticmethod
    def _query_one(query: Mapping[str, list[str]], name: str) -> str | None:
        values = query.get(name)
        return values[0] if values else None

    def _json(self, status: int, value: Any) -> None:
        self._send_bytes(status, _stable_json(value), "application/json; charset=utf-8")

    def _text(self, status: int, value: str, content_type: str) -> None:
        self._send_bytes(status, value.encode("utf-8"), content_type)

    def _binary(self, meta: Mapping[str, Any], data: bytes) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("ETag", f'"{meta["content_hash"]}"')
        self.send_header("X-Content-SHA256", str(meta["content_hash"]))
        self.send_header("X-Object-Key", urllib.parse.quote(str(meta["key"]), safe=""))
        self.send_header("X-Object-Revision-Id", str(meta["revision_id"]))
        self.send_header("X-Bucket-Commit-Seq", str(meta["commit_seq"]))
        self.end_headers()
        self.wfile.write(data)

    def _send_bytes(self, status: int, data: bytes, content_type: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _error(
        self,
        status: int,
        code: str,
        message: str,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        payload = {
            "code": code,
            "details": dict(details or {}),
            "error": code,
            "message": message,
            "status": status,
        }
        try:
            self._send_bytes(status, _stable_json(payload), "application/json; charset=utf-8")
        except (BrokenPipeError, ConnectionResetError):
            pass


def _not_found(path: str, method: str) -> NotFound:
    return NotFound("route not found", method=method, path=path)


class BucketHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        service: ArchiveService,
    ) -> None:
        self.service = service
        handler = type(
            "BoundBucketRequestHandler",
            (BucketRequestHandler,),
            {"service": service},
        )
        super().__init__(server_address, handler)


def create_server(
    service: ArchiveService,
    host: str = "127.0.0.1",
    port: int = 0,
) -> BucketHTTPServer:
    server = BucketHTTPServer((host, port), service)
    actual_host, actual_port = server.server_address[:2]
    service.base_url = f"http://{actual_host}:{actual_port}"
    return server
