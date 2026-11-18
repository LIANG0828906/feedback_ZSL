#!/usr/bin/env python3
from __future__ import annotations

import argparse
import collections
import json
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]


def iso_now() -> str:
    return datetime.now(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def http_json(url: str, timeout: float = 10.0) -> Any:
    with urllib.request.urlopen(url, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def http_text(url: str, timeout: float = 10.0) -> str:
    with urllib.request.urlopen(url, timeout=timeout) as response:
        return response.read().decode("utf-8")


def with_retry(function: Any, attempts: int = 5, delay: float = 0.5) -> Any:
    last_error: Exception | None = None
    for _ in range(attempts):
        try:
            return function()
        except (
            urllib.error.URLError,
            TimeoutError,
            ConnectionError,
            json.JSONDecodeError,
        ) as exc:
            last_error = exc
            time.sleep(delay)
    raise RuntimeError(f"HTTP query failed after {attempts} attempts: {last_error}")


def terminate(process: subprocess.Popen[Any]) -> None:
    if process.poll() is not None:
        return
    try:
        if os.name == "nt":
            process.terminate()
        else:
            process.send_signal(signal.SIGINT)
        process.wait(timeout=8)
    except (ProcessLookupError, subprocess.TimeoutExpired):
        process.kill()
        process.wait(timeout=3)


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class Trace:
    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = path.open("w", encoding="utf-8")

    def add(self, event: str, **fields: Any) -> None:
        value = {"at": iso_now(), "event": event, **fields}
        self.handle.write(json.dumps(value, ensure_ascii=False, sort_keys=True) + "\n")
        self.handle.flush()

    def close(self) -> None:
        self.handle.close()


def aggregate_ledgers(paths: list[Path]) -> dict[str, Any]:
    counts: collections.Counter[str] = collections.Counter()
    by_member: collections.Counter[str] = collections.Counter()
    rows = 0
    missing_values = 0
    for path in paths:
        if not path.exists():
            continue
        with path.open("r", encoding="utf-8") as handle:
            for line in handle:
                if not line.strip():
                    continue
                row = json.loads(line)
                rows += 1
                order_id = row.get("order_id")
                member = row.get("member_id")
                if order_id is None:
                    missing_values += 1
                else:
                    counts[str(order_id)] += 1
                by_member[str(member)] += 1
    duplicates = sum(value - 1 for value in counts.values() if value > 1)
    return {
        "rows": rows,
        "unique_order_ids": len(counts),
        "duplicate_events": duplicates,
        "max_order_id_attempts": max(counts.values()) if counts else 0,
        "rows_without_order_id": missing_values,
        "rows_by_member": dict(sorted(by_member.items())),
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="End-to-end coordinator acceptance")
    parser.add_argument("--backend", choices=["simulator", "kafka"], default="simulator")
    parser.add_argument("--count", type=int, default=20000)
    parser.add_argument("--seed", type=int, default=290)
    parser.add_argument("--bootstrap", default="127.0.0.1:19092")
    parser.add_argument("--topic", default="order-events")
    parser.add_argument("--faults", choices=["none", "full"], default="full")
    parser.add_argument("--work-dir", default=None)
    parser.add_argument("--report", default="artifacts/acceptance_report.json")
    parser.add_argument("--trace", default="artifacts/trace.jsonl")
    parser.add_argument("--timeout-seconds", type=int, default=300)
    parser.add_argument("--keep-work-dir", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    work_dir = (
        Path(args.work_dir).resolve()
        if args.work_dir
        else (ROOT / "data" / "acceptance" / stamp).resolve()
    )
    work_dir.mkdir(parents=True, exist_ok=True)
    logs_dir = work_dir / "logs"
    ledger_dir = work_dir / "ledgers"
    logs_dir.mkdir(exist_ok=True)
    ledger_dir.mkdir(exist_ok=True)
    report_path = (ROOT / args.report).resolve()
    report_path.parent.mkdir(parents=True, exist_ok=True)
    trace = Trace((ROOT / args.trace).resolve())
    trace.add(
        "acceptance_started",
        backend=args.backend,
        count=args.count,
        faults=args.faults,
        work_dir=str(work_dir),
    )

    coordinator: subprocess.Popen[Any] | None = None
    workers: list[tuple[str, subprocess.Popen[Any], Any]] = []
    result: dict[str, Any] = {
        "started_at": iso_now(),
        "backend": args.backend,
        "requested_count": args.count,
        "fault_mode": args.faults,
        "work_dir": str(work_dir),
    }
    try:
        generate_command = [
            sys.executable,
            "-m",
            "ordergroup",
            "generate",
            "--backend",
            args.backend,
            "--bootstrap",
            args.bootstrap,
            "--topic",
            args.topic,
            "--partitions",
            "12",
            "--count",
            str(args.count),
            "--seed",
            str(args.seed),
            "--batch-size",
            "500",
            "--output-dir",
            str(work_dir / "sim"),
        ]
        trace.add("generate_started", command=generate_command)
        generated = subprocess.run(
            generate_command,
            cwd=ROOT,
            text=True,
            capture_output=True,
            timeout=max(60, args.timeout_seconds // 2),
        )
        if generated.returncode != 0:
            raise RuntimeError(
                f"generator failed ({generated.returncode}): {generated.stderr[-2000:]}"
            )
        manifest = json.loads(generated.stdout)
        result["generator"] = {
            "stream_sha256": manifest["stream_sha256"],
            "partition_counts": manifest["partition_counts"],
        }
        trace.add("generate_completed", stream_sha256=manifest["stream_sha256"])

        port = free_port()
        db_path = work_dir / "coordinator.sqlite3"
        env = dict(os.environ)
        env.update(
            {
                "ORDERGROUP_MAX_REBALANCE_PARTITIONS": "6",
                "ORDERGROUP_COOLDOWN_MS": "1000",
                "ORDERGROUP_REVOKE_ACK_TIMEOUT_MS": "1000",
            }
        )
        coordinator_command = [
            sys.executable,
            "-m",
            "ordergroup",
            "coordinator",
            "--backend",
            args.backend,
            "--bootstrap",
            args.bootstrap,
            "--topic",
            args.topic,
            "--partitions",
            "12",
            "--simulator-dir",
            str(work_dir / "sim"),
            "--db",
            str(db_path),
            "--host",
            "127.0.0.1",
            "--port",
            str(port),
            "--quiet",
        ]
        coordinator_log = (logs_dir / "coordinator.log").open("w", encoding="utf-8")
        coordinator = subprocess.Popen(
            coordinator_command,
            cwd=ROOT,
            env=env,
            stdout=coordinator_log,
            stderr=subprocess.STDOUT,
            text=True,
        )
        trace.add("coordinator_started", pid=coordinator.pid, port=port)
        base_url = f"http://127.0.0.1:{port}"
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                http_json(base_url + "/healthz", timeout=1)
                break
            except (urllib.error.URLError, TimeoutError):
                if coordinator.poll() is not None:
                    raise RuntimeError(
                        f"coordinator exited: {(logs_dir / 'coordinator.log').read_text()[-3000:]}"
                    )
                time.sleep(0.1)
        else:
            raise RuntimeError("coordinator did not become ready")

        worker_specs = [
            ("worker-1", 1),
            ("worker-2", 2),
            ("worker-3", 3),
        ]
        consume_delay = 1 if args.faults == "full" and args.count <= 50000 else 0
        for member_id, index in worker_specs:
            ledger_path = ledger_dir / f"{member_id}.jsonl"
            session_timeout_ms = 3000 if index == 3 else 60000
            heartbeat_interval_ms = 500 if index == 3 else 1000
            command = [
                sys.executable,
                "-m",
                "ordergroup",
                "worker",
                "--coordinator",
                base_url,
                "--member-id",
                member_id,
                "--session-timeout-ms",
                str(session_timeout_ms),
                "--heartbeat-interval-ms",
                str(heartbeat_interval_ms),
                "--batch-size",
                "250",
                "--consume-delay-ms",
                str(consume_delay),
                "--duration-seconds",
                str(max(20, args.timeout_seconds)),
                "--processed-log",
                str(ledger_path),
                "--quiet",
            ]
            if args.faults == "full":
                if index == 1:
                    command.extend(
                        [
                            "--pause-after-records",
                            "150",
                            "--pause-duration-ms",
                            "1500",
                        ]
                    )
                elif index == 2:
                    command.extend(
                        [
                            "--stall-after-records",
                            "150",
                            "--stall-duration-ms",
                            "1500",
                        ]
                    )
                elif index == 3:
                    command.extend(["--stop-heartbeat-after-ms", "1000"])
            worker_log = (logs_dir / f"{member_id}.log").open("w", encoding="utf-8")
            process = subprocess.Popen(
                command,
                cwd=ROOT,
                stdout=worker_log,
                stderr=subprocess.STDOUT,
                text=True,
            )
            workers.append((member_id, process, worker_log))
            trace.add("worker_started", member_id=member_id, pid=process.pid)
            time.sleep(0.15)

        started = time.monotonic()
        lag_result: dict[str, Any] | None = None
        stable_zero_reads = 0
        while time.monotonic() - started < args.timeout_seconds:
            if coordinator.poll() is not None:
                raise RuntimeError("coordinator exited during consumption")
            try:
                lag_result = http_json(base_url + "/v1/lag")
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.5)
                continue
            if lag_result.get("total_lag") == 0:
                stable_zero_reads += 1
            else:
                stable_zero_reads = 0
            elapsed = time.monotonic() - started
            minimum_runtime = 10.0 if args.faults == "full" else 0.5
            if stable_zero_reads >= 2 and elapsed >= minimum_runtime:
                break
            time.sleep(0.5)
        else:
            raise RuntimeError("consumption did not reach zero lag before timeout")
        trace.add(
            "lag_zero",
            total_lag=lag_result["total_lag"] if lag_result else None,
            elapsed_seconds=round(time.monotonic() - started, 3),
        )

        status = with_retry(
            lambda: http_json(base_url + "/v1/status?event_limit=5000", timeout=30)
        )
        metrics = with_retry(
            lambda: http_text(base_url + "/metrics", timeout=30)
        )
        final_lag = with_retry(
            lambda: http_json(base_url + "/v1/lag", timeout=30)
        )
        result["status_snapshot"] = {
            "generation": status["generation"],
            "phase": status["phase"],
            "members": [
                {
                    "member_id": member["member_id"],
                    "status": member["status"],
                    "owned_partitions": member["owned_partitions"],
                    "paused_partitions": member["paused_partitions"],
                }
                for member in status["members"]
            ],
            "last_rebalance": status["last_rebalance"],
        }
        result["observed_events"] = sorted(
            {event["kind"] for event in status.get("recent_events", [])}
        )
        result["lag"] = final_lag
        result["metrics_checks"] = {
            "committed_offset_series": metrics.count("order_group_committed_offset{"),
            "lag_series": metrics.count("order_group_lag{"),
            "assigned_series": metrics.count("order_group_partition_assigned{"),
            "heartbeat_timeout_series": metrics.count(
                "order_group_member_heartbeat_timeouts_total{"
            ),
            "rebalance_total_present": "order_group_rebalance_total " in metrics,
            "suppression_present": "order_group_rebalances_suppressed_total{" in metrics,
        }
    except Exception as exc:
        result["error"] = str(exc)
        trace.add("acceptance_error", error=str(exc))
    finally:
        for member_id, process, handle in workers:
            terminate(process)
            handle.close()
            trace.add("worker_stopped", member_id=member_id, exit_code=process.returncode)
        if coordinator is not None:
            terminate(coordinator)
            trace.add("coordinator_stopped", exit_code=coordinator.returncode)

    ledger_paths = [ledger_dir / f"worker-{index}.jsonl" for index in (1, 2, 3)]
    ledger = aggregate_ledgers(ledger_paths)
    result["ledger"] = ledger
    expected_ids = args.count
    checks = {
        "no_runtime_error": "error" not in result,
        "lag_zero": bool(result.get("lag", {}).get("total_lag") == 0),
        "all_events_present": ledger["unique_order_ids"] == expected_ids,
        "no_duplicate_events": ledger["duplicate_events"] == 0,
        "no_missing_order_ids": ledger["rows_without_order_id"] == 0,
        "metrics_partition_series": bool(
            result.get("metrics_checks", {}).get("committed_offset_series") == 12
            and result.get("metrics_checks", {}).get("lag_series") == 12
            and result.get("metrics_checks", {}).get("assigned_series") == 12
        ),
    }
    assignments = result.get("status_snapshot", {}).get("members", [])
    owned: list[int] = []
    for member in assignments:
        if member["status"] == "active":
            owned.extend(member["owned_partitions"])
    checks["active_assignments_exclusive"] = len(owned) == len(set(owned))
    if args.faults == "full":
        checks["heartbeat_timeout_exercised"] = (
            int(result.get("metrics_checks", {}).get("heartbeat_timeout_series", 0)) >= 1
        )
        checks["suppression_exercised"] = bool(
            result.get("metrics_checks", {}).get("suppression_present")
        )
        checks["pause_resume_exercised"] = bool(
            "pause" in result.get("observed_events", [])
            and "resume" in result.get("observed_events", [])
        )
    result["checks"] = checks
    result["passed"] = all(checks.values())
    result["finished_at"] = iso_now()
    report_path.write_text(
        json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    trace.add(
        "acceptance_finished",
        passed=result["passed"],
        checks=checks,
        ledger=ledger,
    )
    trace.close()
    print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

