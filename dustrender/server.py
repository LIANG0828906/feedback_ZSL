"""Loopback-only local rendering service."""

from __future__ import annotations

import json
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from .config import RenderConfig
from .errors import DustRenderError
from .scene import Scene
from .scheduler import RenderScheduler
from .util import canonical_json_bytes


class _Handler(BaseHTTPRequestHandler):
    server_version = "DustRender/1.0"

    def log_message(self, format: str, *args: Any) -> None:
        return

    def _send(self, status: int, value: Any) -> None:
        body = canonical_json_bytes(value) + b"\n"
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path == "/health":
            self._send(HTTPStatus.OK, {"ok": True, "mode": "local-loopback"})
            return
        if self.path == "/api/samples":
            manifest = Path(self.server.work_root) / "samples" / "manifest.json"
            if not manifest.exists():
                self._send(
                    HTTPStatus.NOT_FOUND,
                    {"code": "E_NOT_FOUND", "message": "sample manifest not generated"},
                )
                return
            self._send(HTTPStatus.OK, json.loads(manifest.read_text(encoding="utf-8")))
            return
        self._send(HTTPStatus.NOT_FOUND, {"code": "E_NOT_FOUND", "path": self.path})

    def do_POST(self) -> None:
        if self.path != "/api/render":
            self._send(HTTPStatus.NOT_FOUND, {"code": "E_NOT_FOUND", "path": self.path})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 2 * 1024 * 1024:
                raise ValueError("request body must be non-empty and below 2 MiB")
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
            scene_name = payload.get("scene")
            if not isinstance(scene_name, str):
                raise ValueError("scene must be a path string")
            scene_path = Path(scene_name)
            if not scene_path.is_absolute():
                scene_path = Path(self.server.work_root) / scene_path
            scene = Scene.load(scene_path)
            config = RenderConfig.from_dict(payload.get("config"), **payload.get("overrides", {}))
            output = Path(self.server.work_root) / "render" / scene.scene_id
            run = self.server.scheduler.render(scene, config)
            run.write(output)
            self._send(
                HTTPStatus.OK if run.error is None else HTTPStatus.CONFLICT,
                {
                    "ok": run.error is None,
                    "output": str(output),
                    "summary": run.summary(),
                    "error": run.error.to_dict() if run.error else None,
                },
            )
        except DustRenderError as exc:
            self._send(HTTPStatus.CONFLICT, exc.to_dict())
        except Exception as exc:
            self._send(
                HTTPStatus.BAD_REQUEST,
                {"code": "E_REQUEST_INVALID", "message": str(exc)},
            )


def serve(work_root: str | Path, host: str = "127.0.0.1", port: int = 0) -> None:
    root = Path(work_root).resolve()
    root.mkdir(parents=True, exist_ok=True)
    if host not in {"127.0.0.1", "localhost", "::1"}:
        raise ValueError("the offline server only binds to loopback")
    server = ThreadingHTTPServer((host, port), _Handler)
    server.work_root = str(root)
    server.scheduler = RenderScheduler()
    actual_host, actual_port = server.server_address[:2]
    print(
        json.dumps(
            {"event": "server_ready", "url": f"http://{actual_host}:{actual_port}", "root": str(root)},
            sort_keys=True,
            separators=(",", ":"),
        ),
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
