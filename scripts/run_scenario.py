#!/usr/bin/env python3
"""Run the deterministic six-tenant Vector/Redis quota scenario and verify output."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def http_json(url: str, method: str = "GET", body: object | None = None, timeout: float = 5.0):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    with OPENER.open(req, timeout=timeout) as response:
        return response.status, json.loads(response.read().decode() or "{}")


def http_text(url: str, timeout: float = 5.0) -> str:
    with OPENER.open(url, timeout=timeout) as response:
        return response.read().decode()


def wait_http(url: str, deadline: float) -> None:
    last_error: Exception | None = None
    while time.time() < deadline:
        try:
            status, _ = http_json(url, timeout=0.5)
            if status == 200:
                return
        except Exception as exc:  # noqa: BLE001 - readiness probes intentionally broad
            last_error = exc
        time.sleep(0.1)
    raise RuntimeError(f"service did not become ready: {url}: {last_error}")


def wait_port(port: int, deadline: float) -> None:
    last_error: Exception | None = None
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return
        except Exception as exc:  # noqa: BLE001
            last_error = exc
        time.sleep(0.1)
    raise RuntimeError(f"port did not become ready: {port}: {last_error}")


def terminate(proc: subprocess.Popen | None, timeout: float = 8.0) -> None:
    if proc is None or proc.poll() is not None:
        return
    try:
        proc.send_signal(signal.SIGTERM)
        proc.wait(timeout=timeout)
    except Exception:  # noqa: BLE001
        try:
            proc.kill()
            proc.wait(timeout=2.0)
        except Exception:  # noqa: BLE001
            pass


def find_executable(env_name: str, name: str) -> str | None:
    requested = os.environ.get(env_name)
    if requested:
        path = Path(requested).expanduser()
        if path.exists():
            return str(path)
        raise RuntimeError(f"{env_name} points to a missing executable: {requested}")
    return shutil.which(name)


def start_process(name: str, command: list[str], run_dir: Path, env: dict[str, str]) -> subprocess.Popen:
    log_path = run_dir / f"{name}.log"
    log_file = open(log_path, "wb")
    proc = subprocess.Popen(
        command,
        cwd=ROOT,
        env=env,
        stdout=log_file,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    proc._quota_log = log_file  # type: ignore[attr-defined]
    return proc


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n", encoding="utf-8")


def prepare_config(coordinator_port: int, redis_port: int, run_dir: Path) -> dict:
    cfg = json.loads((ROOT / "config" / "coordinator.json").read_text(encoding="utf-8"))
    cfg["listen_addr"] = f"127.0.0.1:{coordinator_port}"
    cfg["redis"]["addr"] = f"127.0.0.1:{redis_port}"
    cfg["redis"]["db"] = 0
    cfg["redis"]["prefix"] = "lq:scenario:"
    write_json(run_dir / "coordinator.json", cfg)
    return cfg


def prepare_vector(coordinator_port: int, http_port: int, grpc_port: int, run_dir: Path) -> Path:
    text = (ROOT / "config" / "vector.yaml").read_text(encoding="utf-8")
    text = text.replace("127.0.0.1:4318", f"127.0.0.1:{http_port}")
    text = text.replace("127.0.0.1:4317", f"127.0.0.1:{grpc_port}")
    text = text.replace("http://127.0.0.1:8080", f"http://127.0.0.1:{coordinator_port}")
    text = text.replace("data_dir: run/vector", f"data_dir: {(run_dir / 'vector').as_posix()}")
    path = run_dir / "vector.yaml"
    path.write_text(text, encoding="utf-8")
    return path


def scan_output(path: Path) -> dict:
    rows = 0
    unique_record_ids: set[str] = set()
    batch_rows: dict[str, int] = {}
    tenants: dict[str, int] = {}
    sha = hashlib.sha256()
    with open(path, "rb") as handle:
        for raw in handle:
            rows += 1
            sha.update(raw)
            try:
                row = json.loads(raw)
            except json.JSONDecodeError:
                continue
            record_id = str(row.get("_record_id", ""))
            batch_id = str(row.get("_batch_id", ""))
            tenant = str(row.get("_tenant_id", ""))
            if record_id:
                unique_record_ids.add(record_id)
            if batch_id:
                batch_rows[batch_id] = batch_rows.get(batch_id, 0) + 1
            tenants[tenant] = tenants.get(tenant, 0) + 1
    return {
        "rows": rows,
        "unique_record_ids": len(unique_record_ids),
        "duplicate_rows": rows - len(unique_record_ids),
        "unique_batch_ids": len(batch_rows),
        "batch_row_counts": batch_rows,
        "tenant_rows": tenants,
        "sha256": sha.hexdigest(),
    }


def build_report(
    scenario: dict,
    summary: dict,
    status: dict,
    cursors: dict,
    receipts: list[dict],
    stats: dict,
    output: dict,
    execution: dict,
) -> dict:
    status_tenants = {item["tenant_id"]: item for item in status["tenants"]}
    tenant_rows = []
    degraded_tenants: list[str] = []
    recovered_tenants: list[str] = []
    degradation_events: list[dict] = []
    total_offered = 0
    total_ingested = 0
    total_coordinator_dropped = 0
    total_bp_rejected = 0
    total_timeouts = 0
    for tenant in sorted(status_tenants):
        state = status_tenants[tenant]
        generated = summary.get("tenants", {}).get(tenant, {})
        offered = int(generated.get("offered_records", 0))
        ingested = int(state.get("ingested_records", 0))
        coordinator_dropped = int(state.get("dropped_records", 0))
        dropped = max(offered - ingested, 0)
        records = {
            "tenant_id": tenant,
            "offered_records": offered,
            "ingested_records": ingested,
            "dropped_records": dropped,
            "coordinator_dropped_records": coordinator_dropped,
            "ingested_bytes": int(state.get("ingested_bytes", 0)),
            "backpressure_rejected_batches": int(state.get("backpressure_rejected_batches", 0)),
            "write_timeouts": int(state.get("write_timeouts", 0)),
            "pending_batches": int(state.get("pending_batches", 0)),
            "degraded_now": bool(state.get("degraded", False)),
            "output_records": int(output["tenant_rows"].get(tenant, 0)),
        }
        last = state.get("last_degrade")
        history = state.get("degrade_history") or []
        events = list(history)
        if last and not any(e.get("started_at") == last.get("started_at") for e in events):
            events.append(last)
        for event in events:
            degradation_events.append(
                {
                    "tenant_id": tenant,
                    "trigger_metric": event.get("trigger_metric"),
                    "trigger_value": event.get("trigger_value"),
                    "threshold": event.get("threshold"),
                    "window_samples": event.get("window_samples"),
                    "started_at": event.get("started_at"),
                    "recovered_at": event.get("recovered_at", ""),
                    "recovery_ack_samples": event.get("recovery_ack_samples", 0),
                    "recovery_successes": event.get("recovery_successes", 0),
                }
            )
            if event.get("started_at") and tenant not in degraded_tenants:
                degraded_tenants.append(tenant)
            if event.get("recovered_at") and tenant not in recovered_tenants:
                recovered_tenants.append(tenant)
        tenant_rows.append(records)
        total_offered += offered
        total_ingested += ingested
        total_coordinator_dropped += coordinator_dropped
        total_bp_rejected += records["backpressure_rejected_batches"]
        total_timeouts += records["write_timeouts"]

    bp = status["backpressure"]
    pending_batches = sum(int(item["pending_batches"]) for item in status["tenants"])
    expected_degraded = [scenario.get("burst", {}).get("tenant_id")]
    quota_updates_applied = True
    for update in scenario.get("quota_updates", []):
        tenant = update.get("tenant_id")
        expected = update.get("logs_per_second")
        if expected is not None and int(status_tenants[tenant]["quota"]["logs_per_second"]) != int(expected):
            quota_updates_applied = False
    receipts_acknowledged = bool(receipts) and all(bool(item.get("acknowledged")) for item in receipts)
    checks = {
        "no_pending_batches": pending_batches == 0,
        "backpressure_triggered": int(bp.get("entry_count", 0)) > 0,
        "tenant_degradation_triggered": bool(degraded_tenants),
        "only_burst_tenant_degraded": degraded_tenants == expected_degraded,
        "degraded_tenant_recovered": set(degraded_tenants).issubset(set(recovered_tenants)),
        "no_duplicate_output_rows": output["duplicate_rows"] == 0,
        "all_ingested_records_written": total_ingested == output["rows"],
        "records_accounted": total_offered == total_ingested + sum(
            max(item["offered_records"] - item["ingested_records"], 0) for item in tenant_rows
        ),
        "cursors_available": len(cursors.get("cursors", [])) == len(status_tenants),
        "ack_receipts_acknowledged": receipts_acknowledged,
        "runtime_quota_update_applied": quota_updates_applied,
    }
    return {
        "schema_version": 1,
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "scenario": {
            "seed": scenario.get("seed"),
            "duration_ms": scenario.get("duration_ms"),
            "tick_ms": scenario.get("tick_ms"),
            "max_batch_records": scenario.get("max_batch_records"),
            "tenant_rates": scenario.get("tenant_rates"),
            "burst": scenario.get("burst"),
            "faults": scenario.get("faults", []),
            "quota_updates": scenario.get("quota_updates", []),
            "plan": scenario.get("plan", []),
        },
        "execution": execution,
        "backpressure": {
            "triggered": int(bp.get("entry_count", 0)) > 0,
            "entry_count": int(bp.get("entry_count", 0)),
            "total_duration_ms": int(bp.get("total_duration_ms", 0)),
            "max_backlog_depth": int(bp.get("max_backlog_depth", 0)),
            "degraded_tenants": sorted(degraded_tenants),
            "recovered_tenants": sorted(recovered_tenants),
        },
        "degradation_events": sorted(
            degradation_events,
            key=lambda event: (event["tenant_id"], event["started_at"] or ""),
        ),
        "ack_receipts": [
            {
                "tenant_id": item.get("tenant_id"),
                "batch_id": item.get("batch_id"),
                "seq": item.get("seq"),
                "record_count": item.get("record_count"),
                "acknowledged": bool(item.get("acknowledged")),
            }
            for item in receipts
        ],
        "tenants": tenant_rows,
        "totals": {
            "offered_records": total_offered,
            "ingested_records": total_ingested,
            "dropped_records": sum(item["dropped_records"] for item in tenant_rows),
            "coordinator_dropped_records": total_coordinator_dropped,
            "backpressure_rejected_batches": total_bp_rejected,
            "write_timeouts": total_timeouts,
            "pending_batches": pending_batches,
        },
        "final": {
            "written_rows": output["rows"],
            "unique_record_ids": output["unique_record_ids"],
            "unique_batch_ids": output["unique_batch_ids"],
            "duplicate_rows": output["duplicate_rows"],
            "dedup_skipped_batches": int(stats.get("duplicate_batches", 0)),
            "dedup_skipped_records": int(stats.get("duplicate_records", 0)),
        },
        "checks": checks,
        "ok": all(checks.values()),
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--direct", action="store_true", help="send OTLP directly to the coordinator; skip Vector")
    parser.add_argument("--build", action="store_true", help="build Go binaries before running")
    parser.add_argument("--scenario", default=str(ROOT / "config" / "scenario.json"))
    parser.add_argument("--report", default=str(ROOT / "artifacts" / "report.json"))
    parser.add_argument("--run-dir", default="")
    parser.add_argument("--restart-writer-at-ms", type=int, default=0)
    parser.add_argument("--drain-timeout", type=float, default=120.0)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    run_id = time.strftime("%Y%m%d-%H%M%S", time.gmtime()) + f"-{os.getpid()}"
    run_dir = Path(args.run_dir) if args.run_dir else ROOT / "run" / f"scenario-{run_id}"
    (run_dir / "storage").mkdir(parents=True, exist_ok=True)
    (run_dir / "vector").mkdir(parents=True, exist_ok=True)

    env = os.environ.copy()
    env["GOMAXPROCS"] = "2"
    env.setdefault("RUST_LOG", "info")

    coordinator_bin = ROOT / "bin" / "coordinator"
    writer_bin = ROOT / "bin" / "writer"
    loadgen_bin = ROOT / "bin" / "loadgen"
    if args.build or not all(path.exists() for path in (coordinator_bin, writer_bin, loadgen_bin)):
        go = find_executable("GO_BIN", "go")
        if not go:
            raise RuntimeError("Go is required to build binaries; set GO_BIN or put go on PATH")
        subprocess.run(
            [go, "build", "-o", str(coordinator_bin), "./cmd/coordinator"],
            cwd=ROOT,
            env=env,
            check=True,
        )
        subprocess.run([go, "build", "-o", str(writer_bin), "./cmd/writer"], cwd=ROOT, env=env, check=True)
        subprocess.run([go, "build", "-o", str(loadgen_bin), "./cmd/loadgen"], cwd=ROOT, env=env, check=True)

    redis_bin = find_executable("REDIS_SERVER_BIN", "redis-server")
    if not redis_bin:
        raise RuntimeError("redis-server not found; set REDIS_SERVER_BIN or put redis-server on PATH")
    vector_bin = None
    if not args.direct:
        vector_bin = find_executable("VECTOR_BIN", "vector")
        if not vector_bin:
            raise RuntimeError("vector not found; set VECTOR_BIN, put vector on PATH, or pass --direct")

    coordinator_port = free_port()
    writer_port = free_port()
    redis_port = free_port()
    vector_http_port = free_port()
    vector_grpc_port = free_port()
    cfg = prepare_config(coordinator_port, redis_port, run_dir)
    scenario = json.loads(Path(args.scenario).read_text(encoding="utf-8"))
    scenario = copy.deepcopy(scenario)
    scenario["coordinator_url"] = f"http://127.0.0.1:{coordinator_port}"
    scenario["writer_control_url"] = f"http://127.0.0.1:{writer_port}"
    if args.direct:
        scenario["target"] = f"http://127.0.0.1:{coordinator_port}/v1/logs"
    else:
        scenario["target"] = f"http://127.0.0.1:{vector_http_port}/v1/logs"
    scenario_path = run_dir / "scenario.json"
    write_json(scenario_path, scenario)

    processes: dict[str, subprocess.Popen | None] = {
        "redis": None,
        "coordinator": None,
        "writer": None,
        "vector": None,
    }
    execution: dict = {
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "elapsed_ms": 0,
        "redis_backend": Path(redis_bin).name,
        "vector_enabled": not args.direct,
        "writer_restarted": False,
    }
    started = time.monotonic()
    output_path = run_dir / "storage" / "logs.jsonl"
    output_path.write_bytes(b"")
    try:
        redis_cmd = [
            redis_bin,
            "--bind",
            "127.0.0.1",
            "--port",
            str(redis_port),
            "--save",
            "",
            "--appendonly",
            "no",
            "--dir",
            str(run_dir / "redis-data"),
        ]
        (run_dir / "redis-data").mkdir(exist_ok=True)
        processes["redis"] = start_process("redis", redis_cmd, run_dir, env)
        wait_port(redis_port, time.time() + 15)

        coordinator_cmd = [str(coordinator_bin), "-config", str(run_dir / "coordinator.json")]
        processes["coordinator"] = start_process("coordinator", coordinator_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{coordinator_port}/health", time.time() + 20)

        writer_cmd = [
            str(writer_bin),
            "-config",
            str(run_dir / "coordinator.json"),
            "-output",
            str(output_path),
            "-coordinator",
            f"http://127.0.0.1:{coordinator_port}",
            "-control",
            f"127.0.0.1:{writer_port}",
        ]
        processes["writer"] = start_process("writer", writer_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{writer_port}/health", time.time() + 20)

        if not args.direct:
            vector_cfg = prepare_vector(coordinator_port, vector_http_port, vector_grpc_port, run_dir)
            vector_cmd = [str(vector_bin), "--threads", "2", "--config", str(vector_cfg)]
            processes["vector"] = start_process("vector", vector_cmd, run_dir, env)
            wait_port(vector_http_port, time.time() + 25)
            time.sleep(0.5)

        restart_timer = None
        if args.restart_writer_at_ms > 0:
            def restart_writer() -> None:
                current = processes.get("writer")
                if current is not None:
                    current.kill()
                    try:
                        current.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        pass
                time.sleep(0.2)
                processes["writer"] = start_process("writer-restarted", writer_cmd, run_dir, env)
                wait_http(f"http://127.0.0.1:{writer_port}/health", time.time() + 20)
                execution["writer_restarted"] = True

            restart_timer = threading.Timer(args.restart_writer_at_ms / 1000.0, restart_writer)
            restart_timer.daemon = True
            restart_timer.start()

        loadgen_cmd = [
            str(loadgen_bin),
            "-scenario",
            str(scenario_path),
            "-summary",
            str(run_dir / "loadgen-summary.json"),
        ]
        loadgen = subprocess.run(
            loadgen_cmd,
            cwd=ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=max(120.0, scenario["duration_ms"] / 1000.0 * 4.0),
        )
        (run_dir / "loadgen.stdout.log").write_text(loadgen.stdout, encoding="utf-8")
        (run_dir / "loadgen.stderr.log").write_text(loadgen.stderr, encoding="utf-8")
        if restart_timer is not None:
            restart_timer.cancel()
        if loadgen.returncode != 0:
            raise RuntimeError(f"load generator failed with exit code {loadgen.returncode}")

        if processes.get("vector") is not None:
            terminate(processes["vector"], timeout=45.0)
            processes["vector"] = None

        drain_deadline = time.time() + args.drain_timeout
        stable_since = None
        last_written = -1
        last_ingested = -1
        while time.time() < drain_deadline:
            _, status_now = http_json(f"http://127.0.0.1:{coordinator_port}/v1/status")
            _, stats_now = http_json(f"http://127.0.0.1:{writer_port}/stats")
            pending = sum(int(item["pending_batches"]) for item in status_now["tenants"])
            written = int(stats_now.get("written_records", 0))
            ingested = sum(int(item["ingested_records"]) for item in status_now["tenants"])
            if pending == 0 and written == last_written and ingested == last_ingested:
                if stable_since is None:
                    stable_since = time.time()
                elif time.time() - stable_since >= 1.0:
                    break
            else:
                stable_since = None
                last_written = written
                last_ingested = ingested
            time.sleep(0.2)
        else:
            raise RuntimeError("timed out waiting for queue drain")

        status_code, status = http_json(f"http://127.0.0.1:{coordinator_port}/v1/status")
        cursors_code, cursors = http_json(f"http://127.0.0.1:{coordinator_port}/v1/cursors")
        receipts = []
        for cursor in cursors.get("cursors", []):
            if cursor.get("last_batch_id"):
                _, receipt = http_json(
                    f"http://127.0.0.1:{coordinator_port}/v1/receipts/"
                    f"{cursor['tenant_id']}/{cursor['last_batch_id']}"
                )
                receipts.append(receipt)
        stats_code, stats = http_json(f"http://127.0.0.1:{writer_port}/stats")
        metrics = http_text(f"http://127.0.0.1:{coordinator_port}/metrics")
        if min(status_code, cursors_code, stats_code) != 200:
            raise RuntimeError("failed to query final observability endpoints")
        write_json(run_dir / "status.json", status)
        write_json(run_dir / "cursors.json", cursors)
        write_json(run_dir / "writer-stats.json", stats)
        (run_dir / "metrics.txt").write_text(metrics, encoding="utf-8")

        summary = json.loads((run_dir / "loadgen-summary.json").read_text(encoding="utf-8"))
        output = scan_output(output_path)
        execution["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        execution["elapsed_ms"] = int((time.monotonic() - started) * 1000)
        report = build_report(scenario, summary, status, cursors, receipts, stats, output, execution)
        report_path = Path(args.report)
        write_json(report_path, report)
        write_json(run_dir / "report.json", report)
        print(json.dumps({"ok": report["ok"], "report": str(report_path), "checks": report["checks"]}, indent=2))
        return 0 if report["ok"] else 1
    finally:
        terminate(processes.get("vector"))
        terminate(processes.get("writer"))
        terminate(processes.get("coordinator"))
        terminate(processes.get("redis"))
        for proc in processes.values():
            if proc is not None:
                log_file = getattr(proc, "_quota_log", None)
                if log_file is not None:
                    log_file.close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # noqa: BLE001
        print(f"scenario failed: {exc}", file=sys.stderr)
        raise
