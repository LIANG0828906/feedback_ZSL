from __future__ import annotations

import json
import socket
import time
from pathlib import Path

import pytest

from edge_sync.config import load_config
from edge_sync.supervisor import bring_up_stack
from edge_sync.workload import build_manifest


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _write_config(tmp_path: Path, name: str, overrides: dict | None = None) -> Path:
    base = json.loads(Path("configs/smoke.json").read_text(encoding="utf-8"))
    base["target"]["port"] = _free_port()
    base["service"]["port"] = _free_port()
    base["db_path"] = str(tmp_path / f"{name}.db")
    base["target_db_path"] = str(tmp_path / f"{name}-target.db")
    base["report_path"] = str(tmp_path / f"{name}-report.json")
    for index, egress in enumerate(base["egresses"]):
        egress["listen_port"] = _free_port()
    if overrides:
        _deep_update(base, overrides)
    path = tmp_path / f"{name}.json"
    path.write_text(json.dumps(base, indent=2), encoding="utf-8")
    return path


def _deep_update(target: dict, updates: dict) -> None:
    for key, value in updates.items():
        if isinstance(value, dict) and isinstance(target.get(key), dict):
            _deep_update(target[key], value)
        else:
            target[key] = value


@pytest.fixture(scope="session")
def workspace(tmp_path_factory) -> Path:
    return tmp_path_factory.mktemp("edge_sync")


@pytest.fixture()
def small_config(workspace, request) -> Path:
    overrides = getattr(request, "param", None)
    return _write_config(workspace, request.node.name, overrides)


@pytest.fixture()
def running_stack(small_config):
    config = load_config(small_config)
    stack = bring_up_stack(config)
    try:
        manifest = build_manifest(config)
        for task in manifest:
            stack.target_store.register_task(task.task_id, task.shard_count, task.total_bytes)
        yield config, stack, manifest
    finally:
        stack.shutdown()


def wait_for(url: str, timeout_s: float = 10.0) -> dict:
    import urllib.request

    deadline = time.monotonic() + timeout_s
    last_error = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                return json.loads(response.read().decode("utf-8"))
        except OSError as exc:
            last_error = exc
            time.sleep(0.1)
    raise AssertionError(f"{url} never became ready: {last_error}")
