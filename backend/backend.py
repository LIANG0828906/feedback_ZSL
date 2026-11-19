#!/usr/bin/env python3
"""Local fragment receiver for the edge gateway.

The implementation uses only Python's standard library.  It is intentionally
single-process so its memory use is predictable on the target host.  Requests
may arrive concurrently; the small critical section only protects the
per-session received ranges.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import signal
import sys
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


MAX_BODY = 64 * 1024


def merge_range(ranges: list[list[int]], seq: int) -> list[list[int]]:
    """Insert seq into a sorted list of closed ranges."""
    merged: list[list[int]] = []
    placed = False
    for start, end in ranges:
        if end + 1 < seq:
            merged.append([start, end])
            continue
        if seq + 1 < start:
            if not placed:
                merged.append([seq, seq])
                placed = True
            merged.append([start, end])
            continue
        if not placed:
            merged.append([min(start, seq), max(end, seq)])
            placed = True
        else:
            merged[-1][1] = max(merged[-1][1], end)
    if not placed:
        merged.append([seq, seq])
    return merged


@dataclass
class SessionState:
    accepted: dict[int, dict[str, Any]] = field(default_factory=dict)
    ranges: list[list[int]] = field(default_factory=list)
    rejected_seqs: set[int] = field(default_factory=set)
    rejection_count: int = 0
    success_count: int = 0
    duplicate_count: int = 0
    bytes_received: int = 0
    updated_at: float = field(default_factory=time.time)


@dataclass
class RejectRule:
    seqs: set[int]
    reason: str
    session_ids: set[str]
    device_ids: set[str]
    ingress: str
    expires_at: float


class BackendState:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.sessions: dict[str, SessionState] = {}
        self.delay_ms = 0
        self.delay_until = 0.0
        self.reject_rules: list[RejectRule] = []
        self.in_flight = 0
        self.in_flight_peak = 0
        self.requests = 0
        self.rejected = 0
        self.duplicates = 0
        self.started_at = time.time()

    def enter_request(self) -> tuple[int, float]:
        with self.lock:
            self.requests += 1
            self.in_flight += 1
            self.in_flight_peak = max(self.in_flight_peak, self.in_flight)
            now = time.monotonic()
            delay = self.delay_ms if now < self.delay_until else 0
        return self.in_flight, delay / 1000.0

    def leave_request(self) -> None:
        with self.lock:
            self.in_flight -= 1

    def fragment(self, req: dict[str, Any], payload: bytes) -> dict[str, Any]:
        session_id = str(req["session_id"])
        seq = int(req["seq"])
        ingress = str(req.get("ingress", ""))
        device_id = str(req.get("device_id", ""))
        now = time.monotonic()
        with self.lock:
            state = self.sessions.setdefault(session_id, SessionState())
            if seq in state.accepted:
                state.duplicate_count += 1
                self.duplicates += 1
                return self._accepted_response(session_id, seq, state, duplicate=True)

            rule = self._match_reject(now, session_id, device_id, ingress, seq)
            if rule is not None:
                state.rejected_seqs.add(seq)
                state.rejection_count += 1
                self.rejected += 1
                return {
                    "accepted": False,
                    "duplicate": False,
                    "rejected": True,
                    "reason": rule.reason,
                    "code": "INJECTED_FRAGMENT_REJECT",
                    "rejected_seq": seq,
                    "accepted_ranges": state.ranges,
                    "ack_through": ack_through(state.ranges),
                }

            state.accepted[seq] = {
                "size": len(payload),
                "sha256": hashlib.sha256(payload).hexdigest(),
                "at": time.time(),
            }
            state.ranges = merge_range(state.ranges, seq)
            state.success_count += 1
            state.bytes_received += len(payload)
            state.rejected_seqs.discard(seq)
            state.updated_at = time.time()
            return self._accepted_response(session_id, seq, state, duplicate=False)

    def _match_reject(
        self, now: float, session_id: str, device_id: str, ingress: str, seq: int
    ) -> RejectRule | None:
        self.reject_rules = [r for r in self.reject_rules if r.expires_at > now]
        for rule in self.reject_rules:
            if rule.seqs and seq not in rule.seqs:
                continue
            if rule.session_ids and session_id not in rule.session_ids:
                continue
            if rule.device_ids and device_id not in rule.device_ids:
                continue
            if rule.ingress and ingress != rule.ingress:
                continue
            return rule
        return None

    @staticmethod
    def _accepted_response(
        session_id: str, seq: int, state: SessionState, duplicate: bool
    ) -> dict[str, Any]:
        return {
            "accepted": True,
            "duplicate": duplicate,
            "rejected": False,
            "reason": "",
            "code": "OK",
            "rejected_seq": None,
            "session_id": session_id,
            "seq": seq,
            "accepted_ranges": state.ranges,
            "ack_through": ack_through(state.ranges),
            "success_count": state.success_count,
            "rejection_count": state.rejection_count,
            "duplicate_count": state.duplicate_count,
        }

    def add_reject(
        self,
        *,
        seqs: list[int],
        reason: str,
        session_ids: list[str],
        device_ids: list[str],
        ingress: str,
        duration_ms: int,
    ) -> None:
        now = time.monotonic()
        rule = RejectRule(
            seqs=set(seqs),
            reason=reason or "injected_fragment_reject",
            session_ids=set(session_ids),
            device_ids=set(device_ids),
            ingress=ingress,
            expires_at=now + max(duration_ms, 1) / 1000.0,
        )
        with self.lock:
            self.reject_rules.append(rule)

    def set_delay(self, delay_ms: int, duration_ms: int) -> None:
        with self.lock:
            self.delay_ms = max(delay_ms, 0)
            self.delay_until = time.monotonic() + max(duration_ms, 1) / 1000.0

    def clear_faults(self) -> None:
        with self.lock:
            self.delay_ms = 0
            self.delay_until = 0
            self.reject_rules.clear()

    def snapshot(self) -> dict[str, Any]:
        with self.lock:
            return {
                "now_unix": time.time(),
                "uptime_s": time.time() - self.started_at,
                "sessions": len(self.sessions),
                "requests": self.requests,
                "accepted": sum(s.success_count for s in self.sessions.values()),
                "rejected": self.rejected,
                "duplicates": self.duplicates,
                "in_flight": self.in_flight,
                "in_flight_peak": self.in_flight_peak,
                "active_reject_rules": len(self.reject_rules),
                "delay_ms": self.delay_ms,
            }


def ack_through(ranges: list[list[int]]) -> int:
    for start, end in ranges:
        if start == 0:
            return end
        if start > 0:
            return -1
    return -1


STATE = BackendState()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "EdgeFragmentBackend/1.0"

    def log_message(self, fmt: str, *args: Any) -> None:
        if getattr(self.server, "verbose", False):
            super().log_message(fmt, *args)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"ok": True, "time_unix": time.time()})
            return
        if self.path == "/admin/state":
            self._json(200, STATE.snapshot())
            return
        if self.path.startswith("/v1/sessions/") and self.path.endswith("/ranges"):
            session_id = self.path[len("/v1/sessions/") : -len("/ranges")]
            with STATE.lock:
                state = STATE.sessions.get(session_id)
                if state is None:
                    self._json(404, {"error": "session not found"})
                    return
                body = {
                    "session_id": session_id,
                    "accepted_ranges": state.ranges,
                    "ack_through": ack_through(state.ranges),
                    "success_count": state.success_count,
                    "rejection_count": state.rejection_count,
                }
            self._json(200, body)
            return
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path == "/v1/fragments":
            self._fragment()
            return
        if self.path == "/admin/delay":
            body = self._body()
            if body is None:
                return
            try:
                delay_ms = int(body.get("delay_ms", 0))
                duration_ms = int(body.get("duration_ms", 1000))
                if delay_ms < 0 or delay_ms > 10_000:
                    raise ValueError("delay_ms must be in 0..10000")
            except (TypeError, ValueError) as exc:
                self._json(400, {"error": str(exc)})
                return
            STATE.set_delay(delay_ms, duration_ms)
            self._json(200, {"ok": True, "delay_ms": delay_ms, "duration_ms": duration_ms})
            return
        if self.path == "/admin/reject":
            body = self._body()
            if body is None:
                return
            try:
                seqs = [int(v) for v in body.get("seqs", [])]
                duration_ms = int(body.get("duration_ms", 1000))
            except (TypeError, ValueError) as exc:
                self._json(400, {"error": str(exc)})
                return
            STATE.add_reject(
                seqs=seqs,
                reason=str(body.get("reason", "")),
                session_ids=[str(v) for v in body.get("session_ids", [])],
                device_ids=[str(v) for v in body.get("device_ids", [])],
                ingress=str(body.get("ingress", "")),
                duration_ms=duration_ms,
            )
            self._json(200, {"ok": True, "seqs": seqs, "duration_ms": duration_ms})
            return
        if self.path == "/admin/clear":
            STATE.clear_faults()
            self._json(200, {"ok": True})
            return
        self._json(404, {"error": "not found"})

    def _fragment(self) -> None:
        body = self._body()
        if body is None:
            return
        required = ("session_id", "seq", "data_base64")
        missing = [key for key in required if key not in body]
        if missing:
            self._json(400, {"error": "missing fields: " + ",".join(missing)})
            return
        try:
            session_id = str(body["session_id"])
            seq = int(body["seq"])
            if seq < 0:
                raise ValueError("seq must be non-negative")
            payload = base64.b64decode(str(body["data_base64"]), validate=True)
            digest = hashlib.sha256(payload).hexdigest()
            claimed = str(body.get("checksum_sha256", ""))
            if claimed and claimed.lower() != digest:
                raise ValueError("checksum mismatch")
        except (TypeError, ValueError) as exc:
            self._json(400, {"error": str(exc)})
            return
        _, delay = STATE.enter_request()
        try:
            if delay:
                time.sleep(delay)
            result = STATE.fragment(body, payload)
        finally:
            STATE.leave_request()
        self._json(200, result)

    def _body(self) -> dict[str, Any] | None:
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self._json(400, {"error": "invalid Content-Length"})
            return None
        if length <= 0 or length > MAX_BODY:
            self._json(400, {"error": "invalid body length"})
            return None
        raw = self.rfile.read(length)
        try:
            value = json.loads(raw)
        except json.JSONDecodeError as exc:
            self._json(400, {"error": f"invalid JSON: {exc}"})
            return None
        if not isinstance(value, dict):
            self._json(400, {"error": "JSON object required"})
            return None
        return value

    def _json(self, status: int, value: Any) -> None:
        raw = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        self.wfile.write(raw)


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128

    def __init__(self, addr: tuple[str, int], verbose: bool = False) -> None:
        super().__init__(addr, Handler)
        self.verbose = verbose


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args()
    server = Server((args.host, args.port), args.verbose)
    print(
        json.dumps(
            {"event": "backend_listening", "addr": f"{args.host}:{args.port}"},
            separators=(",", ":"),
        ),
        flush=True,
    )

    def stop(_signum: int, _frame: Any) -> None:
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        server.serve_forever(poll_interval=0.1)
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
