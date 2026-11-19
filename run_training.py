from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from generate_data import generate
from tools.inject_duplicate import inject_duplicate
from tools.inject_stale import inject_stale
from tools.kill_shard import kill_shard
from tools.pause_process import change_pause
from train.common import ROOT, atomic_write_json, load_config, resolve_path
from train.db import connect, database_path, last_applied_step
from train.verify import verify


def _environment() -> dict[str, str]:
    environment = os.environ.copy()
    environment.update(
        {
            "OMP_NUM_THREADS": "1",
            "OPENBLAS_NUM_THREADS": "1",
            "MKL_NUM_THREADS": "1",
            "NUMEXPR_NUM_THREADS": "1",
            "PYTHONUNBUFFERED": "1",
            "PYTHONDONTWRITEBYTECODE": "1",
        }
    )
    return environment


class ManagedProcess:
    def __init__(self, name: str, command: list[str], log_path: Path):
        self.name = name
        self.command = command
        self.log_path = log_path
        self.handle = None
        self.process: subprocess.Popen | None = None
        self.start()

    def start(self) -> None:
        self.log_path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = open(self.log_path, "ab", buffering=0)
        kwargs: dict[str, Any] = {
            "cwd": str(ROOT),
            "stdout": self.handle,
            "stderr": subprocess.STDOUT,
            "env": _environment(),
        }
        if os.name == "nt":
            kwargs["creationflags"] = getattr(
                subprocess, "CREATE_NEW_PROCESS_GROUP", 0
            )
        self.process = subprocess.Popen(self.command, **kwargs)

    def close_log(self) -> None:
        if self.handle is not None:
            self.handle.close()
            self.handle = None

    def terminate(self, timeout: float = 5.0) -> None:
        if self.process is None:
            return
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5.0)
        self.close_log()


def _http_json(config: dict[str, Any], path: str) -> dict[str, Any]:
    coordinator = config["coordinator"]
    url = f"http://{coordinator['host']}:{coordinator['status_port']}{path}"
    with urllib.request.urlopen(url, timeout=2.0) as response:
        return json.loads(response.read().decode("utf-8"))


