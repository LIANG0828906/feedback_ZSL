#!/usr/bin/env python3
"""Deterministic 20 writes/s + 30 reads/s SLO probe for the HTTP service."""

from __future__ import annotations

import argparse
import http.client
import json
import math
import random
import threading
import time
from dataclasses import dataclass, field
from typing import Any


def p95(values: list[float]) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    return round(ordered[max(0, math.ceil(len(ordered) * 0.95) - 1)], 3)


@dataclass
class Results:
    writes: list[float] = field(default_factory=list)
    reads: list[float] = field(default_factory=list)
    errors: list[dict[str, Any]] = field(default_factory=list)


def request(
    connection: http.client.HTTPConnection,
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
) -> tuple[int, dict[str, Any], float]:
    headers = {"Content-Type": "application/json"}
    payload = (
        json.dumps(body, separators=(",", ":")).encode("utf-8")
        if body is not None
        else None
    )
    started = time.perf_counter()
    try:
        connection.request(method, path, body=payload, headers=headers)
        response = connection.getresponse()
        raw = response.read()
        elapsed = (time.perf_counter() - started) * 1000.0
        parsed = json.loads(raw.decode("utf-8")) if raw else {}
        return response.status, parsed, elapsed
    except Exception:
        connection.close()
        raise


def worker(
    *,
    host: str,
    port: int,
    kind: str,
    rate: float,
    workers: int,
    duration: float,
    start_at: float,
    order_ids: list[int],
    tenant_id: int,
    seed: int,
    results: Results,
    result_lock: threading.Lock,
) -> None:
    rng = random.Random(seed * 1000 + (0 if kind == "write" else 1))
    connection = http.client.HTTPConnection(host, port, timeout=10)
    interval = workers / rate if rate > 0 else duration
    index = 0
    while True:
        target = start_at + index * interval
        delay = target - time.perf_counter()
        if delay > 0:
            time.sleep(delay)
        if time.perf_counter() >= start_at + duration:
            break
        order_id = rng.choice(order_ids)
        try:
            if kind == "write":
                body = {
                    "orderId": order_id,
                    "tenantId": tenant_id,
                    "status": (index + seed) % 5,
                    "amount": f"{(index % 900000) / 100 + 10:.2f}",
                    "currency": ("CNY", "USD", "EUR")[index % 3],
                    "updatedAt": time.strftime(
                        "%Y-%m-%dT%H:%M:%SZ", time.gmtime()
                    ),
                }
                status, parsed, elapsed = request(
                    connection, "POST", "/api/v1/orders", body
                )
            else:
                status, parsed, elapsed = request(
                    connection,
                    "GET",
                    f"/api/v1/orders/{order_id}?tenantId={tenant_id}",
                )
            with result_lock:
                if status == 200 and parsed.get("ok"):
                    if kind == "write":
                        results.writes.append(elapsed)
                    else:
                        results.reads.append(elapsed)
                else:
                    results.errors.append(
                        {
                            "kind": kind,
                            "status": status,
                            "errorCode": parsed.get("errorCode"),
                            "elapsedMs": round(elapsed, 3),
                        }
                    )
        except Exception as exc:
            with result_lock:
                results.errors.append(
                    {"kind": kind, "exception": type(exc).__name__, "detail": str(exc)}
                )
            connection.close()
            connection = http.client.HTTPConnection(host, port, timeout=10)
        index += 1
    connection.close()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8090")
    parser.add_argument("--duration", type=float, default=20.0)
    parser.add_argument("--writes-rate", type=float, default=20.0)
    parser.add_argument("--reads-rate", type=float, default=30.0)
    parser.add_argument("--writer-workers", type=int, default=8)
    parser.add_argument("--reader-workers", type=int, default=8)
    parser.add_argument("--tenant-id", type=int, default=2)
    parser.add_argument("--order-ids", default="")
    parser.add_argument("--write-p95-ms", type=float, default=250.0)
    parser.add_argument("--read-p95-ms", type=float, default=120.0)
    parser.add_argument("--seed", type=int, default=289)
    parser.add_argument("--report-out")
    args = parser.parse_args()

    base = args.base_url.removeprefix("http://").removeprefix("https://").rstrip("/")
    host, port_text = base.split(":", 1)
    port = int(port_text)
    if args.order_ids:
        order_ids = [int(value) for value in args.order_ids.split(",") if value]
    else:
        # The deterministic seed assigns tenant 2 to order ids 2, 18, 34, ...
        order_ids = list(range(2, 20001, 16))[:256]
    results = Results()
    lock = threading.Lock()
    start_at = time.perf_counter() + 0.5
    threads: list[threading.Thread] = []
    for index in range(args.writer_workers):
        threads.append(
            threading.Thread(
                target=worker,
                kwargs={
                    "host": host,
                    "port": port,
                    "kind": "write",
                    "rate": args.writes_rate,
                    "workers": args.writer_workers,
                    "duration": args.duration,
                    "start_at": start_at,
                    "order_ids": order_ids,
                    "tenant_id": args.tenant_id,
                    "seed": args.seed + index,
                    "results": results,
                    "result_lock": lock,
                },
            )
        )
    for index in range(args.reader_workers):
        threads.append(
            threading.Thread(
                target=worker,
                kwargs={
                    "host": host,
                    "port": port,
                    "kind": "read",
                    "rate": args.reads_rate,
                    "workers": args.reader_workers,
                    "duration": args.duration,
                    "start_at": start_at,
                    "order_ids": order_ids,
                    "tenant_id": args.tenant_id,
                    "seed": args.seed + 100 + index,
                    "results": results,
                    "result_lock": lock,
                },
            )
        )
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    write_p95 = p95(results.writes)
    read_p95 = p95(results.reads)
    passed = (
        not results.errors
        and len(results.writes) >= int(args.writes_rate * args.duration * 0.9)
        and len(results.reads) >= int(args.reads_rate * args.duration * 0.9)
        and write_p95 <= args.write_p95_ms
        and read_p95 <= args.read_p95_ms
    )
    report = {
        "ok": passed,
        "durationSeconds": args.duration,
        "targets": {
            "writesPerSecond": args.writes_rate,
            "readsPerSecond": args.reads_rate,
            "writeP95Ms": args.write_p95_ms,
            "readP95Ms": args.read_p95_ms,
        },
        "observed": {
            "writeSamples": len(results.writes),
            "readSamples": len(results.reads),
            "writeP95Ms": write_p95,
            "readP95Ms": read_p95,
            "writeMaxMs": round(max(results.writes), 3) if results.writes else 0,
            "readMaxMs": round(max(results.reads), 3) if results.reads else 0,
            "errors": results.errors[:100],
            "errorCount": len(results.errors),
        },
    }
    encoded = json.dumps(report, ensure_ascii=False, indent=2)
    print(encoded)
    if args.report_out:
        with open(args.report_out, "w", encoding="utf-8") as handle:
            handle.write(encoded + "\n")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
