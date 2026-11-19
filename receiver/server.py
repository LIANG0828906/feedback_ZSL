from __future__ import annotations

import json
import logging
import os
import random
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import urlsplit


LOGGER = logging.getLogger("receiver")
MAX_BODY_BYTES = 2 * 1024 * 1024


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, str(default)))
    except ValueError:
        return default


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, str(default)))
    except ValueError:
        return default


class ReceiverState:
    def __init__(self) -> None:
        self.tenant_id = os.environ.get("TENANT_ID", "unknown")
        self.lock = threading.Lock()
        self.random = random.Random(
            _env_int("FAULT_SEED", sum(ord(char) for char in self.tenant_id))
        )
        self.base_latency_ms = max(0, _env_int("BASE_LATENCY_MS", 30))
        self.slow_ratio = min(
            1.0, max(0.0, _env_float("SLOW_RATIO", 0.0))
        )
        self.slow_min_ms = max(0, _env_int("SLOW_MIN_MS", 500))
        self.slow_max_ms = max(
            self.slow_min_ms, _env_int("SLOW_MAX_MS", 3000)
        )
        self.error_ratio = min(
            1.0, max(0.0, _env_float("ERROR_RATE", 0.0))
        )
        self.permanent_4xx_ratio = min(
            1.0, max(0.0, _env_float("PERMANENT_4XX_RATE", 0.0))
        )
        self.health_ok = True
        self.seen: set[str] = set()
        self.requests = 0
        self.accepted = 0
        self.duplicates = 0
        self.errors_5xx = 0
        self.errors_4xx = 0
        self.slow_requests = 0

    def snapshot(self) -> dict[str, Any]:
        with self.lock:
            return {
                "tenant_id": self.tenant_id,
                "requests": self.requests,
                "accepted": self.accepted,
                "duplicates": self.duplicates,
                "errors_5xx": self.errors_5xx,
                "errors_4xx": self.errors_4xx,
                "slow_requests": self.slow_requests,
                "base_latency_ms": self.base_latency_ms,
                "slow_ratio": self.slow_ratio,
                "slow_min_ms": self.slow_min_ms,
                "slow_max_ms": self.slow_max_ms,
                "error_ratio": self.error_ratio,
                "permanent_4xx_ratio": self.permanent_4xx_ratio,
                "health_ok": self.health_ok,
            }

    def update(self, value: dict[str, Any]) -> dict[str, Any]:
        with self.lock:
            if "base_latency_ms" in value:
                self.base_latency_ms = max(0, int(value["base_latency_ms"]))
            if "slow_ratio" in value:
                self.slow_ratio = min(
                    1.0, max(0.0, float(value["slow_ratio"]))
                )
            if "slow_min_ms" in value:
                self.slow_min_ms = max(0, int(value["slow_min_ms"]))
            if "slow_max_ms" in value:
                self.slow_max_ms = max(
                    self.slow_min_ms, int(value["slow_max_ms"])
                )
            if "error_ratio" in value:
                self.error_ratio = min(
                    1.0, max(0.0, float(value["error_ratio"]))
                )
            if "permanent_4xx_ratio" in value:
                self.permanent_4xx_ratio = min(
                    1.0, max(0.0, float(value["permanent_4xx_ratio"]))
                )
            if "health_ok" in value:
                self.health_ok = bool(value["health_ok"])
        return self.snapshot()

    def plan_attempt(self) -> tuple[float, str | None]:
        with self.lock:
            self.requests += 1
            is_slow = self.random.random() < self.slow_ratio
            delay_ms = (
                self.random.uniform(self.slow_min_ms, self.slow_max_ms)
                if is_slow
                else float(self.base_latency_ms)
            )
            if is_slow:
                self.slow_requests += 1
            failure: str | None = None
            if self.random.random() < self.error_ratio:
                failure = "5xx"
                self.errors_5xx += 1
            elif self.random.random() < self.permanent_4xx_ratio:
                failure = "4xx"
                self.errors_4xx += 1
            return delay_ms / 1000.0, failure

    def accept(self, event_id: str) -> bool:
        with self.lock:
            if event_id in self.seen:
                self.duplicates += 1
                return False
            self.seen.add(event_id)
            self.accepted += 1
            return True

    def reset(self) -> None:
        with self.lock:
            self.seen.clear()
            self.requests = 0
            self.accepted = 0
            self.duplicates = 0
            self.errors_5xx = 0
            self.errors_4xx = 0
            self.slow_requests = 0


