from __future__ import annotations

import argparse
import http.client
import json
import os
import secrets
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse


def _json_bytes(value: dict[str, Any]) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _free_port(start: int, end: int, excluded: set[int]) -> int:
    span = end - start + 1
    offset = secrets.randbelow(span) if span > 0 else 0
    candidates = list(range(start + offset, end + 1)) + list(
        range(start, start + offset)
    )
    for port in candidates:
        if port in excluded:
            continue
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
                sock.setsockopt(
                    socket.SOL_SOCKET,
                    socket.SO_EXCLUSIVEADDRUSE,
                    1,
                )
            sock.bind(("127.0.0.1", port))
            return port
        except OSError:
            continue
        finally:
            sock.close()
    raise RuntimeError(f"no free backend port in range {start}-{end}")


@dataclass
class BackendSlot:
    version: str
    port: int
    process: subprocess.Popen[bytes]
    backend_id: int
    started_monotonic: float = field(default_factory=time.monotonic)

    def stop(self) -> None:
        if self.process.poll() is not None:
            return
        try:
            request = urllib.request.Request(
                f"http://127.0.0.1:{self.port}/admin/shutdown",
                data=b"{}",
                method="POST",
                headers={"Content-Type": "application/json"},
            )
            urllib.request.urlopen(request, timeout=0.5).read()
        except Exception:
            pass
        try:
            self.process.wait(timeout=1.0)
            return
        except subprocess.TimeoutExpired:
            pass
        self.process.terminate()
        try:
            self.process.wait(timeout=1.0)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=1.0)


