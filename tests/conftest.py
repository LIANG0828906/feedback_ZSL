from __future__ import annotations

import copy
import json
import os
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import pytest


ROOT = Path(__file__).resolve().parents[1]


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def http_json(
    method: str,
    url: str,
    *,
    body: dict[str, Any] | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 5.0,
) -> tuple[int, dict[str, str], Any, float]:
    payload = (
        json.dumps(body, separators=(",", ":")).encode("utf-8")
        if body is not None
        else None
    )
    request_headers = {"Accept": "application/json", "Connection": "close"}
    request_headers.update(headers or {})
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        url,
        data=payload,
        headers=request_headers,
        method=method,
    )
    started = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            elapsed = (time.monotonic() - started) * 1000.0
            try:
                parsed = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                parsed = raw.decode("utf-8", errors="replace")
            return response.status, dict(response.headers.items()), parsed, elapsed
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        elapsed = (time.monotonic() - started) * 1000.0
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = raw.decode("utf-8", errors="replace")
        return exc.code, dict(exc.headers.items()), parsed, elapsed


def wait_health(base_url: str, timeout: float = 20.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            status, _, payload, _ = http_json(
                "GET", base_url + "/healthz", timeout=0.5
            )
            if status == 200:
                return payload
        except Exception as exc:
            last = exc
        time.sleep(0.05)
    raise RuntimeError(f"service failed to start: {last}")


@dataclass
class ServiceHandle:
    base_url: str
    config_path: Path
    database_path: Path
    process: subprocess.Popen
    run_id: str
    admin_token: str
    log_path: Path

    def get(
        self,
        path: str,
        *,
        tenant: str | None = None,
        timeout: float = 5.0,
    ) -> tuple[int, dict[str, str], dict[str, Any], float]:
        headers = {"X-Tenant-Id": tenant} if tenant else {}
        return http_json(
            "GET",
            self.base_url + path,
            headers=headers,
            timeout=timeout,
        )

    def admin(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
    ) -> tuple[int, dict[str, str], dict[str, Any], float]:
        return http_json(
            method,
            self.base_url + path,
            body=body,
            headers={"X-Admin-Token": self.admin_token},
        )

    def status(self) -> dict[str, Any]:
        status, _, payload, _ = self.admin("GET", "/admin/status")
        assert status == 200, payload
        return payload


@pytest.fixture
def service_factory(tmp_path: Path):
    processes: list[ServiceHandle] = []
    source_config = json.loads(
        (ROOT / "config" / "service.json").read_text(encoding="utf-8")
    )
    counter = 0

    def start(
        mutate: Callable[[dict[str, Any]], None] | None = None,
    ) -> ServiceHandle:
        nonlocal counter
        counter += 1
        directory = tmp_path / f"service-{counter}"
        directory.mkdir(parents=True)
        port = free_port()
        config = copy.deepcopy(source_config)
        config["service"]["port"] = port
        config["service"]["admin_token"] = "test-admin-token"
        config["service"]["business_threads"] = 8
        config["database"]["path"] = str(directory / "orders.db")
        config["database"]["orders_per_tenant"] = 20
        config["database"]["init"] = True
        config["audit"]["path"] = str(directory / "orders.db")
        config["audit"]["run_id"] = None
        config["sli_evaluation_interval_ms"] = 50
        for tenant in config["tenants"]:
            tenant["rate_per_second"] = 100
            tenant["burst"] = 50
            tenant["max_inflight"] = 8
        if mutate is not None:
            mutate(config)
        config_path = directory / "service.json"
        config_path.write_text(
            json.dumps(config, indent=2), encoding="utf-8"
        )
        log_path = directory / "service.log"
        log_handle = open(log_path, "w", encoding="utf-8")
        env = os.environ.copy()
        env["ORDER_GUARD_RUN_ID"] = f"pytest-{counter}-{time.time_ns()}"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "order_guard",
                "--config",
                str(config_path),
                "--log-level",
                "warning",
            ],
            cwd=str(ROOT),
            env=env,
            stdout=log_handle,
            stderr=subprocess.STDOUT,
        )
        log_handle.close()
        base_url = f"http://127.0.0.1:{port}"
        try:
            health = wait_health(base_url)
        except Exception:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
            raise
        handle = ServiceHandle(
            base_url=base_url,
            config_path=config_path,
            database_path=Path(config["database"]["path"]),
            process=process,
            run_id=str(health["run_id"]),
            admin_token="test-admin-token",
            log_path=log_path,
        )
        processes.append(handle)
        return handle

    yield start

    for handle in processes:
        if handle.process.poll() is None:
            handle.process.terminate()
        try:
            handle.process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            handle.process.kill()
            handle.process.wait(timeout=5)
