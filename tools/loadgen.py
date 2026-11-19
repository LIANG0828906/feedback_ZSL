#!/usr/bin/env python3
from __future__ import annotations

import argparse
import http.client
import json
import os
import random
import threading
import time
from collections import Counter
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlencode, urlsplit


LOOPBACK_HOSTS = {"127.0.0.1", "localhost", "::1"}


def percentile(values: list[float], percent: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(int((percent / 100.0) * len(ordered) + 0.999999) - 1, 0)
    return round(ordered[min(index, len(ordered) - 1)], 3)


def latency_summary(values: list[float]) -> dict[str, Any]:
    return {
        "count": len(values),
        "p50_ms": percentile(values, 50),
        "p95_ms": percentile(values, 95),
        "p99_ms": percentile(values, 99),
        "max_ms": percentile(values, 100),
    }


def parse_mix(raw: str) -> list[tuple[str, float]]:
    result: list[tuple[str, float]] = []
    for item in raw.split(","):
        name, separator, value = item.partition("=")
        name = name.strip().lower()
        if not separator or name not in {"strong", "session", "eventual"}:
            raise argparse.ArgumentTypeError(
                "consistency mix entries must look like strong=0.2"
            )
        try:
            weight = float(value)
        except ValueError:
            raise argparse.ArgumentTypeError("consistency weight is not a number") from None
        if weight < 0:
            raise argparse.ArgumentTypeError("consistency weight must be non-negative")
        result.append((name, weight))
    if not result or sum(weight for _, weight in result) <= 0:
        raise argparse.ArgumentTypeError("consistency mix must have positive weight")
    return result


def weighted_choice(
    random_source: random.Random, choices: list[tuple[str, float]]
) -> str:
    total = sum(weight for _, weight in choices)
    point = random_source.random() * total
    upto = 0.0
    for value, weight in choices:
        upto += weight
        if point <= upto:
            return value
    return choices[-1][0]


class Transport:
    def __init__(self, base_url: str, timeout: float) -> None:
        self.base_url = base_url
        self.timeout = timeout
        parsed = urlsplit(base_url)
        if parsed.scheme != "http" or parsed.hostname not in LOOPBACK_HOSTS:
            raise SystemExit("base URL must be http on a loopback host")
        self.host = parsed.hostname or "127.0.0.1"
        self.port = parsed.port or 80
        self.prefix = parsed.path.rstrip("/")
        self._local = threading.local()

    def connection(self) -> http.client.HTTPConnection:
        connection = getattr(self._local, "connection", None)
        if connection is None:
            connection = http.client.HTTPConnection(
                self.host, self.port, timeout=self.timeout
            )
            self._local.connection = connection
        return connection

    def request(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, dict[str, Any]]:
        request_headers = {"Accept": "application/json"}
        if headers:
            request_headers.update(headers)
        raw_body = None
        if body is not None:
            raw_body = json.dumps(body, sort_keys=True, separators=(",", ":")).encode(
                "utf-8"
            )
            request_headers["Content-Type"] = "application/json"
        connection = self.connection()
        try:
            connection.request(
                method,
                f"{self.prefix}{path}",
                body=raw_body,
                headers=request_headers,
            )
            response = connection.getresponse()
            payload = response.read()
            try:
                parsed = json.loads(payload.decode("utf-8")) if payload else {}
            except (UnicodeDecodeError, json.JSONDecodeError):
                parsed = {"error": {"code": "E_NON_JSON_RESPONSE"}}
            return response.status, parsed
        except Exception:
            connection.close()
            self._local.connection = None
            raise


class SharedState:
    def __init__(self, session_id: str) -> None:
        self._lock = threading.Lock()
        self.session_id = session_id
        self._watermark = 0
        self._recent: list[tuple[str, int]] = []

    def record_write(self, order_no: str, watermark: int, session_id: str) -> None:
        with self._lock:
            self.session_id = session_id
            self._watermark = max(self._watermark, watermark)
            self._recent.append((order_no, watermark))
            if len(self._recent) > 2000:
                del self._recent[:1000]

    def session_position(self) -> tuple[str, int]:
        with self._lock:
            return self.session_id, self._watermark

    def recent_order(self, random_source: random.Random) -> str | None:
        with self._lock:
            if not self._recent:
                return None
            return self._recent[random_source.randrange(len(self._recent))][0]


def build_operations(
    args: argparse.Namespace, read_mix: list[tuple[str, float]]
) -> list[dict[str, Any]]:
    random_source = random.Random(args.seed)
    operations: list[dict[str, Any]] = []
    writes_left = args.writes
    reads_left = args.reads
    for index in range(args.writes + args.reads):
        choose_write = False
        total_left = writes_left + reads_left
        if total_left:
            if writes_left <= 0:
                choose_write = False
            elif reads_left <= 0:
                choose_write = True
            else:
                choose_write = random_source.random() < (
                    writes_left / total_left
                )
        if choose_write:
            writes_left -= 1
            operations.append(
                {
                    "kind": "write",
                    "index": index,
                    "order_no": f"LOAD-{args.run_id}-{index:08d}",
                }
            )
        else:
            reads_left -= 1
            consistency = weighted_choice(random_source, read_mix)
            operations.append(
                {
                    "kind": "read",
                    "index": index,
                    "consistency": consistency,
                    "list": args.list_every > 0
                    and index % args.list_every == 0,
                    "sample": random_source.randrange(1, 20001),
                }
            )
    return operations


def write_body(order_no: str, tenant: str, status: str, index: int) -> dict[str, Any]:
    unit_price = f"{10 + (index % 90)}.00"
    created = f"2025-02-01T{(index // 3600) % 24:02d}:{(index // 60) % 60:02d}:{index % 60:02d}.000Z"
    return {
        "order_no": order_no,
        "tenant_id": tenant,
        "status": status,
        "created_at_utc": created,
        "lines": [
            {
                "line_no": 1,
                "sku": f"LOAD-SKU-{index % 500:04d}",
                "quantity": 1,
                "unit_price": unit_price,
            },
            {
                "line_no": 2,
                "sku": f"LOAD-SKU-{(index + 1) % 500:04d}",
                "quantity": 1,
                "unit_price": "1.00",
            },
        ],
    }


def run_worker(
    worker_id: int,
    args: argparse.Namespace,
    transport: Transport,
    operations: list[dict[str, Any]],
    state: SharedState,
    next_index: list[int],
    index_lock: threading.Lock,
    start_time: float,
    result: dict[str, Any],
    result_lock: threading.Lock,
) -> None:
    random_source = random.Random(args.seed + worker_id)
    while True:
        with index_lock:
            index = next_index[0]
            if index >= len(operations):
                return
            next_index[0] += 1
        operation = operations[index]
        if args.rate > 0:
            scheduled = start_time + index / args.rate
            delay = scheduled - time.monotonic()
            if delay > 0:
                time.sleep(delay)

        started = time.monotonic()
        status_code = 0
        error_code: str | None = None
        kind = operation["kind"]
        consistency = operation.get("consistency", "strong")
        try:
            if kind == "write":
                code, payload = transport.request(
                    "POST",
                    "/orders",
                    body=write_body(
                        operation["order_no"],
                        args.tenant,
                        args.status,
                        operation["index"],
                    ),
                    headers={"X-Session-Id": state.session_position()[0]},
                )
                status_code = code
                if code == 200:
                    watermark = int(payload["commit_watermark"])
                    state.record_write(
                        operation["order_no"],
                        watermark,
                        str(payload["session_id"]),
                    )
                else:
                    error_code = str(
                        payload.get("error", {}).get("code", "E_UNKNOWN")
                    )
            else:
                session_id, watermark = state.session_position()
                headers = {
                    "X-Session-Id": session_id,
                    "X-Min-Watermark": str(watermark),
                }
                if operation["list"]:
                    code, payload = transport.request(
                        "GET",
                        "/orders?"
                        + urlencode(
                            {
                                "tenant_id": args.tenant,
                                "status": args.status,
                                "page": 1,
                                "page_size": args.page_size,
                                "consistency": consistency,
                            }
                        ),
                        headers=headers,
                    )
                else:
                    order_no = state.recent_order(random_source)
                    if order_no is None:
                        order_no = f"ORD-{operation['sample']:06d}"
                    code, payload = transport.request(
                        "GET",
                        f"/orders/{quote(order_no, safe='')}?consistency={consistency}",
                        headers=headers,
                    )
                status_code = code
                if code != 200:
                    error_code = str(
                        payload.get("error", {}).get("code", "E_UNKNOWN")
                    )
        except Exception as exc:
            status_code = 0
            error_code = f"E_TRANSPORT_{type(exc).__name__}"
        elapsed_ms = (time.monotonic() - started) * 1000.0

        with result_lock:
            result["requests"] += 1
            result["by_kind"][kind] += 1
            if kind == "read":
                result["by_consistency"][consistency] += 1
            result["status_codes"][str(status_code)] += 1
            if error_code:
                result["failure_codes"][error_code] += 1
            else:
                result["successes"] += 1
            result["latencies"].append(elapsed_ms)
            result[f"{kind}_latencies"].append(elapsed_ms)


def main() -> int:
    parser = argparse.ArgumentParser(description="Local order workload generator")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--writes", type=int, default=200)
    parser.add_argument("--reads", type=int, default=800)
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--rate", type=float, default=50.0)
    parser.add_argument("--tenant", default="T01")
    parser.add_argument("--status", default="CONFIRMED")
    parser.add_argument(
        "--read-consistency-mix",
        type=parse_mix,
        default=parse_mix("strong=0.2,session=0.4,eventual=0.4"),
    )
    parser.add_argument("--seed", type=int, default=271)
    parser.add_argument("--run-id", default="R0271")
    parser.add_argument("--page-size", type=int, default=50)
    parser.add_argument("--list-every", type=int, default=10)
    parser.add_argument("--fail-on-error", action="store_true")
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--admin-token", default=os.environ.get("ORDERSYNC_ADMIN_TOKEN"))
    parser.add_argument("--reset-report", action="store_true")
    parser.add_argument(
        "--max-write-p95-ms", type=float, default=None
    )
    parser.add_argument(
        "--max-strong-read-p95-ms", type=float, default=None
    )
    parser.add_argument("--enforce-slo", action="store_true")
    parser.add_argument(
        "--report-out", default="artifacts/load-report.json"
    )
    args = parser.parse_args()
    if args.writes < 0 or args.reads < 0 or args.concurrency < 1:
        parser.error("writes/reads must be non-negative and concurrency must be positive")
    if args.writes + args.reads == 0:
        parser.error("at least one operation is required")

    transport = Transport(args.base_url, args.timeout)
    admin_headers = (
        {"X-Admin-Token": args.admin_token} if args.admin_token else {}
    )
    if args.reset_report:
        status, payload = transport.request(
            "POST", "/admin/report/reset", headers=admin_headers
        )
        if status != 200:
            code = payload.get("error", {}).get("code", "E_REPORT_RESET")
            raise SystemExit(f"report reset failed: {code}")

    operations = build_operations(args, args.read_consistency_mix)
    result: dict[str, Any] = {
        "requests": 0,
        "successes": 0,
        "by_kind": Counter(),
        "by_consistency": Counter(),
        "status_codes": Counter(),
        "failure_codes": Counter(),
        "latencies": [],
        "read_latencies": [],
        "write_latencies": [],
    }
    result_lock = threading.Lock()
    index_lock = threading.Lock()
    next_index = [0]
    state = SharedState("loadgen-session")
    started = time.monotonic()
    threads = [
        threading.Thread(
            target=run_worker,
            args=(
                worker_id,
                args,
                transport,
                operations,
                state,
                next_index,
                index_lock,
                started,
                result,
                result_lock,
            ),
            name=f"load-worker-{worker_id}",
        )
        for worker_id in range(args.concurrency)
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    elapsed_seconds = time.monotonic() - started

    service_report: dict[str, Any] | None = None
    try:
        _, service_report = transport.request("GET", "/report")
    except Exception:
        service_report = None
    summary = {
        "schema_version": 1,
        "operations": {
            "requests": result["requests"],
            "successes": result["successes"],
            "by_kind": dict(sorted(result["by_kind"].items())),
            "by_consistency": dict(sorted(result["by_consistency"].items())),
            "status_codes": dict(sorted(result["status_codes"].items())),
            "failure_codes": dict(sorted(result["failure_codes"].items())),
        },
        "throughput_ops_per_second": round(
            result["requests"] / elapsed_seconds, 3
        )
        if elapsed_seconds > 0
        else 0.0,
        "latency_ms": {
            "all": latency_summary(result["latencies"]),
            "reads": latency_summary(result["read_latencies"]),
            "writes": latency_summary(result["write_latencies"]),
        },
        "service_report": service_report,
    }
    write_p95 = None
    strong_p95 = None
    if service_report is not None:
        try:
            write_p95 = service_report["writes"]["latency_ms"]["p95_ms"]
            strong_p95 = service_report["read_latency_ms"]["by_consistency"][
                "strong"
            ]["p95_ms"]
        except (KeyError, TypeError):
            pass
    slo_checks: dict[str, Any] = {}
    if args.max_write_p95_ms is not None:
        slo_checks["write_p95"] = {
            "actual_ms": write_p95,
            "maximum_ms": args.max_write_p95_ms,
            "passed": write_p95 is not None
            and float(write_p95) <= args.max_write_p95_ms,
        }
    if args.max_strong_read_p95_ms is not None:
        slo_checks["strong_read_p95"] = {
            "actual_ms": strong_p95,
            "maximum_ms": args.max_strong_read_p95_ms,
            "passed": strong_p95 is not None
            and float(strong_p95) <= args.max_strong_read_p95_ms,
        }
    summary["slo"] = {
        "checks": slo_checks,
        "passed": all(
            bool(check["passed"]) for check in slo_checks.values()
        )
        if slo_checks
        else None,
    }
    if args.report_out:
        destination = Path(args.report_out)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(
            json.dumps(summary, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    print(json.dumps(summary, sort_keys=True, separators=(",", ":")))
    if args.fail_on_error and result["successes"] != result["requests"]:
        return 1
    if args.enforce_slo and slo_checks and not summary["slo"]["passed"]:
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