class ReceiverHandler(BaseHTTPRequestHandler):
    server: "ReceiverServer"
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: Any) -> None:
        LOGGER.debug("%s - %s", self.address_string(), fmt % args)

    def _send(self, status: int, value: Any) -> None:
        body = json.dumps(
            value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self) -> Any:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > MAX_BODY_BYTES:
            raise ValueError("invalid request body length")
        raw = self.rfile.read(length)
        return json.loads(raw.decode("utf-8"))

    def do_GET(self) -> None:
        path = urlsplit(self.path).path.rstrip("/") or "/"
        if path == "/live":
            self._send(200, {"live": True, "tenant_id": self.server.state.tenant_id})
            return
        if path == "/health":
            with self.server.state.lock:
                healthy = self.server.state.health_ok
            self._send(
                200 if healthy else 503,
                {
                    "healthy": healthy,
                    "tenant_id": self.server.state.tenant_id,
                },
            )
            return
        if path == "/stats":
            self._send(200, self.server.state.snapshot())
            return
        self._send(404, {"error": "not_found"})

    def do_POST(self) -> None:
        path = urlsplit(self.path).path.rstrip("/") or "/"
        try:
            body = self._body()
        except (ValueError, json.JSONDecodeError):
            self._send(400, {"error": "invalid_json"})
            return

        if path == "/webhook":
            event_id = body.get("event_id") if isinstance(body, dict) else None
            if not isinstance(event_id, str) or not event_id:
                self._send(400, {"error": "event_id_required"})
                return
            delay_seconds, failure = self.server.state.plan_attempt()
            if delay_seconds > 0:
                time.sleep(delay_seconds)
            if failure == "5xx":
                self._send(
                    503,
                    {"accepted": False, "event_id": event_id, "error": "injected_5xx"},
                )
                return
            if failure == "4xx":
                self._send(
                    422,
                    {
                        "accepted": False,
                        "event_id": event_id,
                        "error": "injected_4xx",
                    },
                )
                return
            duplicate = not self.server.state.accept(event_id)
            self._send(
                200,
                {
                    "accepted": True,
                    "duplicate": duplicate,
                    "event_id": event_id,
                    "tenant_id": self.server.state.tenant_id,
                },
            )
            return
        if path == "/control":
            if not isinstance(body, dict):
                self._send(400, {"error": "control_object_required"})
                return
            try:
                self._send(200, self.server.state.update(body))
            except (TypeError, ValueError) as exc:
                self._send(400, {"error": "invalid_control", "detail": str(exc)})
            return
        if path == "/reset":
            self.server.state.reset()
            self._send(200, self.server.state.snapshot())
            return
        self._send(404, {"error": "not_found"})


class ReceiverServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 256

    def __init__(self, address: tuple[str, int], state: ReceiverState):
        super().__init__(address, ReceiverHandler)
        self.state = state


def main() -> int:
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    state = ReceiverState()
    server = ReceiverServer(
        (os.environ.get("BIND", "0.0.0.0"), int(os.environ.get("PORT", "8080"))),
        state,
    )

    def stop(signum: int, _frame: object) -> None:
        LOGGER.info("receiver signal %s", signum)
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    LOGGER.info("receiver started tenant=%s", state.tenant_id)
    try:
        server.serve_forever(poll_interval=0.25)
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

