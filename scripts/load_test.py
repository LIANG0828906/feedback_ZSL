#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import math
import sys
import threading
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

import grpc
import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from inference_router.pb import inference_pb2, inference_pb2_grpc  # noqa: E402


def percentile(values: list[float], quantile: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    rank = max(0, min(len(ordered) - 1, math.ceil(quantile * len(ordered)) - 1))
    return float(ordered[rank])


def run_load(
    target: str,
    *,
    requests: int,
    rate: float,
    concurrency: int,
    seed: int,
    request_prefix: str = "req",
    session_prefix: str = "sess",
    feature_dim: int = 64,
    client_timeout_s: float = 2.0,
) -> dict[str, Any]:
    rng = np.random.default_rng(seed)
    stream = [
        rng.normal(0.0, 1.0, size=feature_dim).astype(np.float32).tolist()
        for _ in range(requests)
    ]
    channel = grpc.insecure_channel(
        target,
        options=[
            ("grpc.max_send_message_length", 4 * 1024 * 1024),
            ("grpc.max_receive_message_length", 4 * 1024 * 1024),
        ],
    )
    stub = inference_pb2_grpc.RouterInferenceStub(channel)
    results: list[dict[str, Any]] = []
    results_lock = threading.Lock()
    scheduling_lag_ms: list[float] = []
    start = time.perf_counter()

    def invoke(index: int) -> None:
        request_id = f"{request_prefix}-{index:06d}"
        session_id = f"{session_prefix}-{index:06d}"
        call_started = time.perf_counter()
        record: dict[str, Any] = {
            "request_id": request_id,
            "session_id": session_id,
            "ok": False,
            "error_code": "",
            "e2e_ms": 0.0,
            "calls": [],
            "retry_switches": 0,
            "deduplicated": False,
            "session_violation": False,
        }
        try:
            reply = stub.Infer(
                inference_pb2.InferRequest(
                    request_id=request_id,
                    session_id=session_id,
                    features=stream[index],
                ),
                timeout=client_timeout_s,
            )
            record["ok"] = bool(reply.ok)
            record["error_code"] = reply.error_code
            record["retry_switches"] = int(reply.retry_switches)
            record["deduplicated"] = bool(reply.deduplicated)
            record["calls"] = [
                {
                    "layer_id": call.layer_id,
                    "replica_id": call.replica_id,
                    "session_id": call.session_id,
                    "latency_ms": float(call.latency_ms),
                    "timed_out": bool(call.timed_out),
                    "attempt": int(call.attempt),
                    "outcome": call.outcome,
                }
                for call in reply.calls
            ]
            record["session_violation"] = any(
                call.session_id != session_id for call in reply.calls
            )
        except grpc.RpcError as exc:
            record["error_code"] = f"GRPC_{exc.code().name}"
        except Exception as exc:
            record["error_code"] = f"CLIENT_{type(exc).__name__}"
        record["e2e_ms"] = (time.perf_counter() - call_started) * 1000.0
        with results_lock:
            results.append(record)

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        for index in range(requests):
            target_time = start + index / rate
            delay = target_time - time.perf_counter()
            if delay > 0:
                time.sleep(delay)
            scheduling_lag_ms.append(
                max(0.0, (time.perf_counter() - target_time) * 1000.0)
            )
            pool.submit(invoke, index)
    wall_s = time.perf_counter() - start
    channel.close()

    success = sum(1 for item in results if item["ok"])
    errors = len(results) - success
    e2e = [float(item["e2e_ms"]) for item in results]
    error_codes = Counter(
        item["error_code"] for item in results if not item["ok"]
    )
    layer_calls = Counter()
    layer_timeouts = Counter()
    replica_calls = Counter()
    replica_timeouts = Counter()
    for item in results:
        for call in item["calls"]:
            layer_calls[call["layer_id"]] += 1
            replica_calls[f"{call['layer_id']}/{call['replica_id']}"] += 1
            if call["timed_out"]:
                layer_timeouts[call["layer_id"]] += 1
                replica_timeouts[f"{call['layer_id']}/{call['replica_id']}"] += 1
    return {
        "schema_version": 1,
        "seed": seed,
        "target": target,
        "requested": requests,
        "rate_per_second": rate,
        "concurrency": concurrency,
        "wall_seconds": round(wall_s, 3),
        "achieved_rate_per_second": round(len(results) / wall_s, 3) if wall_s else 0.0,
        "scheduling_lag_p95_ms": round(percentile(scheduling_lag_ms, 0.95), 3),
        "total": len(results),
        "success": success,
        "errors": errors,
        "error_codes": dict(sorted(error_codes.items())),
        "session_violations": sum(1 for item in results if item["session_violation"]),
        "deduplicated": sum(1 for item in results if item["deduplicated"]),
        "layer_call_distribution": dict(sorted(layer_calls.items())),
        "layer_timeouts": dict(sorted(layer_timeouts.items())),
        "replica_call_distribution": dict(sorted(replica_calls.items())),
        "replica_timeouts": dict(sorted(replica_timeouts.items())),
        "retry_switches": sum(int(item["retry_switches"]) for item in results),
        "e2e_ms": {
            "p50": round(percentile(e2e, 0.50), 3),
            "p95": round(percentile(e2e, 0.95), 3),
            "p99": round(percentile(e2e, 0.99), 3),
            "max": round(max(e2e), 3) if e2e else 0.0,
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Deterministic fixed-seed gRPC load test")
    parser.add_argument("--target", default="127.0.0.1:18080")
    parser.add_argument("--requests", type=int, default=1200)
    parser.add_argument("--rate", type=float, default=120.0)
    parser.add_argument("--concurrency", type=int, default=32)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--request-prefix", default="req")
    parser.add_argument("--session-prefix", default="sess")
    parser.add_argument("--output")
    args = parser.parse_args()
    report = run_load(
        args.target,
        requests=args.requests,
        rate=args.rate,
        concurrency=args.concurrency,
        seed=args.seed,
        request_prefix=args.request_prefix,
        session_prefix=args.session_prefix,
    )
    rendered = json.dumps(report, indent=2, sort_keys=True)
    if args.output:
        Path(args.output).parent.mkdir(parents=True, exist_ok=True)
        Path(args.output).write_text(rendered + "\n", encoding="utf-8")
    print(rendered)
    return 0 if report["errors"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
