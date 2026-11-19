#!/usr/bin/env python3
"""Generate bidirectional load, wait for convergence and verify both databases."""

from __future__ import annotations

import argparse
import json
import random
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import psycopg

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from dualwrite.config import load_config


def http_json(url: str, method: str = "GET", body: dict | None = None) -> dict:
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        payload = exc.read().decode("utf-8", "replace")
        raise RuntimeError(f"HTTP {exc.code}: {payload}") from exc


def wait_pending(api: str, timeout: float) -> dict:
    deadline = time.monotonic() + timeout
    last: dict = {}
    while time.monotonic() < deadline:
        last = http_json(f"{api}/v1/status")
        pending = sum(last.get("pending_rows_by_target", {}).values())
        if pending == 0 and last.get("in_flight_deliveries", 0) == 0:
            return last
        time.sleep(0.5)
    raise TimeoutError(f"did not converge within {timeout}s: {last}")


def wait_validation(api: str, batch_id: str, timeout: float) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        payload = http_json(f"{api}/v1/validations/{batch_id}?limit=1000")
        if payload["batch"]["status"] == "completed":
            return payload
        time.sleep(0.5)
    raise TimeoutError(f"validation {batch_id} did not complete")


def percentile(values: list[float], fraction: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, int(len(ordered) * fraction) - 1))
    return round(ordered[index], 3)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="config.example.json")
    parser.add_argument("--api", default=None)
    parser.add_argument("--rate", type=float, default=30.0, help="total changes per second")
    parser.add_argument("--duration", type=float, default=10.0)
    parser.add_argument("--converge-timeout", type=float, default=120.0)
    parser.add_argument("--validation-timeout", type=float, default=120.0)
    parser.add_argument("--report", default="artifacts/load_report.json")
    args = parser.parse_args()

    config = load_config(args.config)
    api = args.api or f"http://{config.listen_host}:{config.listen_port}"
    east = psycopg.connect(config.sites["east"].dsn)
    south = psycopg.connect(config.sites["south"].dsn)
    member_ids = list(range(1, 201))
    order_ids = list(range(1, 301))

    baselines = {
        row[0]: int(row[1])
        for row in east.execute(
            "SELECT member_id, points FROM members WHERE member_id = ANY(%s)",
            (member_ids,),
        ).fetchall()
    }
    deltas = {row_id: 0 for row_id in member_ids}
    lock = threading.Lock()
    latencies: list[float] = []
    errors: list[str] = []
    store: dict[str, dict] = {}

    def worker(site: str, conn: psycopg.Connection, per_site_rate: float):
        rng = random.Random(20261119 + (1 if site == "east" else 2))
        stop_at = time.monotonic() + args.duration
        interval = 1.0 / max(0.1, per_site_rate)
        next_at = time.monotonic()
        local_latencies: list[float] = []
        local_deltas: dict[int, int] = {}
        change_count = 0
        while time.monotonic() < stop_at:
            now = time.monotonic()
            if now < next_at:
                time.sleep(min(0.005, next_at - now))
                continue
            try:
                started = time.perf_counter()
                if change_count % 2 == 0:
                    member_id = rng.choice(member_ids)
                    delta = rng.randint(1, 9)
                    conn.execute(
                        "UPDATE members SET points = points + %s WHERE member_id = %s",
                        (delta, member_id),
                    )
                    local_deltas[member_id] = local_deltas.get(member_id, 0) + delta
                else:
                    order_id = rng.choice(order_ids)
                    status = rng.choice(["completed", "pending_shipment"])
                    conn.execute(
                        "UPDATE orders SET status = %s WHERE order_id = %s",
                        (status, order_id),
                    )
                conn.commit()
                local_latencies.append((time.perf_counter() - started) * 1000.0)
                change_count += 1
            except Exception as exc:
                conn.rollback()
                with lock:
                    errors.append(f"{site}: {type(exc).__name__}: {exc}")
            next_at += interval
        with lock:
            latencies.extend(local_latencies)
            for row_id, delta in local_deltas.items():
                deltas[row_id] += delta
            store[site] = {"changes": change_count}

    threads = [
        threading.Thread(
            target=worker,
            args=("east", east, args.rate / 2),
            name="load-east",
        ),
        threading.Thread(
            target=worker,
            args=("south", south, args.rate / 2),
            name="load-south",
        ),
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    status = wait_pending(api, args.converge_timeout)
    expected_bad = []
    final_points = {}
    for site, conn in (("east", east), ("south", south)):
        final_points[site] = {
            row[0]: int(row[1])
            for row in conn.execute(
                "SELECT member_id, points FROM members WHERE member_id = ANY(%s)",
                (member_ids,),
            ).fetchall()
        }
        for row_id in member_ids:
            expected = baselines[row_id] + deltas[row_id]
            if final_points[site][row_id] != expected:
                expected_bad.append(
                    {
                        "site": site,
                        "member_id": row_id,
                        "expected": expected,
                        "actual": final_points[site][row_id],
                    }
                )

    trace_latencies = []
    for member_id in member_ids[:25]:
        started = time.perf_counter()
        http_json(f"{api}/v1/trace/member/{member_id}?limit=20")
        trace_latencies.append((time.perf_counter() - started) * 1000.0)

    east_members = east.execute(
        "SELECT member_id, points, row_version, updated_at "
        "FROM members ORDER BY member_id"
    ).fetchall()
    south_members = south.execute(
        "SELECT member_id, points, row_version, updated_at "
        "FROM members ORDER BY member_id"
    ).fetchall()
    east_orders = east.execute(
        "SELECT order_id, status, row_version, updated_at "
        "FROM orders ORDER BY order_id"
    ).fetchall()
    south_orders = south.execute(
        "SELECT order_id, status, row_version, updated_at "
        "FROM orders ORDER BY order_id"
    ).fetchall()
    full_row_mismatches = {
        "members": sum(
            1 for left, right in zip(east_members, south_members) if left != right
        ),
        "orders": sum(
            1 for left, right in zip(east_orders, south_orders) if left != right
        ),
    }
    if len(east_members) != len(south_members):
        full_row_mismatches["members"] += abs(
            len(east_members) - len(south_members)
        )
    if len(east_orders) != len(south_orders):
        full_row_mismatches["orders"] += abs(len(east_orders) - len(south_orders))

    batch = http_json(f"{api}/v1/validate", "POST", {"scope": "all"})
    validation = wait_validation(api, batch["batch_id"], args.validation_timeout)
    summary = validation["batch"]["summary"]

    duplicate_deliveries = 0
    sqlite_path = Path(config.audit.sqlite_path)
    if sqlite_path.exists():
        import sqlite3

        audit = sqlite3.connect(sqlite_path)
        try:
            duplicate_deliveries = int(
                audit.execute(
                    "SELECT COUNT(*) FROM deliveries WHERE duplicate_of IS NOT NULL "
                    "OR outcome = 'duplicate'"
                ).fetchone()[0]
            )
        finally:
            audit.close()

    report = {
        "api": api,
        "changes_by_site": store,
        "duplicate_deliveries": duplicate_deliveries,
        "errors": errors,
        "full_row_mismatches": full_row_mismatches,
        "load": {
            "duration_seconds": args.duration,
            "total_changes": sum(item["changes"] for item in store.values()),
            "write_p50_ms": round(statistics.median(latencies), 3) if latencies else 0,
            "write_p95_ms": percentile(latencies, 0.95),
            "write_max_ms": round(max(latencies), 3) if latencies else 0,
        },
        "ok": (
            not errors
            and not expected_bad
            and not any(full_row_mismatches.values())
            and summary.get("mismatch_count", 0) == 0
        ),
        "pending_rows_by_target": status.get("pending_rows_by_target", {}),
        "points_errors": expected_bad,
        "trace": {
            "p50_ms": round(statistics.median(trace_latencies), 3),
            "p95_ms": percentile(trace_latencies, 0.95),
            "samples": len(trace_latencies),
        },
        "validation": {
            "batch_id": batch["batch_id"],
            "summary": summary,
        },
    }
    Path(args.report).parent.mkdir(parents=True, exist_ok=True)
    Path(args.report).write_text(
        json.dumps(report, indent=2, sort_keys=True), encoding="utf-8"
    )
    print(json.dumps(report, indent=2, sort_keys=True))
    east.close()
    south.close()
    return 0 if report["ok"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
