from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from typing import Any, Dict, List, Optional

from mlp_ps.errors import (
    E_LAYOUT_CONFLICT,
    E_PARAM_SHAPE,
    E_TRAIN_STALL,
    exit_code_for,
)
from mlp_ps.protocol import Frame, sync_read_frame, sync_send_frame


REPO = Path(__file__).resolve().parents[1]


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _write_config(
    root: Path,
    *,
    total_steps: int,
    fault: Optional[Dict[str, Any]] = None,
) -> Path:
    raw = json.loads((REPO / "config" / "demo.json").read_text(encoding="utf-8"))
    raw["run"]["port"] = _free_port()
    raw["run"]["run_dir"] = str(root / "run")
    raw["training"]["total_steps"] = total_steps
    raw["training"]["sample_count"] = total_steps * raw["training"]["batch_size"]
    raw["training"]["eval_samples"] = 64
    raw["checkpoint"]["steps"] = [0, total_steps]
    raw["checkpoint"]["padding_bytes"] = 4096
    raw["runtime"]["step_pace_ms"] = 5
    raw["network"]["registration_timeout_s"] = 30.0
    raw["network"]["step_timeout_s"] = 3.0
    if fault is None:
        raw["fault"] = {"enabled": False}
    else:
        raw["fault"] = fault
    path = root / "config.json"
    path.write_text(json.dumps(raw, indent=2), encoding="utf-8")
    return path


def _env() -> Dict[str, str]:
    result = os.environ.copy()
    result.update(
        {
            "OMP_NUM_THREADS": "1",
            "MKL_NUM_THREADS": "1",
            "OPENBLAS_NUM_THREADS": "1",
            "PYTHONUNBUFFERED": "1",
        }
    )
    return result


def _start_server(config_path: Path, log_path: Path) -> subprocess.Popen:
    handle = log_path.open("w", encoding="utf-8")
    process = subprocess.Popen(
        [sys.executable, "-m", "mlp_ps.server", "--config", str(config_path)],
        cwd=str(REPO),
        env=_env(),
        stdout=handle,
        stderr=subprocess.STDOUT,
    )
    process._test_log_handle = handle  # type: ignore[attr-defined]
    return process


def _wait_ready(config_path: Path, process: subprocess.Popen, timeout_s: float = 20.0) -> None:
    config = json.loads(config_path.read_text(encoding="utf-8"))
    ready = Path(config["run"]["run_dir"]) / "server_ready.json"
    deadline = time.perf_counter() + timeout_s
    while not ready.exists():
        if process.poll() is not None:
            raise AssertionError(f"server exited before ready: {process.returncode}")
        if time.perf_counter() >= deadline:
            raise AssertionError("timed out waiting for server ready")
        time.sleep(0.02)


def _stop(process: subprocess.Popen) -> None:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=3.0)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3.0)
    handle = getattr(process, "_test_log_handle", None)
    if handle is not None:
        handle.close()


