from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any
from urllib.request import urlopen

import grpc

from .config import load_router_config
from .modelgen import generate_models
from .pb import inference_pb2, inference_pb2_grpc


class StackError(RuntimeError):
    pass


def _wait_until(predicate, timeout_s: float, interval_s: float = 0.05) -> bool:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        try:
            if predicate():
                return True
        except Exception:
            pass
        time.sleep(interval_s)
    return False


class LocalStack:
    def __init__(self, root: str | Path, config_path: str | Path) -> None:
        self.root = Path(root).resolve()
        self.config_path = Path(config_path).resolve()
        self.runtime_dir = self.root / "runtime"
        self.log_dir = self.runtime_dir / "logs"
        self.worker_config_dir = self.runtime_dir / "workers"
        self.log_dir.mkdir(parents=True, exist_ok=True)
        self.worker_config_dir.mkdir(parents=True, exist_ok=True)
        self._processes: dict[str, subprocess.Popen] = {}
        self._log_handles: dict[str, Any] = {}
        self.router_process: subprocess.Popen | None = None
        self.config = load_router_config(self.config_path, inspect_models=False)
        self.worker_configs: dict[str, dict[str, Any]] = {}
        self._prepare_models_and_configs()

    def _prepare_models_and_configs(self) -> None:
        model_dirs = {Path(replica.model_path).parent for replica in self.config.shards}
        for model_dir in model_dirs:
            if not all(Path(replica.model_path).is_file() for replica in self.config.shards):
                generate_models(model_dir)
        for replica in self.config.shards:
            config = {
                "layer": replica.layer,
                "replica_group": replica.replica_group,
                "replica_id": replica.replica_id,
                "model_path": replica.model_path,
                "listen_address": replica.listen_address,
                "input_dim": replica.input_dim,
                "output_dim": replica.output_dim,
                "threads": 4,
            }
            config_path = self.worker_config_dir / f"{replica.replica_id}.json"
            config_path.write_text(
                json.dumps(config, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            self.worker_configs[replica.replica_id] = {
                **config,
                "config_path": str(config_path),
            }
        # Reload now that any missing models have been generated.
        self.config = load_router_config(self.config_path, inspect_models=True)

    def _env(self) -> dict[str, str]:
        env = dict(os.environ)
        current = env.get("PYTHONPATH", "")
        env["PYTHONPATH"] = (
            f"{self.root}{os.pathsep}{current}" if current else str(self.root)
        )
        env["PYTHONUNBUFFERED"] = "1"
        return env

    def _open_log(self, name: str):
        path = self.log_dir / f"{name}.log"
        handle = path.open("ab", buffering=0)
        self._log_handles[name] = handle
        return handle

    def start_worker(self, replica_id: str) -> subprocess.Popen:
        existing = self._processes.get(replica_id)
        if existing is not None and existing.poll() is None:
            return existing
        config = self.worker_configs[replica_id]
        log = self._open_log(f"worker-{replica_id}")
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "inference_router.worker",
                "--config",
                config["config_path"],
            ],
            cwd=self.root,
            env=self._env(),
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        self._processes[replica_id] = process
        return process

    def start_all_workers(self) -> None:
        for replica in self.config.shards:
            self.start_worker(replica.replica_id)
        for replica in self.config.shards:
            if not self.wait_worker_ready(replica.listen_address, replica.replica_id, 10.0):
                raise StackError(f"worker {replica.replica_id} did not become ready")

    def wait_worker_ready(self, address: str, replica_id: str, timeout_s: float) -> bool:
        def ready() -> bool:
            with grpc.insecure_channel(address) as channel:
                reply = inference_pb2_grpc.WorkerInferenceStub(channel).Health(
                    inference_pb2.HealthRequest(), timeout=0.5
                )
                return bool(reply.ok) and reply.replica_id == replica_id

        return _wait_until(ready, timeout_s)

    def stop_worker(self, replica_id: str) -> None:
        process = self._processes.pop(replica_id, None)
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)

    def stop_workers(self, replica_ids: list[str] | None = None) -> None:
        for replica_id in replica_ids or list(self._processes):
            self.stop_worker(replica_id)

    def fresh_database(self) -> None:
        db = Path(self.config.service.db_path)
        for suffix in ("", "-wal", "-shm"):
            try:
                Path(str(db) + suffix).unlink()
            except FileNotFoundError:
                pass

    def start_router(self, *, fresh_db: bool = False) -> subprocess.Popen:
        if self.router_process is not None and self.router_process.poll() is None:
            return self.router_process
        if fresh_db:
            self.fresh_database()
        log = self._open_log("router")
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "inference_router.router",
                "--config",
                str(self.config_path),
            ],
            cwd=self.root,
            env=self._env(),
            stdout=log,
            stderr=subprocess.STDOUT,
        )
        self.router_process = process
        if not self.wait_router_ready(15.0):
            raise StackError("router did not become ready")
        return process

    def wait_router_ready(self, timeout_s: float) -> bool:
        host, port_text = self.config.service.http.rsplit(":", 1)
        url = f"http://{host}:{int(port_text)}/healthz"

        def ready() -> bool:
            with urlopen(url, timeout=0.5) as response:
                return response.status == 200

        return _wait_until(ready, timeout_s)

    def stop_router(self) -> None:
        process = self.router_process
        self.router_process = None
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)

    def stop_all(self) -> None:
        self.stop_router()
        self.stop_workers()
        for handle in self._log_handles.values():
            try:
                handle.close()
            except Exception:
                pass
        self._log_handles.clear()

    def process_table(self) -> dict[str, int]:
        table = {
            replica_id: process.pid
            for replica_id, process in self._processes.items()
            if process.poll() is None
        }
        if self.router_process is not None and self.router_process.poll() is None:
            table["router"] = self.router_process.pid
        return table
