#!/usr/bin/env python3
"""固定速率写入压测：默认每秒 50 行（出库单+明细混合写）。

用法：python3 scripts/02_workload.py --duration 120 --rate 50
每个业务行单独提交，模拟峰值写入；结束后打印 P50/P95/P99 延迟与 TPS。
"""
from __future__ import annotations

import argparse
import random
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "pylibs"))
sys.path.insert(0, str(ROOT / "src"))

import psycopg  # noqa: E402

from config import load_config  # noqa: E402

STATUSES = ["CREATED", "PICKED", "SHIPPED", "SETTLED"]


def percentile(data: list[float], pct: float) -> float:
    if not data:
        return 0.0
    ordered = sorted(data)
    idx = min(len(ordered) - 1, int(len(ordered) * pct))
    return ordered[idx] * 1000


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--duration", type=int, default=120)
    parser.add_argument("--rate", type=int, default=50, help="每秒业务行数")
    args = parser.parse_args()

    cfg = load_config()
    rng = random.Random(7710319)
    latencies: list[float] = []
    interval = 1.0 / args.rate
    deadline = time.time() + args.duration
    written = 0
    next_tick = time.time()

    with psycopg.connect(cfg.primary_dsn) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT coalesce(max(order_id),0) FROM wms.outbound_order")
            start_id = cur.fetchone()[0]
        while time.time() < deadline:
            next_tick += interval
            started = time.perf_counter()
            with conn.cursor() as cur:
                cur.execute(
                    "INSERT INTO wms.outbound_order(order_no, warehouse, status, "
                    "total_amount) VALUES (%s,%s,%s,%s) RETURNING order_id",
                    (
                        f"SO-L{start_id + written:09d}",
                        rng.choice(["WH-EAST", "WH-WEST", "WH-NORTH"]),
                        rng.choice(STATUSES),
                        0,
                    ),
                )
                order_id = cur.fetchone()[0]
                amount_total = 0.0
                for line in range(rng.randint(1, 3)):
                    qty = rng.randint(1, 20)
                    price = round(rng.uniform(10, 500), 2)
                    amount = round(qty * price, 2)
                    amount_total += amount
                    cur.execute(
                        "INSERT INTO wms.settlement_detail(order_id, item_name, "
                        "quantity, unit_price, amount, status) "
                        "VALUES (%s,%s,%s,%s,%s,%s)",
                        (order_id, f"ITEM-L{rng.randint(1, 500):04d}", qty,
                         price, amount, rng.choice(["PENDING", "MATCHED"])),
                    )
                cur.execute(
                    "UPDATE wms.outbound_order SET total_amount=%s, updated_at=now() "
                    "WHERE order_id=%s",
                    (round(amount_total, 2), order_id),
                )
            conn.commit()
            latencies.append(time.perf_counter() - started)
            written += 1
            drift = next_tick - time.time()
            if drift > 0:
                time.sleep(drift)

    elapsed = args.duration
    print(
        f"rows={written} tps={written / elapsed:.1f} "
        f"p50={percentile(latencies, 0.50):.2f}ms "
        f"p95={percentile(latencies, 0.95):.2f}ms "
        f"p99={percentile(latencies, 0.99):.2f}ms"
    )


if __name__ == "__main__":
    main()