class ErrorChainIntegrationTests(unittest.TestCase):
    def test_duplicate_shard_is_broadcast_and_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_path = _write_config(root, total_steps=2)
            server = _start_server(config_path, root / "server.log")
            sockets: List[socket.socket] = []
            try:
                _wait_ready(config_path, server)
                config = json.loads(config_path.read_text(encoding="utf-8"))
                for worker_id in ("duplicate-a", "duplicate-b"):
                    sock = socket.create_connection(
                        (config["run"]["host"], config["run"]["port"]), timeout=3.0
                    )
                    sock.settimeout(5.0)
                    sockets.append(sock)
                    sync_send_frame(
                        sock,
                        {
                            "type": "REGISTER",
                            "protocol": 1,
                            "worker_id": worker_id,
                            "shard_id": 0,
                            "role": "initial",
                        },
                    )
                    if worker_id == "duplicate-a":
                        first = sync_read_frame(sock, 1024 * 1024)
                        self.assertEqual(first.message_type, "REGISTERED")

                saw_abort = False
                for sock in sockets:
                    try:
                        frame = sync_read_frame(sock, 1024 * 1024)
                    except (ConnectionError, OSError, socket.timeout):
                        continue
                    if frame.message_type == "ABORT":
                        self.assertEqual(frame.header["code"], E_LAYOUT_CONFLICT)
                        saw_abort = True
                self.assertTrue(saw_abort)
                self.assertEqual(
                    server.wait(timeout=10.0),
                    exit_code_for(E_LAYOUT_CONFLICT),
                )
                result = json.loads(
                    (Path(config["run"]["run_dir"]) / "job_result.json").read_text(
                        encoding="utf-8"
                    )
                )
                self.assertEqual(result["code"], E_LAYOUT_CONFLICT)
            finally:
                for sock in sockets:
                    sock.close()
                _stop(server)

    def test_parameter_shape_error_is_broadcast(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_path = _write_config(root, total_steps=3)
            config = json.loads(config_path.read_text(encoding="utf-8"))
            server = _start_server(config_path, root / "server.log")
            workers: Dict[str, subprocess.Popen] = {}
            handles = []
            try:
                _wait_ready(config_path, server)
                for shard_id in range(4):
                    handle = (root / f"worker-{shard_id}.log").open("w", encoding="utf-8")
                    handles.append(handle)
                    command = [
                        sys.executable,
                        "-m",
                        "mlp_ps.worker",
                        "--config",
                        str(config_path),
                        "--worker-id",
                        f"worker-{shard_id}",
                        "--shard-id",
                        str(shard_id),
                    ]
                    if shard_id == 1:
                        command.append("--inject-bad-gradient-shape")
                    workers[str(shard_id)] = subprocess.Popen(
                        command,
                        cwd=str(REPO),
                        env=_env(),
                        stdout=handle,
                        stderr=subprocess.STDOUT,
                    )
                self.assertEqual(server.wait(timeout=20.0), exit_code_for(E_PARAM_SHAPE))
                for shard_id in (0, 2, 3):
                    workers[str(shard_id)].wait(timeout=5.0)
                    result_path = (
                        Path(config["run"]["run_dir"])
                        / f"worker_result_worker-{shard_id}.json"
                    )
                    result = json.loads(result_path.read_text(encoding="utf-8"))
                    self.assertEqual(result["status"], "ABORTED")
                    self.assertEqual(result["code"], E_PARAM_SHAPE)
            finally:
                for process in workers.values():
                    _stop(process)
                _stop(server)
                for handle in handles:
                    handle.close()

    def test_disconnect_without_takeover_hits_train_stall(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config_path = _write_config(
                root,
                total_steps=12,
                fault={
                    "enabled": True,
                    "shard_id": 2,
                    "after_step": 1,
                    "outage_steps": 1,
                    "stall_tolerance_steps": 3,
                    "replacement_id": "worker-2-recovery",
                    "replacement_delay_ms": 0,
                },
            )
            config = json.loads(config_path.read_text(encoding="utf-8"))
            server = _start_server(config_path, root / "server.log")
            workers: Dict[str, subprocess.Popen] = {}
            handles = []
            try:
                _wait_ready(config_path, server)
                for shard_id in range(4):
                    handle = (root / f"worker-{shard_id}.log").open("w", encoding="utf-8")
                    handles.append(handle)
                    workers[str(shard_id)] = subprocess.Popen(
                        [
                            sys.executable,
                            "-m",
                            "mlp_ps.worker",
                            "--config",
                            str(config_path),
                            "--worker-id",
                            f"worker-{shard_id}",
                            "--shard-id",
                            str(shard_id),
                        ],
                        cwd=str(REPO),
                        env=_env(),
                        stdout=handle,
                        stderr=subprocess.STDOUT,
                    )
                self.assertEqual(server.wait(timeout=20.0), exit_code_for(E_TRAIN_STALL))
                for shard_id in (0, 1, 3):
                    workers[str(shard_id)].wait(timeout=5.0)
                    result_path = (
                        Path(config["run"]["run_dir"])
                        / f"worker_result_worker-{shard_id}.json"
                    )
                    result = json.loads(result_path.read_text(encoding="utf-8"))
                    self.assertEqual(result["status"], "ABORTED")
                    self.assertEqual(result["code"], E_TRAIN_STALL)
                failed = json.loads(
                    (Path(config["run"]["run_dir"]) / "worker_result_worker-2.json").read_text(
                        encoding="utf-8"
                    )
                )
                self.assertEqual(failed["status"], "DISCONNECTED")
            finally:
                for process in workers.values():
                    _stop(process)
                _stop(server)
                for handle in handles:
                    handle.close()


if __name__ == "__main__":
    unittest.main()