class InstanceSupervisor:
    def __init__(
        self,
        instance_id: str,
        group: str,
        initial_version: str,
        host: str,
        port: int,
        backend_port_range: tuple[int, int],
        faults: dict[str, Any],
        precheck_timeout_seconds: float,
        drain_timeout_seconds: float,
        request_timeout_seconds: float,
    ) -> None:
        self.instance_id = instance_id
        self.group = group
        self.initial_version = initial_version
        self.host = host
        self.port = port
        self.backend_port_range = backend_port_range
        self.version_faults = dict(faults.get("versions", {}))
        self.rollback_restart_refused = bool(
            faults.get("rollback_restart_refused", False)
        )
        self.precheck_timeout_seconds = precheck_timeout_seconds
        self.drain_timeout_seconds = drain_timeout_seconds
        self.request_timeout_seconds = request_timeout_seconds

        self._lock = threading.RLock()
        self._operation_lock = threading.Lock()
        self._closing = threading.Event()
        self._active: BackendSlot | None = None
        self._candidate: BackendSlot | None = None
        self._inflight: dict[int, int] = {}
        self._next_backend_id = 1
        self._switch_generation = 0
        self._samples: list[dict[str, Any]] = []
        self._next_sample_id = 1
        self._used_backend_ports: set[int] = set()
        self._started_monotonic = time.monotonic()

        self._active = self._start_backend(initial_version)
        self.server = SupervisorServer((self.host, self.port), self)

    def serve_forever(self) -> None:
        self.server.serve_forever(poll_interval=0.1)

    def close(self) -> None:
        self._closing.set()
        with self._lock:
            candidate = self._candidate
            active = self._active
            self._candidate = None
            self._active = None
        if candidate is not None:
            candidate.stop()
        if active is not None:
            active.stop()

    def _backend_faults(self, version: str) -> dict[str, Any]:
        value = self.version_faults.get(version, {})
        if not isinstance(value, dict):
            return {}
        return value

    def _start_backend(self, version: str) -> BackendSlot:
        root = str(Path(__file__).resolve().parent.parent)
        env = os.environ.copy()
        existing_pythonpath = env.get("PYTHONPATH")
        env["PYTHONPATH"] = (
            root
            if not existing_pythonpath
            else root + os.pathsep + existing_pythonpath
        )
        faults_json = json.dumps(
            self._backend_faults(version),
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        )
        last_error = "unknown"
        for _start_attempt in range(6):
            with self._lock:
                port = _free_port(
                    self.backend_port_range[0],
                    self.backend_port_range[1],
                    self._used_backend_ports,
                )
                self._used_backend_ports.add(port)
                backend_id = self._next_backend_id
                self._next_backend_id += 1
            command = [
                sys.executable,
                "-m",
                "settlement_rollout.backend",
                "--instance-id",
                self.instance_id,
                "--version",
                version,
                "--port",
                str(port),
                "--host",
                "127.0.0.1",
                "--faults-json",
                faults_json,
            ]
            process = subprocess.Popen(
                command,
                cwd=root,
                env=env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
            )
            slot = BackendSlot(
                version=version,
                port=port,
                process=process,
                backend_id=backend_id,
            )
            deadline = time.monotonic() + max(
                30.0,
                self.precheck_timeout_seconds * 2.0,
            )
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    last_error = (
                        f"backend {version} exited with code "
                        f"{process.returncode} on port {port}"
                    )
                    break
                try:
                    status = self._backend_status(slot)
                    if (
                        status is not None
                        and status.get("instance_id") == self.instance_id
                        and status.get("version") == version
                    ):
                        return slot
                except OSError:
                    pass
                time.sleep(0.02)
            else:
                last_error = (
                    f"backend {version} did not start on port {port}"
                )
            slot.stop()
            time.sleep(0.05)
        raise RuntimeError(last_error)

    @staticmethod
    def _backend_status(slot: BackendSlot, timeout: float = 0.5) -> dict[str, Any] | None:
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{slot.port}/admin/status",
                timeout=timeout,
            ) as response:
                return json.loads(response.read().decode("utf-8"))
        except (OSError, urllib.error.URLError, json.JSONDecodeError):
            return None

    @staticmethod
    def _backend_health(slot: BackendSlot, timeout: float = 0.5) -> tuple[int, str]:
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{slot.port}/health",
                timeout=timeout,
            ) as response:
                payload = json.loads(response.read().decode("utf-8"))
                return response.status, str(payload.get("code", ""))
        except urllib.error.HTTPError as exc:
            try:
                payload = json.loads(exc.read().decode("utf-8"))
            except Exception:
                payload = {}
            status = exc.code
            exc.close()
            return status, str(payload.get("code", "HEALTH_CHECK_FAILED"))
        except OSError:
            return 0, "BACKEND_UNAVAILABLE"

    def _active_version(self) -> str | None:
        with self._lock:
            return None if self._active is None else self._active.version

    def _candidate_state(self) -> dict[str, Any] | None:
        with self._lock:
            if self._candidate is None:
                return None
            return {
                "version": self._candidate.version,
                "backend_id": self._candidate.backend_id,
            }

    def precheck(
        self,
        target_version: str,
        mode: str = "switch",
    ) -> dict[str, Any]:
        with self._operation_lock:
            active_version = self._active_version()
            if (
                mode == "rollback"
                and self.rollback_restart_refused
                and active_version != self.initial_version
                and target_version == self.initial_version
            ):
                return {
                    "ok": False,
                    "ready": False,
                    "code": "INSTANCE_RESTART_REFUSED",
                    "reason": "configured_restart_refusal",
                    "instance_id": self.instance_id,
                    "active_version": active_version,
                    "target_version": target_version,
                }
            if active_version == target_version:
                return {
                    "ok": True,
                    "ready": True,
                    "already_active": True,
                    "instance_id": self.instance_id,
                    "target_version": target_version,
                }
            with self._lock:
                candidate = self._candidate
            if candidate is not None and candidate.version != target_version:
                with self._lock:
                    self._candidate = None
                candidate.stop()
            with self._lock:
                candidate = self._candidate
            if candidate is None:
                try:
                    candidate = self._start_backend(target_version)
                except Exception as exc:
                    return {
                        "ok": False,
                        "ready": False,
                        "code": "INSTANCE_PRECHECK_FAILED",
                        "reason": "backend_start_failed",
                        "message": str(exc),
                        "instance_id": self.instance_id,
                        "active_version": active_version,
                        "target_version": target_version,
                    }
                with self._lock:
                    self._candidate = candidate
            deadline = time.monotonic() + self.precheck_timeout_seconds
            last_code = ""
            while time.monotonic() < deadline:
                status, code = self._backend_health(candidate, timeout=0.3)
                last_code = code
                if status == 200:
                    return {
                        "ok": True,
                        "ready": True,
                        "already_active": False,
                        "instance_id": self.instance_id,
                        "active_version": active_version,
                        "target_version": target_version,
                        "candidate_backend_id": candidate.backend_id,
                    }
                if status == 503:
                    break
                if candidate.process.poll() is not None:
                    last_code = "backend_process_exited"
                    break
                time.sleep(0.03)
            with self._lock:
                if self._candidate is candidate:
                    self._candidate = None
            candidate.stop()
            return {
                "ok": False,
                "ready": False,
                "code": "INSTANCE_PRECHECK_FAILED",
                "reason": "health_endpoint_unhealthy",
                "health_code": last_code,
                "instance_id": self.instance_id,
                "active_version": active_version,
                "target_version": target_version,
            }

    def discard_candidate(self) -> None:
        with self._operation_lock:
            with self._lock:
                candidate = self._candidate
                self._candidate = None
            if candidate is not None:
                candidate.stop()

    def activate(self, target_version: str) -> dict[str, Any]:
        with self._operation_lock:
            with self._lock:
                active = self._active
                candidate = self._candidate
            if active is None:
                return {
                    "ok": False,
                    "code": "INSTANCE_SWITCH_FAILED",
                    "reason": "no_active_backend",
                    "instance_id": self.instance_id,
                }
            if active.version == target_version:
                return {
                    "ok": True,
                    "switched": True,
                    "already_active": True,
                    "instance_id": self.instance_id,
                    "active_version": target_version,
                }
            if candidate is None or candidate.version != target_version:
                return {
                    "ok": False,
                    "code": "INSTANCE_SWITCH_FAILED",
                    "reason": "candidate_not_prechecked",
                    "instance_id": self.instance_id,
                    "active_version": active.version,
                    "target_version": target_version,
                }
            status, health_code = self._backend_health(candidate, timeout=0.5)
            if status != 200:
                return {
                    "ok": False,
                    "code": "INSTANCE_SWITCH_FAILED",
                    "reason": "candidate_unhealthy_at_activation",
                    "health_code": health_code,
                    "instance_id": self.instance_id,
                    "target_version": target_version,
                }
            with self._lock:
                self._active = candidate
                self._candidate = None
                self._switch_generation += 1
                generation = self._switch_generation
            self._wait_for_drain(active)
            active.stop()
            return {
                "ok": True,
                "switched": True,
                "already_active": False,
                "instance_id": self.instance_id,
                "active_version": target_version,
                "switch_generation": generation,
            }

    def switch(self, target_version: str, mode: str = "switch") -> dict[str, Any]:
        precheck = self.precheck(target_version, mode=mode)
        if not precheck.get("ready"):
            return precheck
        return self.activate(target_version)

    def _wait_for_drain(self, slot: BackendSlot) -> None:
        deadline = time.monotonic() + self.drain_timeout_seconds
        while time.monotonic() < deadline:
            with self._lock:
                if self._inflight.get(slot.backend_id, 0) <= 0:
                    return
            time.sleep(0.01)

    def _acquire_active(self) -> tuple[BackendSlot | None, int]:
        with self._lock:
            active = self._active
            if active is None:
                return None, 0
            backend_id = active.backend_id
            self._inflight[backend_id] = self._inflight.get(backend_id, 0) + 1
            return active, backend_id

    def _release_active(self, backend_id: int) -> None:
        with self._lock:
            current = self._inflight.get(backend_id, 0)
            if current <= 1:
                self._inflight[backend_id] = 0
            else:
                self._inflight[backend_id] = current - 1

    def _record_sample(
        self,
        status: int,
        latency_ms: float,
        version: str,
    ) -> None:
        with self._lock:
            sample_id = self._next_sample_id
            self._next_sample_id += 1
            self._samples.append(
                {
                    "instance_id": self.instance_id,
                    "sample_id": sample_id,
                    "status": int(status),
                    "latency_ms": round(float(latency_ms), 6),
                    "version": version,
                }
            )

    def proxy_settlement(self, path: str) -> tuple[int, dict[str, str], bytes]:
        attempts = 0
        while attempts < 2:
            attempts += 1
            slot, backend_id = self._acquire_active()
            if slot is None:
                self._record_sample(503, 0.0, "unavailable")
                return (
                    503,
                    {"Content-Type": "application/json; charset=utf-8"},
                    _json_bytes(
                        {
                            "ok": False,
                            "code": "INSTANCE_UNAVAILABLE",
                            "instance_id": self.instance_id,
                        }
                    ),
                )
            connection = http.client.HTTPConnection(
                "127.0.0.1",
                slot.port,
                timeout=self.request_timeout_seconds,
            )
            try:
                connection.request("GET", path, headers={"Connection": "close"})
                response = connection.getresponse()
                body = response.read()
                latency = float(
                    response.getheader("X-Simulated-Latency-Ms") or 0.0
                )
                self._record_sample(response.status, latency, slot.version)
                headers = {
                    "Content-Type": response.getheader(
                        "Content-Type", "application/json; charset=utf-8"
                    ),
                    "X-Instance-Version": slot.version,
                    "X-Simulated-Latency-Ms": str(latency),
                }
                return response.status, headers, body
            except (OSError, http.client.HTTPException):
                with self._lock:
                    still_current = (
                        self._active is not None
                        and self._active.backend_id == slot.backend_id
                    )
                if still_current:
                    break
            finally:
                connection.close()
                self._release_active(backend_id)
        self._record_sample(503, 0.0, "unavailable")
        return (
            503,
            {"Content-Type": "application/json; charset=utf-8"},
            _json_bytes(
                {
                    "ok": False,
                    "code": "INSTANCE_UNAVAILABLE",
                    "instance_id": self.instance_id,
                }
            ),
        )

    def health(self) -> tuple[int, dict[str, Any]]:
        with self._lock:
            active = self._active
        if active is None:
            return 503, {
                "ok": False,
                "code": "INSTANCE_UNAVAILABLE",
                "instance_id": self.instance_id,
            }
        status, code = self._backend_health(active, timeout=0.3)
        if status == 200:
            return 200, {
                "ok": True,
                "instance_id": self.instance_id,
                "version": active.version,
            }
        return 503, {
            "ok": False,
            "code": "HEALTH_CHECK_FAILED",
            "health_code": code,
            "instance_id": self.instance_id,
            "version": active.version,
        }

    def status(self) -> dict[str, Any]:
        with self._lock:
            active = self._active
            candidate = self._candidate
            return {
                "ok": True,
                "instance_id": self.instance_id,
                "group": self.group,
                "initial_version": self.initial_version,
                "active_version": None if active is None else active.version,
                "switched": (
                    active is not None
                    and active.version != self.initial_version
                ),
                "candidate": None if candidate is None else candidate.version,
                "switch_generation": self._switch_generation,
                "rollback_restart_refused": self.rollback_restart_refused,
                "sample_count": len(self._samples),
                "uptime_seconds": round(time.monotonic() - self._started_monotonic, 3),
            }

    def samples_since(self, since: int) -> list[dict[str, Any]]:
        with self._lock:
            return [
                dict(sample)
                for sample in self._samples
                if int(sample["sample_id"]) > since
            ]


class SupervisorHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "SettlementInstance/1.0"

    @property
    def supervisor(self) -> InstanceSupervisor:
        return self.server.supervisor  # type: ignore[attr-defined]

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def _send(
        self,
        status: int,
        body: bytes,
        headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        merged = {
            "Content-Type": "application/json; charset=utf-8",
            "Content-Length": str(len(body)),
            "Connection": "close",
        }
        if headers:
            merged.update(headers)
        for key, value in merged.items():
            if key == "Content-Length":
                continue
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, status: int, value: dict[str, Any]) -> None:
        self._send(status, _json_bytes(value))

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path == "/settle":
            status, headers, body = self.supervisor.proxy_settlement(
                parsed.path + (f"?{parsed.query}" if parsed.query else "")
            )
            self._send(status, body, headers)
            return
        if parsed.path == "/health":
            status, payload = self.supervisor.health()
            self._send_json(status, payload)
            return
        if parsed.path == "/admin/status":
            self._send_json(200, self.supervisor.status())
            return
        if parsed.path == "/admin/stats":
            query = parse_qs(parsed.query)
            try:
                since = int(query.get("since", ["0"])[0])
            except ValueError:
                self._send_json(
                    400,
                    {
                        "ok": False,
                        "code": "INVALID_SINCE",
                        "message": "since must be an integer",
                    },
                )
                return
            samples = self.supervisor.samples_since(since)
            self._send_json(
                200,
                {
                    "ok": True,
                    "instance_id": self.supervisor.instance_id,
                    "samples": samples,
                },
            )
            return
        self._send_json(404, {"ok": False, "code": "NOT_FOUND"})

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8")) if raw else {}
        except json.JSONDecodeError:
            self._send_json(
                400,
                {"ok": False, "code": "INVALID_JSON"},
            )
            return
        if parsed.path == "/admin/precheck":
            target = payload.get("target_version")
            if not isinstance(target, str) or not target:
                self._send_json(
                    400,
                    {
                        "ok": False,
                        "code": "INVALID_TARGET_VERSION",
                    },
                )
                return
            result = self.supervisor.precheck(
                target,
                mode=str(payload.get("mode", "switch")),
            )
            self._send_json(200 if result.get("ok") else 409, result)
            return
        if parsed.path == "/admin/activate":
            target = payload.get("target_version")
            if not isinstance(target, str) or not target:
                self._send_json(
                    400,
                    {
                        "ok": False,
                        "code": "INVALID_TARGET_VERSION",
                    },
                )
                return
            result = self.supervisor.activate(target)
            self._send_json(200 if result.get("ok") else 409, result)
            return
        if parsed.path == "/admin/discard":
            self.supervisor.discard_candidate()
            self._send_json(200, {"ok": True})
            return
        if parsed.path == "/admin/switch":
            target = payload.get("target_version")
            if not isinstance(target, str) or not target:
                self._send_json(
                    400,
                    {"ok": False, "code": "INVALID_TARGET_VERSION"},
                )
                return
            result = self.supervisor.switch(
                target,
                mode=str(payload.get("mode", "switch")),
            )
            self._send_json(200 if result.get("ok") else 409, result)
            return
        if parsed.path == "/admin/shutdown":
            self._send_json(200, {"ok": True})
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        self._send_json(404, {"ok": False, "code": "NOT_FOUND"})


class SupervisorServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128

    def __init__(
        self,
        address: tuple[str, int],
        supervisor: InstanceSupervisor,
    ) -> None:
        super().__init__(address, SupervisorHandler)
        self.supervisor = supervisor


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Settlement instance supervisor")
    parser.add_argument("--instance-id", required=True)
    parser.add_argument("--group", required=True)
    parser.add_argument("--initial-version", required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", required=True, type=int)
    parser.add_argument("--backend-port-start", required=True, type=int)
    parser.add_argument("--backend-port-end", required=True, type=int)
    parser.add_argument("--faults-json", default="{}")
    parser.add_argument("--precheck-timeout", type=float, default=2.0)
    parser.add_argument("--drain-timeout", type=float, default=5.0)
    parser.add_argument("--request-timeout", type=float, default=10.0)
    args = parser.parse_args(argv)
    faults = json.loads(args.faults_json)
    supervisor = InstanceSupervisor(
        instance_id=args.instance_id,
        group=args.group,
        initial_version=args.initial_version,
        host=args.host,
        port=args.port,
        backend_port_range=(args.backend_port_start, args.backend_port_end),
        faults=faults,
        precheck_timeout_seconds=args.precheck_timeout,
        drain_timeout_seconds=args.drain_timeout,
        request_timeout_seconds=args.request_timeout,
    )
    try:
        supervisor.serve_forever()
    finally:
        supervisor.close()
        supervisor.server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
