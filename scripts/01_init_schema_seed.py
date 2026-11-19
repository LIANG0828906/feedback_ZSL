#!/usr/bin/env python3
"""在主备两实例上建表，并用固定随机种子生成初始数据。

主库：6 万出库单 + 12 万结算明细；备库：仅建表（初始为空）。
重复执行不会产生重复数据：检测到已有数据时跳过种子写入。
"""
from __future__ import annotations

import os
import random
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "pylibs"))
sys.path.insert(0, str(ROOT / "src"))

import psycopg  # noqa: E402

from config import load_config  # noqa: E402

ORDER_STATUSES = ["CREATED", "PICKED", "SHIPPED", "SETTLED", "CLOSED"]
DETAIL_STATUSES = ["PENDING", "MATCHED", "SETTLED"]
WAREHOUSES = ["WH-EAST", "WH-WEST", "WH-NORTH", "WH-SOUTH", "WH-CENTRAL"]


def apply_schema(dsn: str) -> None:
    ddl = (ROOT / "sql" / "schema.sql").read_text(encoding="utf-8")
    with psycopg.connect(dsn, autocommit=True) as conn:
        with conn.cursor() as cur:
            cur.execute(ddl)


def seed_primary(dsn: str, n_orders: int = 60_000) -> None:
    rng = random.Random(20261119)
    with psycopg.connect(dsn) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT count(*) FROM wms.outbound_order")
            existing = cur.fetchone()[0]
            if existing:
                print(f"primary already has {existing} orders, seed skipped")
                return
            order_rows = []
            detail_rows = []
            detail_counter = 0
            for oid in range(1, n_orders + 1):
                order_rows.append(
                    (
                        oid,
                        f"SO-{oid:08d}",
                        rng.choice(WAREHOUSES),
                        rng.choice(ORDER_STATUSES),
                        0,
                    )
                )
                for _ in range(2):
                    detail_counter += 1
                    qty = rng.randint(1, 50)
                    price = round(rng.uniform(5, 900), 2)
                    detail_rows.append(
                        (
                            detail_counter,
                            oid,
                            f"ITEM-{rng.randint(1, 2000):05d}",
                            qty,
                            price,
                            round(qty * price, 2),
                            rng.choice(DETAIL_STATUSES),
                        )
                    )
            with cur.copy(
                "COPY wms.outbound_order "
                "(order_id, order_no, warehouse, status, total_amount) FROM STDIN"
            ) as copy:
                for row in order_rows:
                    copy.write_row(row)
            with cur.copy(
                "COPY wms.settlement_detail "
                "(detail_id, order_id, item_name, quantity, unit_price, amount, status) "
                "FROM STDIN"
            ) as copy:
                for row in detail_rows:
                    copy.write_row(row)
            cur.execute(
                """
                UPDATE wms.outbound_order o SET total_amount = sub.amt,
                       updated_at = now()
                FROM (SELECT order_id, sum(amount) AS amt
                      FROM wms.settlement_detail GROUP BY order_id) sub
                WHERE o.order_id = sub.order_id
                """
            )
            cur.execute(
                "SELECT setval('wms.outbound_order_order_id_seq', "
                "(SELECT max(order_id) FROM wms.outbound_order))"
            )
            cur.execute(
                "SELECT setval('wms.settlement_detail_detail_id_seq', "
                "(SELECT max(detail_id) FROM wms.settlement_detail))"
            )
        conn.commit()
    print(f"seed done: {n_orders} orders, {detail_counter} details")


def main() -> None:
    cfg = load_config()
    apply_schema(cfg.primary_dsn)
    apply_schema(cfg.replica_dsn)
    seed_primary(cfg.primary_dsn, n_orders=int(os.environ.get("SEED_ORDERS", 60_000)))


if __name__ == "__main__":
    main()