def wait_http(config: dict[str, Any], timeout: float = 20.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            payload = _http_json(config, "/healthz")
            if payload.get("status") == "ok":
                return
        except (OSError, urllib.error.URLError, json.JSONDecodeError):
            pass
        time.sleep(0.1)
    raise TimeoutError("coordinator status HTTP endpoint did not become ready")


def wait_for_shards(config: dict[str, Any], timeout: float = 20.0) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    expected = {item["shard_id"] for item in config["shards"]}
    last_status: dict[str, Any] = {}
    while time.monotonic() < deadline:
        try:
            last_status = _http_json(config, "/status")
            shards = {row["shard_id"]: row for row in last_status.get("shards", [])}
            if set(shards) == expected and all(
                row["status"] == "active" for row in shards.values()
            ):
                return last_status
        except (OSError, urllib.error.URLError, json.JSONDecodeError):
            pass
        time.sleep(0.1)
    raise TimeoutError(f"parameter shards did not register: {json.dumps(last_status)[:500]}")


def _write_pid(name: str, pid: int) -> None:
    directory = resolve_path("run/pids")
    directory.mkdir(parents=True, exist_ok=True)
    (directory / f"{name}.pid").write_text(str(pid), encoding="ascii")


def _fresh_state() -> None:
    database = resolve_path("run/training_state.sqlite3")
    for suffix in ("", "-wal", "-shm"):
        path = Path(str(database) + suffix)
        if path.exists():
            path.unlink()
    for directory_name in ("run/logs", "run/worker_reports", "run/pids"):
        directory = resolve_path(directory_name)
        if directory.exists():
            for child in directory.glob("*"):
                if child.is_file():
                    child.unlink()


def _read_db_version(config: dict[str, Any]) -> int:
    connection = connect(database_path(config), timeout=2.0)
    try:
        return last_applied_step(connection)
    finally:
        connection.close()


def _print_log_tail(path: Path, lines: int = 30) -> None:
    if not path.exists():
        return
    content = path.read_text(encoding="utf-8", errors="replace").splitlines()
    print(f"--- {path.name} (tail) ---", flush=True)
    print("\n".join(content[-lines:]), flush=True)


def run(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    if args.fresh:
        _fresh_state()
    metadata_path = resolve_path(config["data"]["metadata"])
    if args.regenerate_data or not metadata_path.exists():
        generate(args.config, force=args.regenerate_data)

    python = sys.executable
    logs = resolve_path("run/logs")
    logs.mkdir(parents=True, exist_ok=True)
    processes: dict[str, ManagedProcess] = {}
    training_started = time.perf_counter()
    workers_completed_at: float | None = None
    fault_state = {
        "duplicate": False,
        "stale": False,
        "kill": False,
        "restart": False,
    }
    fault_events: list[dict[str, Any]] = []

    def start_coordinator() -> ManagedProcess:
        managed = ManagedProcess(
            "coordinator",
            [python, "-m", "train.coordinator", "--config", args.config],
            logs / "coordinator.log",
        )
        processes["coordinator"] = managed
        _write_pid("coordinator", managed.process.pid)
        wait_http(config)
        return managed

    def start_shard(shard_id: str) -> ManagedProcess:
        managed = ManagedProcess(
            shard_id,
            [
                python,
                "-m",
                "ps.shard",
                "--config",
                args.config,
                "--shard-id",
                shard_id,
            ],
            logs / f"{shard_id}.log",
        )
        processes[shard_id] = managed
        _write_pid(shard_id, managed.process.pid)
        return managed

    try:
        start_coordinator()
        for shard in config["shards"]:
            start_shard(shard["shard_id"])
        wait_for_shards(config)

        worker_processes: dict[str, ManagedProcess] = {}
        for worker in config["workers"]:
            worker_id = worker["worker_id"]
            managed = ManagedProcess(
                worker_id,
                [
                    python,
                    "-m",
                    "train.worker",
                    "--config",
                    args.config,
                    "--worker-id",
                    worker_id,
                ],
                logs / f"{worker_id}.log",
            )
            worker_processes[worker_id] = managed
            processes[worker_id] = managed
            _write_pid(worker_id, managed.process.pid)

        faults = config["faults"]
        deadline = time.monotonic() + args.timeout_seconds
        while any(
            managed.process.poll() is None
            for managed in worker_processes.values()
        ):
            if time.monotonic() > deadline:
                raise TimeoutError("worker training exceeded the configured timeout")
            try:
                version = _read_db_version(config)
            except Exception:
                version = 0
            if args.with_faults:
                if (
                    not fault_state["duplicate"]
                    and version >= int(faults["duplicate_at_step"])
                ):
                    result = inject_duplicate(args.config, step=version)
                    fault_state["duplicate"] = True
                    fault_events.append({"type": "duplicate", **result})
                if (
                    not fault_state["stale"]
                    and version >= int(faults["stale_at_step"])
                ):
                    stale_worker = "worker-1"
                    change_pause(stale_worker, resume=False)
                    try:
                        results = inject_stale(
                            args.config,
                            stale_worker,
                            count=int(faults["stale_injections"]),
                        )
                    finally:
                        change_pause(stale_worker, resume=True)
                    fault_state["stale"] = True
                    fault_events.append(
                        {
                            "type": "stale",
                            "paused_worker": stale_worker,
                            "results": results,
                        }
                    )
                if (
                    not fault_state["kill"]
                    and version >= int(faults["kill_at_step"])
                ):
                    result = kill_shard(
                        args.config, str(faults["kill_shard"])
                    )
                    fault_state["kill"] = True
                    fault_events.append({"type": "kill_shard", **result})
            if (
                args.restart_coordinator_at_step
                and not fault_state["restart"]
                and version >= int(args.restart_coordinator_at_step)
            ):
                old = processes["coordinator"]
                old.process.kill()
                old.process.wait(timeout=5.0)
                old.close_log()
                fault_state["restart"] = True
                restart_started = time.perf_counter()
                start_coordinator()
                fault_events.append(
                    {
                        "restart_seconds": round(
                            time.perf_counter() - restart_started, 3
                        ),
                        "type": "coordinator_restart",
                    }
                )
            time.sleep(0.02)

        workers_completed_at = time.perf_counter()
        for worker_id, managed in worker_processes.items():
            managed.process.wait(timeout=5.0)
            if managed.process.returncode != 0:
                _print_log_tail(managed.log_path)
                raise RuntimeError(
                    f"{worker_id} exited with code {managed.process.returncode}"
                )
            managed.close_log()
        e2e_seconds = workers_completed_at - training_started
        time.sleep(0.3)
        verification = verify(args.config, e2e_seconds=round(e2e_seconds, 3))
        report = {
            "e2e_seconds": round(e2e_seconds, 3),
            "fault_events": fault_events,
            "status": verification["status"],
            "verification_summary": verification["summary"],
            "checks": verification["checks"],
        }
        atomic_write_json(resolve_path("reports/run_report.json"), report)
        print(json.dumps(report, indent=2, sort_keys=True), flush=True)
        return 0 if verification["status"] == "pass" else 1
    except Exception as exc:
        print(f"run failed: {type(exc).__name__}: {exc}", file=sys.stderr, flush=True)
        for name, managed in processes.items():
            if managed.log_path.exists():
                _print_log_tail(managed.log_path)
        atomic_write_json(
            resolve_path("reports/run_report.json"),
            {
                "error": f"{type(exc).__name__}: {exc}",
                "fault_events": fault_events,
                "status": "error",
            },
        )
        return 1
    finally:
        for managed in reversed(list(processes.values())):
            if managed.process is not None and managed.process.poll() is None:
                managed.terminate()
        for managed in processes.values():
            managed.close_log()


def main() -> int:
    parser = argparse.ArgumentParser(description="Run end-to-end training and verification")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--fresh", action="store_true")
    parser.add_argument("--with-faults", action="store_true")
    parser.add_argument("--restart-coordinator-at-step", type=int, default=None)
    parser.add_argument("--regenerate-data", action="store_true")
    parser.add_argument("--timeout-seconds", type=float, default=480.0)
    args = parser.parse_args()
    return run(args)


if __name__ == "__main__":
    raise SystemExit(main())
