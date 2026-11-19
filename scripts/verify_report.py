#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Check the lab acceptance conditions in a scenario report"
    )
    parser.add_argument("report")
    parser.add_argument("--slow-tenant", default="tenant-17")
    parser.add_argument("--p95-limit-ms", type=float, default=500.0)
    args = parser.parse_args()
    with open(args.report, "r", encoding="utf-8") as handle:
        report = json.load(handle)
    by_id = {item["tenant_id"]: item for item in report["tenants"]}
    slow = by_id.get(args.slow_tenant)
    if slow is None:
        print(f"missing tenant {args.slow_tenant}", file=sys.stderr)
        return 2
    healthy = [
        item
        for item in report["tenants"]
        if item["tenant_id"] != args.slow_tenant
        and item["events_submitted"] > 0
    ]
    failures: list[str] = []
    if not slow["degraded"] and slow["degradation_ms"] == 0:
        failures.append("slow tenant was never degraded")
    if slow["dead_letters"] <= 0:
        failures.append("slow tenant produced no dead letters")
    if not healthy:
        failures.append("scenario contains no comparison tenant")
    for tenant in healthy:
        if tenant["p95_latency_ms"] > args.p95_limit_ms:
            failures.append(
                f"{tenant['tenant_id']} p95 {tenant['p95_latency_ms']}ms "
                f"> {args.p95_limit_ms}ms"
            )
        if tenant["success_rate"] < 1.0:
            failures.append(
                f"{tenant['tenant_id']} success rate {tenant['success_rate']} < 1"
            )
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print(
        json.dumps(
            {
                "healthy_tenants": sorted(
                    item["tenant_id"] for item in healthy
                ),
                "p95_limit_ms": args.p95_limit_ms,
                "slow_dead_letters": slow["dead_letters"],
                "slow_degradation_ms": slow["degradation_ms"],
                "status": "pass",
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

