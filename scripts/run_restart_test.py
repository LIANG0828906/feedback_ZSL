#!/usr/bin/env python3
"""Kill the coordinator and writer after durable output, then verify replay dedup."""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from run_scenario import (  # noqa: E402
    ROOT,
    free_port,
    find_executable,
    http_json,
    scan_output,
    start_process,
    terminate,
    wait_http,
    wait_port,
    write_json,
)


def request_json(url: str, method: str = "GET", body: object | None = None) -> tuple[int, dict]:
    raw = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=raw, method=method)
    if raw is not None:
        request.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=10) as response:
            return response.status, json.loads(response.read().decode() or "{}")
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode() or "{}")


def check_runtime_quota(coordinator_url: str) -> tuple[bool, bool, bool]:
    status, _ = request_json(
        coordinator_url + "/v1/tenants/tenant-01/quota",
        method="PUT",
        body={"logs_per_second": 1},
    )
    immediate = status == 200
    records = []
    for tenant, count, batch in (("tenant-01", 2, "quota-over"), ("tenant-02", 1, "quota-ok")):
        log_records = []
        for index in range(count):
            log_records.append(
                {
                    "timeUnixNano": str(time.time_ns()),
                    "body": {"stringValue": "quota test"},
                    "attributes": [
                        {"key": "record.id", "value": {"stringValue": f"{batch}-r{index}"}},
                        {"key": "batch.id", "value": {"stringValue": batch}},
                    ],
                }
            )
        records.append(
            {
                "resource": {
                    "attributes": [
                        {"key": "tenant.id", "value": {"stringValue": tenant}},
                    ]
                },
                "scopeLogs": [{"logRecords": log_records}],
            }
        )
    status, response = request_json(
        coordinator_url + "/v1/logs",
        method="POST",
        body={"resourceLogs": records},
    )
    isolated = (
        status == 207
        and int(response.get("accepted_records", 0)) == 1
        and any(
            item.get("tenant_id") == "tenant-01" and item.get("code") == "TENANT_QUOTA_EXCEEDED"
            for item in response.get("results", [])
        )
        and any(
            item.get("tenant_id") == "tenant-02" and item.get("code") == "ACCEPTED"
            for item in response.get("results", [])
        )
    )
    invalid_status, _ = request_json(
        coordinator_url + "/v1/tenants/tenant-01/quota",
        method="PUT",
        body={"logs_per_second": 0},
    )
    _, quota = request_json(coordinator_url + "/v1/tenants/tenant-01/quota")
    preserved = invalid_status == 400 and int(quota.get("logs_per_second", 0)) == 1
    request_json(
        coordinator_url + "/v1/tenants/tenant-01/quota",
        method="PUT",
        body={"logs_per_second": 200},
    )
    return immediate, isolated, preserved


