"""End-to-end acceptance driver: mixed traffic, slow inference and rollback.

Assumes the gateway is already running (python -m gateway.main --config ...).
Replays the deterministic sample file, injects slow v4 calls, and reports
routing distribution, error codes, circuit-breaker rollback and queue isolation.
"""

from __future__ import annotations

import argparse
import json
import time
from collections import Counter
from pathlib import Path

import requests


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:8000")
    parser.add_argument(
        "--samples", default="artifacts/samples/sample_requests.jsonl"
    )
    parser.add_argument("--slow-count", type=int, default=12)
    parser.add_argument("--slow-delay-ms", type=float, default=200.0)
    args = parser.parse_args()

    session = requests.Session()
    session.trust_env = False
    samples = [
        json.loads(line)
        for line in Path(args.samples).read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]

    print(f"replaying {len(samples)} deterministic samples ...")
    routing = Counter()
    error_codes = Counter()
    for sample in samples:
        resp = session.post(f"{args.base_url}/v1/rank", json=sample, timeout=5)
        body = resp.json()
        if body.get("code") == "OK":
            routing[body["model_version"]] += 1
        else:
            error_codes[body["code"]] += 1
    print("routing distribution:", dict(routing))
    print("business error codes:", dict(error_codes))

    print(
        f"injecting {args.slow_count} slow v4 calls "
        f"({args.slow_delay_ms}ms each) ..."
    )
    # Replay traffic includes deliberately mis-ordered payloads, which may have
    # tripped the error-rate breaker. Reset the breaker together with the weight
    # so the slow-call phase demonstrates the latency (P99) path in isolation.
    print("resetting circuit breaker and pinning canary weight to 1.0 ...")
    session.post(
        f"{args.base_url}/v1/admin/weight",
        json={"weight": 1.0, "reset_circuit_breaker": True},
        timeout=5,
    )
    session.post(
        f"{args.base_url}/v1/admin/slow-calls",
        json={
            "version": "v4",
            "count": args.slow_count,
            "delay_ms": args.slow_delay_ms,
        },
        timeout=5,
    )
    session.post(
        f"{args.base_url}/v1/admin/weight", json={"weight": 1.0}, timeout=5
    )

    v4_columns = [
        "c_item_price", "c_item_age", "c_match_score", "c_ctr_7d",
        "c_cvr_7d", "c_dwell_mean", "c_user_age", "c_user_tenure",
        "c_emb_cosine", "c_ctx_hour",
    ]
    slow_payloads = []
    for index in range(10):
        slow_payloads.append(
            {
                "request_id": f"slow-demo-{index}",
                "tenant_id": "tenant-c",
                "client_id": "demo-slow",
                "protocol_version": "rank-features-v2",
                "columns": v4_columns,
                "features": [0.05] * len(v4_columns),
            }
        )
    canary_latency = []
    for payload in slow_payloads:
        start = time.perf_counter()
        body = session.post(
            f"{args.base_url}/v1/rank", json=payload, timeout=10
        ).json()
        canary_latency.append((time.perf_counter() - start) * 1000.0)
        if body.get("code") != "OK":
            print(f"  request {payload['request_id']} rolled back: {body['code']}")
    print(f"canary latency samples (ms): {[round(v, 1) for v in canary_latency]}")

    status = session.get(f"{args.base_url}/v1/status", timeout=5).json()
    breaker = status["circuit_breaker"]
    print("circuit breaker tripped:", breaker["tripped"])
    if breaker["last_rollback"]:
        record = breaker["last_rollback"]
        print(
            "rollback record:",
            json.dumps(
                {
                    "canary_version": record["canary_version"],
                    "sample_count": record["sample_count"],
                    "reasons": record["reasons"],
                    "window_timeout_count": record["window_timeout_count"],
                },
                indent=2,
            ),
        )
    print("weight after rollback:", status["routing"])

    # Stable version must remain available throughout the rollback window.
    v3_columns = [
        "c_user_age", "c_user_tenure", "c_item_age", "c_item_price",
        "c_ctr_7d", "c_cvr_7d", "c_match_score", "c_dwell_mean",
    ]
    stable = session.post(
        f"{args.base_url}/v1/rank",
        json={
            "request_id": "stable-check",
            "tenant_id": "tenant-a",
            "client_id": "demo-stable",
            "protocol_version": "rank-features-v1",
            "columns": v3_columns,
            "features": [0.1] * len(v3_columns),
        },
        timeout=5,
    ).json()
    print("post-rollback stable request:", stable["code"], stable.get("model_version"))
    assert stable["code"] == "OK" and stable["model_version"] == "v3"


if __name__ == "__main__":
    main()
