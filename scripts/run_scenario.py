#!/usr/bin/env python3
from __future__ import annotations

import argparse
import ctypes
import json
import os
import threading
import time
from pathlib import Path
from typing import Any
from urllib.error import URLError
from urllib.request import urlopen

import grpc
import numpy as np

ROOT = Path(__file__).resolve().parents[1]
import sys

sys.path.insert(0, str(ROOT))

from inference_router.pb import inference_pb2, inference_pb2_grpc  # noqa: E402
from inference_router.stack import LocalStack  # noqa: E402
from inference_router.store import RequestStore  # noqa: E402
from scripts.fault_inject import set_delay  # noqa: E402
from scripts.load_test import run_load  # noqa: E402


def _set_cpu_affinity() -> list[int]:
    if hasattr(os, "sched_setaffinity"):
        available = sorted(os.sched_getaffinity(0))
        selected = available[:2]
        os.sched_setaffinity(0, set(selected))
        return selected
    if os.name == "nt":
        process = ctypes.windll.kernel32.GetCurrentProcess()
        process_mask = ctypes.c_size_t()
        system_mask = ctypes.c_size_t()
        if ctypes.windll.kernel32.GetProcessAffinityMask(
            process, ctypes.byref(process_mask), ctypes.byref(system_mask)
        ):
            available = [
                bit
                for bit in range(64)
                if process_mask.value & (1 << bit)
            ]
            selected = available[:2]
            mask = sum(1 << bit for bit in selected)
            if selected and ctypes.windll.kernel32.SetProcessAffinityMask(process, mask):
                return selected
    return []


def _read_rss_bytes(pid: int) -> int:
    if pid <= 0:
        return 0
    if os.name == "nt":
        class ProcessMemoryCounters(ctypes.Structure):
            _fields_ = [
                ("cb", ctypes.c_ulong),
                ("PageFaultCount", ctypes.c_ulong),
                ("PeakWorkingSetSize", ctypes.c_size_t),
                ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t),
                ("PeakPagefileUsage", ctypes.c_size_t),
            ]

        process = ctypes.windll.kernel32.OpenProcess(0x1000 | 0x0400, False, pid)
        if not process:
            return 0
        try:
            counters = ProcessMemoryCounters()
            counters.cb = ctypes.sizeof(counters)
            if not ctypes.windll.psapi.GetProcessMemoryInfo(
                process, ctypes.byref(counters), counters.cb
            ):
                return 0
            return int(counters.WorkingSetSize)
        finally:
            ctypes.windll.kernel32.CloseHandle(process)
    try:
        status = Path(f"/proc/{pid}/status").read_text(encoding="utf-8")
        for line in status.splitlines():
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) * 1024
    except (FileNotFoundError, ProcessLookupError, PermissionError, ValueError):
        return 0
    return 0


class MemoryMonitor:
    def __init__(self, stack: LocalStack) -> None:
        self.stack = stack
        self.peak = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="memory-monitor", daemon=True)

    def start(self) -> None:
        self._thread.start()

    def _run(self) -> None:
        while not self._stop.is_set():
            pids = set(self.stack.process_table().values())
            pids.add(os.getpid())
            total = sum(_read_rss_bytes(pid) for pid in pids)
            self.peak = max(self.peak, total)
            self._stop.wait(0.1)

    def stop(self) -> int:
        self._stop.set()
        self._thread.join(timeout=2)
        return self.peak


def _http_json(url: str) -> dict[str, Any]:
    last_error: Exception | None = None
    for _ in range(3):
        try:
            with urlopen(url, timeout=5.0) as response:
                return json.loads(response.read().decode("utf-8"))
        except (TimeoutError, URLError) as exc:
            last_error = exc
            time.sleep(0.1)
    assert last_error is not None
    raise last_error


def _http_text(url: str) -> str:
    last_error: Exception | None = None
    for _ in range(3):
        try:
            with urlopen(url, timeout=5.0) as response:
                return response.read().decode("utf-8")
        except (TimeoutError, URLError) as exc:
            last_error = exc
            time.sleep(0.1)
    assert last_error is not None
    raise last_error


def _feature(seed: int, index: int, count: int = 64) -> list[float]:
    rng = np.random.default_rng(seed)
    vector: list[float] = []
    for _ in range(index + 1):
        vector = rng.normal(0.0, 1.0, size=count).astype(np.float32).tolist()
    return vector