def main() -> int:
    env = os.environ.copy()
    env["GOMAXPROCS"] = "2"
    go = find_executable("GO_BIN", "go")
    redis_bin = find_executable("REDIS_SERVER_BIN", "redis-server")
    if not redis_bin:
        raise RuntimeError("redis-server not found")
    if go:
        subprocess.run([go, "build", "-o", str(ROOT / "bin" / "coordinator"), "./cmd/coordinator"], cwd=ROOT, check=True, env=env)
        subprocess.run([go, "build", "-o", str(ROOT / "bin" / "writer"), "./cmd/writer"], cwd=ROOT, check=True, env=env)
        subprocess.run([go, "build", "-o", str(ROOT / "bin" / "loadgen"), "./cmd/loadgen"], cwd=ROOT, check=True, env=env)

    run_dir = ROOT / "run" / f"restart-{time.strftime('%Y%m%d-%H%M%S', time.gmtime())}-{os.getpid()}"
    (run_dir / "storage").mkdir(parents=True, exist_ok=True)
    (run_dir / "redis-data").mkdir(parents=True, exist_ok=True)
    coordinator_port = free_port()
    writer_port = free_port()
    redis_port = free_port()
    cfg = json.loads((ROOT / "config" / "coordinator.json").read_text(encoding="utf-8"))
    cfg["listen_addr"] = f"127.0.0.1:{coordinator_port}"
    cfg["redis"]["addr"] = f"127.0.0.1:{redis_port}"
    cfg["redis"]["prefix"] = "lq:restart:"
    write_json(run_dir / "coordinator.json", cfg)
    scenario = {
        "seed": 296,
        "duration_ms": 1000,
        "tick_ms": 100,
        "target": f"http://127.0.0.1:{coordinator_port}/v1/logs",
        "coordinator_url": f"http://127.0.0.1:{coordinator_port}",
        "writer_control_url": f"http://127.0.0.1:{writer_port}",
        "tenant_rates": {
            "tenant-01": 200,
            "tenant-02": 300,
            "tenant-03": 600,
            "tenant-04": 900,
            "tenant-05": 1500,
            "tenant-06": 3000,
        },
        "plan": [
            {"at_ms": index * 20, "tenant_id": f"tenant-{index + 1:02d}", "records": 10, "batches": 2}
            for index in range(6)
        ],
    }
    write_json(run_dir / "scenario.json", scenario)
    output_path = run_dir / "storage" / "logs.jsonl"
    output_path.write_bytes(b"")
    coordinator_cmd = [str(ROOT / "bin" / "coordinator"), "-config", str(run_dir / "coordinator.json")]
    writer_cmd = [
        str(ROOT / "bin" / "writer"),
        "-config",
        str(run_dir / "coordinator.json"),
        "-output",
        str(output_path),
        "-coordinator",
        f"http://127.0.0.1:{coordinator_port}",
        "-control",
        f"127.0.0.1:{writer_port}",
    ]
    processes: dict[str, subprocess.Popen | None] = {"redis": None, "coordinator": None, "writer": None}
    try:
        processes["redis"] = start_process(
            "redis",
            [
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
            ],
            run_dir,
            env,
        )
        wait_port(redis_port, time.time() + 15)
        processes["coordinator"] = start_process("coordinator", coordinator_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{coordinator_port}/health", time.time() + 20)
        processes["writer"] = start_process("writer", writer_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{writer_port}/health", time.time() + 20)

        quota_immediate, quota_isolated, invalid_quota_preserved = check_runtime_quota(
            f"http://127.0.0.1:{coordinator_port}"
        )
        _, _ = http_json(
            f"http://127.0.0.1:{writer_port}/admin/fault",
            method="POST",
            body={"tenant_id": "*", "delay_ms": 0, "ack_delay_ms": 10000},
        )
        loadgen = subprocess.run(
            [
                str(ROOT / "bin" / "loadgen"),
                "-scenario",
                str(run_dir / "scenario.json"),
                "-summary",
                str(run_dir / "loadgen-summary.json"),
            ],
            cwd=ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=60,
        )
        if loadgen.returncode != 0:
            raise RuntimeError(f"loadgen failed: {loadgen.stderr}")
        summary = json.loads((run_dir / "loadgen-summary.json").read_text(encoding="utf-8"))
        expected_records = sum(int(item["offered_records"]) for item in summary["tenants"].values()) + 1

        deadline = time.time() + 20
        prekill = None
        while time.time() < deadline:
            _, stats = http_json(f"http://127.0.0.1:{writer_port}/stats")
            if int(stats.get("written_records", 0)) >= expected_records:
                prekill = stats
                break
            time.sleep(0.05)
        if prekill is None:
            raise RuntimeError("writer did not reach durable output before ack")
        _, prekill_status = http_json(f"http://127.0.0.1:{coordinator_port}/v1/status")
        pending_before = sum(int(item["pending_batches"]) for item in prekill_status["tenants"])
        if pending_before == 0:
            raise RuntimeError("no unacknowledged batches were available for the restart test")

        for name in ("writer", "coordinator"):
            process = processes[name]
            if process is not None:
                process.kill()
                process.wait(timeout=10)
        processes["coordinator"] = start_process("coordinator-restarted", coordinator_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{coordinator_port}/health", time.time() + 20)
        processes["writer"] = start_process("writer-restarted", writer_cmd, run_dir, env)
        wait_http(f"http://127.0.0.1:{writer_port}/health", time.time() + 20)

        deadline = time.time() + 40
        while time.time() < deadline:
            _, status = http_json(f"http://127.0.0.1:{coordinator_port}/v1/status")
            pending = sum(int(item["pending_batches"]) for item in status["tenants"])
            if pending == 0:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("queue did not drain after restart")
        time.sleep(0.5)
        _, status = http_json(f"http://127.0.0.1:{coordinator_port}/v1/status")
        _, cursors = http_json(f"http://127.0.0.1:{coordinator_port}/v1/cursors")
        _, stats = http_json(f"http://127.0.0.1:{writer_port}/stats")
        output = scan_output(output_path)
        checks = {
            "all_offered_records_written_once": output["rows"] == expected_records and output["duplicate_rows"] == 0,
            "writer_observed_duplicate_replay": int(stats.get("duplicate_batches", 0)) > 0,
            "cursors_present_after_restart": all(int(item["last_seq"]) > 0 for item in cursors["cursors"]),
            "queue_empty_after_restart": sum(int(item["pending_batches"]) for item in status["tenants"]) == 0,
            "runtime_quota_change_immediate": quota_immediate,
            "tenant_quota_rejection_isolated": quota_isolated,
            "invalid_quota_preserved_previous_value": invalid_quota_preserved,
        }
        report = {
            "schema_version": 1,
            "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "expected_records": expected_records,
            "pending_before_restart": pending_before,
            "written_before_restart": int(prekill.get("written_records", 0)),
            "final_written_rows": output["rows"],
            "duplicate_rows": output["duplicate_rows"],
            "dedup_skipped_batches": int(stats.get("duplicate_batches", 0)),
            "dedup_skipped_records": int(stats.get("duplicate_records", 0)),
            "cursors": cursors["cursors"],
            "checks": checks,
            "ok": all(checks.values()),
        }
        write_json(ROOT / "artifacts" / "restart-report.json", report)
        write_json(run_dir / "restart-report.json", report)
        print(json.dumps({"ok": report["ok"], "checks": checks}, indent=2))
        return 0 if report["ok"] else 1
    finally:
        for name in ("writer", "coordinator", "redis"):
            terminate(processes.get(name))
        for process in processes.values():
            if process is not None:
                log_file = getattr(process, "_quota_log", None)
                if log_file is not None:
                    log_file.close()


if __name__ == "__main__":
    raise SystemExit(main())
