#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
import urllib.request


def get_json(url: str) -> dict:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(url, timeout=10) as response:
        return json.loads(response.read().decode("utf-8"))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("report")
    parser.add_argument("--coordinator", default="http://127.0.0.1:18080")
    parser.add_argument("--tenant", default="tenant-07")
    args = parser.parse_args()
    with open(args.report, "r", encoding="utf-8") as handle:
        report = json.load(handle)
    tenant = next(
        (item for item in report["tenants"] if item["tenant_id"] == args.tenant),
        None,
    )
    if tenant is None:
        print(f"tenant {args.tenant} missing from report", file=sys.stderr)
        return 2
    stats = get_json(
        f"{args.coordinator}/v1/receiver-stats/{args.tenant}"
    )
    failures: list[str] = []
    if tenant["queue_depth"] != 0 or tenant["inflight"] != 0:
        failures.append("events remain queued or in flight after restart")
    if tenant["delivered"] + tenant["dead_letters"] != tenant["events_submitted"]:
        failures.append("submitted count does not match terminal event count")
    if tenant["dead_letters"] != 0:
        failures.append("healthy restart scenario unexpectedly produced dead letters")
    if stats.get("duplicates") != 0:
        failures.append(
            f"receiver observed {stats.get('duplicates')} duplicate deliveries"
        )
    if stats.get("accepted") != tenant["delivered"]:
        failures.append(
            "receiver accepted count does not equal coordinator delivered count"
        )
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print(
        json.dumps(
            {
                "accepted": stats["accepted"],
                "delivered": tenant["delivered"],
                "duplicates": stats["duplicates"],
                "status": "pass",
                "submitted": tenant["events_submitted"],
                "tenant_id": args.tenant,
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