def _infer(
    target: str,
    request_id: str,
    session_id: str,
    features: list[float],
) -> inference_pb2.InferReply:
    with grpc.insecure_channel(target) as channel:
        return inference_pb2_grpc.RouterInferenceStub(channel).Infer(
            inference_pb2.InferRequest(
                request_id=request_id,
                session_id=session_id,
                features=features,
            ),
            timeout=3.0,
        )


def _wait_available(http_base: str, replica_id: str, expected: bool, timeout_s: float) -> bool:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        status = _http_json(f"{http_base}/status")
        for replica in status["replicas"]:
            if replica["replica_id"] == replica_id:
                if replica["available"] == expected:
                    return True
                break
        time.sleep(0.1)
    return False


def _directory_bytes(*paths: Path) -> int:
    total = 0
    for path in paths:
        if not path.exists():
            continue
        for item in path.rglob("*"):
            if item.is_file():
                try:
                    total += item.stat().st_size
                except OSError:
                    pass
    return total


def main() -> int:
    parser = argparse.ArgumentParser(description="Run deterministic load and fault-injection scenario")
    parser.add_argument("--config", default=str(ROOT / "config" / "router.json"))
    parser.add_argument("--baseline-seconds", type=float, default=5.0)
    parser.add_argument("--fault-seconds", type=float, default=8.0)
    parser.add_argument("--rate", type=float, default=120.0)
    parser.add_argument("--concurrency", type=int, default=32)
    parser.add_argument("--output", default=str(ROOT / "reports" / "scenario.json"))
    parser.add_argument("--no-affinity", action="store_true")
    args = parser.parse_args()

    selected_cpus = [] if args.no_affinity else _set_cpu_affinity()
    stack = LocalStack(ROOT, args.config)
    http_base = f"http://{stack.config.service.http}"
    grpc_target = stack.config.service.router_grpc
    monitor = MemoryMonitor(stack)
    report: dict[str, Any] = {
        "schema_version": 1,
        "config": str(Path(args.config).resolve()),
        "cpu_affinity": selected_cpus,
        "constraints": {
            "logical_cpus": 2,
            "peak_memory_mib": 1280,
            "temp_disk_mib": 2048,
        },
        "scenarios": {},
        "assertions": {},
    }
    assertions = report["assertions"]
    initial_disk = _directory_bytes(ROOT / "data", ROOT / "runtime")
    exit_code = 0
    try:
        stack.start_all_workers()
        stack.start_router(fresh_db=True)
        monitor.start()
        time.sleep(1.2)

        baseline_requests = int(args.rate * args.baseline_seconds)
        baseline = run_load(
            grpc_target,
            requests=baseline_requests,
            rate=args.rate,
            concurrency=args.concurrency,
            seed=42,
            request_prefix="base",
            session_prefix="base-session",
        )
        report["scenarios"]["baseline"] = baseline
        assertions["baseline_no_errors"] = baseline["errors"] == 0
        assertions["baseline_p95_within_120ms"] = baseline["e2e_ms"]["p95"] <= 120.0
        assertions["baseline_no_session_violation"] = baseline["session_violations"] == 0

        delayed_replica = "l2-a"
        delayed_address = next(
            replica.listen_address
            for replica in stack.config.shards
            if replica.replica_id == delayed_replica
        )
        fault_result: dict[str, Any] = {}

        def run_fault_load() -> None:
            fault_result.update(
                run_load(
                    grpc_target,
                    requests=int(args.rate * args.fault_seconds),
                    rate=args.rate,
                    concurrency=args.concurrency,
                    seed=43,
                    request_prefix="fault",
                    session_prefix="fault-session",
                )
            )

        fault_thread = threading.Thread(target=run_fault_load, name="fault-load", daemon=True)
        fault_thread.start()
        time.sleep(2.0)
        injection = set_delay(delayed_address, 250)
        time.sleep(1.6)
        evicted_status = _http_json(f"{http_base}/status")
        evicted = _wait_available(http_base, delayed_replica, False, 0.5)
        time.sleep(0.4)
        cleared = set_delay(delayed_address, 0)
        recovered = _wait_available(http_base, delayed_replica, True, 5.0)
        fault_thread.join()
        injected = dict(fault_result)
        report["scenarios"]["single_replica_delay"] = {
            "injection": injection,
            "clear": cleared,
            "replica_id": delayed_replica,
            "evicted_during_fault": evicted,
            "recovered_after_clear": recovered,
            "status_snapshot": {
                "replicas": [
                    item
                    for item in evicted_status["replicas"]
                    if item["replica_id"] == delayed_replica
                ],
                "routing_preference": evicted_status["routing_preference"],
            },
            "load": injected,
        }
        assertions["delay_replica_evicted"] = evicted
        assertions["delay_replica_recovered"] = recovered
        assertions["delay_no_errors"] = injected.get("errors", 1) == 0
        assertions["delay_p99_within_300ms"] = injected.get("e2e_ms", {}).get("p99", 9999) <= 300.0
        assertions["delay_no_session_violation"] = injected.get("session_violations", 1) == 0
        assertions["delay_retry_switch_observed"] = injected.get("retry_switches", 0) > 0

        l2_shards = [item for item in stack.config.shards if item.layer == "l2"]
        for shard in l2_shards:
            set_delay(shard.listen_address, 500)
        dual_started = time.perf_counter()
        dual_reply = _infer(
            grpc_target,
            "dual-l2-failure-001",
            "dual-l2-session-001",
            _feature(99, 0),
        )
        dual_e2e_ms = (time.perf_counter() - dual_started) * 1000.0
        for shard in l2_shards:
            set_delay(shard.listen_address, 0)
        dual_attempted = list(dual_reply.attempted_replicas)
        dual_sessions = {call.session_id for call in dual_reply.calls}
        report["scenarios"]["dual_l2_timeout"] = {
            "ok": dual_reply.ok,
            "error_code": dual_reply.error_code,
            "attempted_replicas": dual_attempted,
            "session_ids": sorted(dual_sessions),
            "e2e_ms": round(dual_e2e_ms, 3),
            "router_e2e_ms": round(dual_reply.e2e_ms, 3),
            "calls": [
                {
                    "layer_id": call.layer_id,
                    "replica_id": call.replica_id,
                    "timed_out": call.timed_out,
                    "session_id": call.session_id,
                }
                for call in dual_reply.calls
            ],
        }
        assertions["dual_l2_returns_error"] = not dual_reply.ok
        assertions["dual_l2_attempts_two_replicas"] = len(
            {
                call.replica_id
                for call in dual_reply.calls
                if call.layer_id == "l2"
            }
        ) == 2
        assertions["dual_l2_same_session"] = (
            len(dual_sessions) == 1 and "dual-l2-session-001" in dual_sessions
        )
        assertions["dual_l2_within_budget"] = dual_reply.e2e_ms <= 120.0
        time.sleep(1.2)

        duplicate_index = 123 if baseline_requests > 123 else 0
        duplicate_id = f"base-{duplicate_index:06d}"
        duplicate_session = f"base-session-{duplicate_index:06d}"
        duplicate_features = _feature(42, duplicate_index)
        before_query = _http_json(f"{http_base}/v1/requests/{duplicate_id}")["request"]
        duplicate_reply = _infer(
            grpc_target, duplicate_id, duplicate_session, duplicate_features
        )
        after_query = _http_json(f"{http_base}/v1/requests/{duplicate_id}")["request"]
        report["scenarios"]["idempotency"] = {
            "request_id": duplicate_id,
            "deduplicated": duplicate_reply.deduplicated,
            "ok": duplicate_reply.ok,
            "calls_before": len(before_query["calls"]),
            "calls_after": len(after_query["calls"]),
            "attempted_replicas_before": [
                call["replica_id"] for call in before_query["calls"]
            ],
            "attempted_replicas_after": [
                call["replica_id"] for call in after_query["calls"]
            ],
        }
        assertions["duplicate_returns_cached_result"] = (
            duplicate_reply.deduplicated and duplicate_reply.ok
        )
        assertions["duplicate_does_not_add_calls"] = (
            len(before_query["calls"]) == len(after_query["calls"])
        )

        stats_snapshot = _http_json(f"{http_base}/stats")
        status_snapshot = _http_json(f"{http_base}/status")
        metrics_text = _http_text(f"{http_base}/metrics")
        required_metrics = [
            "inference_layer_available_replicas",
            "inference_replica_calls_total",
            "inference_replica_timeouts_total",
            "inference_replica_latency_seconds",
            "inference_replica_evictions_total",
            "inference_e2e_p99_seconds",
        ]
        report["observability"] = {
            "status_replica_count": len(status_snapshot["replicas"]),
            "status_sort_key_order": list(status_snapshot.keys()),
            "stats_keys": list(stats_snapshot.keys()),
            "metrics_present": {
                name: name in metrics_text for name in required_metrics
            },
        }
        assertions["status_has_five_replicas"] = len(status_snapshot["replicas"]) == 5
        assertions["all_required_metrics_present"] = all(
            item["metrics_present"].values()
            for item in [report["observability"]]
        )

        stack.stop_router()
        pending_id = "recover-000001"
        pending_features = _feature(777, 0)
        pending_store = RequestStore(stack.config.service.db_path)
        pending_state, _ = pending_store.claim_request(
            pending_id, "recover-session-000001", pending_features
        )
        pending_store.close()
        stack.start_router(fresh_db=False)
        recovered_query = _http_json(f"{http_base}/v1/requests/{pending_id}")["request"]
        duplicate_after_restart = _infer(
            grpc_target, duplicate_id, duplicate_session, duplicate_features
        )
        after_restart_query = _http_json(f"{http_base}/v1/requests/{duplicate_id}")["request"]
        report["scenarios"]["restart_and_recovery"] = {
            "pending_claim_state": pending_state,
            "recovered_request_status": recovered_query["status"],
            "recovered_request_calls": [
                {
                    "layer_id": call["layer_id"],
                    "replica_id": call["replica_id"],
                    "session_id": call["session_id"],
                }
                for call in recovered_query["calls"]
            ],
            "duplicate_after_restart": {
                "deduplicated": duplicate_after_restart.deduplicated,
                "ok": duplicate_after_restart.ok,
                "calls_before": len(after_query["calls"]),
                "calls_after": len(after_restart_query["calls"]),
                "output_equal": list(duplicate_after_restart.output)
                == list(duplicate_reply.output),
            },
        }
        assertions["pending_request_recovered"] = (
            recovered_query["status"] == "succeeded"
            and len(recovered_query["calls"]) == 2
        )
        assertions["completed_result_unchanged_after_restart"] = (
            duplicate_after_restart.deduplicated
            and duplicate_after_restart.ok
            and len(after_restart_query["calls"]) == len(after_query["calls"])
            and list(duplicate_after_restart.output) == list(duplicate_reply.output)
        )

        for shard in l2_shards:
            stack.stop_worker(shard.replica_id)
        all_down_evicted = all(
            _wait_available(http_base, shard.replica_id, False, 5.0)
            for shard in l2_shards
        )
        unavailable_reply = _infer(
            grpc_target,
            "all-l2-unavailable-001",
            "all-l2-unavailable-session-001",
            _feature(1000, 0),
        )
        report["scenarios"]["all_l2_unavailable"] = {
            "evicted": all_down_evicted,
            "ok": unavailable_reply.ok,
            "error_code": unavailable_reply.error_code,
            "attempted_replicas": list(unavailable_reply.attempted_replicas),
        }
        assertions["all_l2_evicted"] = all_down_evicted
        assertions["all_l2_stable_error"] = (
            not unavailable_reply.ok
            and unavailable_reply.error_code == "L2_UNAVAILABLE"
        )
        for shard in l2_shards:
            stack.start_worker(shard.replica_id)
        l2_recovered = all(
            stack.wait_worker_ready(shard.listen_address, shard.replica_id, 8.0)
            and _wait_available(http_base, shard.replica_id, True, 8.0)
            for shard in l2_shards
        )
        assertions["l2_recovered_after_restart"] = l2_recovered
        report["scenarios"]["all_l2_unavailable"]["recovered"] = l2_recovered

        final_status = _http_json(f"{http_base}/status")
        final_stats = _http_json(f"{http_base}/stats")
        report["final_state"] = {
            "available_replicas": {
                layer["layer_id"]: layer["available_replicas"]
                for layer in final_status["layers"]
            },
            "e2e": final_stats["e2e"],
            "culprit": final_stats["culprit"],
            "window": final_stats["window"],
        }
        assertions["all_layers_final_available"] = all(
            layer["available_replicas"] > 0 for layer in final_status["layers"]
        )
    except Exception as exc:
        report["fatal_error"] = f"{type(exc).__name__}: {exc}"
        exit_code = 2
    finally:
        peak = monitor.stop() if monitor._thread.is_alive() else monitor.peak
        report["resources"] = {
            "peak_rss_mib": round(peak / (1024 * 1024), 3),
            "peak_memory_limit_mib": 1280,
            "temp_disk_mib": round(
                (_directory_bytes(ROOT / "data", ROOT / "runtime") - initial_disk)
                / (1024 * 1024),
                3,
            ),
            "temp_disk_limit_mib": 2048,
        }
        assertions["peak_memory_within_limit"] = (
            report["resources"]["peak_rss_mib"] <= 1280
        )
        assertions["temp_disk_within_limit"] = (
            report["resources"]["temp_disk_mib"] <= 2048
        )
        stack.stop_all()

    report["success"] = bool(assertions) and all(assertions.values()) and exit_code == 0
    if not report["success"]:
        exit_code = 1
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(json.dumps(report, indent=2, sort_keys=True))
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
