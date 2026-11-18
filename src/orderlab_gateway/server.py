from __future__ import annotations

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlparse

from .config import GatewayConfig
from .engine import QueryEngine, QueryRequest
from .errors import E_INTERNAL, E_REQUEST, GatewayError
from .replay import ReplayRunner


class GatewayHandler(BaseHTTPRequestHandler):
    engine: QueryEngine
    config: GatewayConfig
    replay: ReplayRunner
    server_version = "OrderLabGateway/0.1"

    def do_GET(self) -> None:
        path = urlparse(self.path).path
        if path == "/health":
            self._send(200, {"ok": True, "service": "orderlab-gateway"})
        elif path == "/status":
            self._send(200, {"ok": True, "status": self.engine.status()})
        elif path == "/":
            self._send(
                200,
                {
                    "ok": True,
                    "endpoints": ["POST /query", "POST /replay", "GET /status"],
                },
            )
        else:
            self._send(404, {"ok": False, "error": {"code": E_REQUEST, "message": "not found"}})

    def do_POST(self) -> None:
        path = urlparse(self.path).path
        try:
            body = self._read_json()
            if path == "/query":
                self._handle_query(body)
            elif path == "/replay":
                self._handle_replay(body)
            else:
                self._send(
                    404,
                    {"ok": False, "error": {"code": E_REQUEST, "message": "not found"}},
                )
        except GatewayError as exc:
            self._send(200, {"ok": False, "error": exc.to_dict()})
        except Exception as exc:
            print(f"gateway internal error: {exc}", file=sys.stderr)
            self._send(
                500,
                {
                    "ok": False,
                    "error": {
                        "code": E_INTERNAL,
                        "message": "internal gateway error",
                    },
                },
            )

    def _handle_query(self, body: dict[str, Any]) -> None:
        sql = body.get("sql")
        if not isinstance(sql, str):
            raise GatewayError(E_REQUEST, "request body must contain a string field named sql")
        request = QueryRequest(
            sql=sql,
            dialect=body.get("dialect"),
            query_id=body.get("query_id"),
            fallback=body.get("fallback"),
            tenant=str(body["tenant"]) if body.get("tenant") is not None else None,
        )
        try:
            response = self.engine.query(request)
        except GatewayError as exc:
            payload = {
                "ok": False,
                "query_id": request.query_id,
                "dialect": request.dialect,
                "error": exc.to_dict(),
            }
            self._send(200, payload)
            return
        self._send(200, response)

    def _handle_replay(self, body: dict[str, Any]) -> None:
        concurrency = body.get("concurrency")
        if concurrency is not None and not isinstance(concurrency, int):
            raise GatewayError(E_REQUEST, "concurrency must be an integer")
        repeats = body.get("repeats", 1)
        if not isinstance(repeats, int):
            raise GatewayError(E_REQUEST, "repeats must be an integer")
        report = self.replay.run(concurrency=concurrency, repeats=repeats)
        self._send(200, {"ok": True, "report": report})

    def _read_json(self) -> dict[str, Any]:
        length_text = self.headers.get("Content-Length", "0")
        try:
            length = int(length_text)
        except ValueError as exc:
            raise GatewayError(E_REQUEST, "invalid Content-Length") from exc
        if length < 1 or length > 1024 * 1024:
            raise GatewayError(E_REQUEST, "request body must be between 1 byte and 1 MiB")
        try:
            data = json.loads(self.rfile.read(length).decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise GatewayError(E_REQUEST, "request body must be valid JSON") from exc
        if not isinstance(data, dict):
            raise GatewayError(E_REQUEST, "request body must be a JSON object")
        return data

    def _send(self, status: int, payload: dict[str, Any]) -> None:
        data = json.dumps(
            payload,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt: str, *args: Any) -> None:
        print(f"gateway: {self.address_string()} {fmt % args}", file=sys.stderr)


def serve(config: GatewayConfig, engine: QueryEngine | None = None) -> None:
    active_engine = engine or QueryEngine(config)
    handler = type(
        "ConfiguredGatewayHandler",
        (GatewayHandler,),
        {
            "engine": active_engine,
            "config": config,
            "replay": ReplayRunner(config, active_engine),
        },
    )
    httpd = ThreadingHTTPServer((config.server.host, config.server.port), handler)
    httpd.daemon_threads = True
    print(
        f"orderlab gateway listening on {config.server.host}:{config.server.port}",
        file=sys.stderr,
    )
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
