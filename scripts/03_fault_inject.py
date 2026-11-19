#!/usr/bin/env python3
"""故障注入：断链 / 复制槽回收 / Redis 位点篡改 / 备库数据缺口。

所有操作只影响本任务配置中的 slot/subscription/Redis 前缀键。
用法：
  python3 scripts/03_fault_inject.py disconnect   # 停用备库订阅（槽转不活跃）
  python3 scripts/03_fault_inject.py drop-slot    # 模拟运维手动删槽
  python3 scripts/03_fault_inject.py tamper --ahead
  python3 scripts/03_fault_inject.py tamper --behind
  python3 scripts/03_fault_inject.py delete-offset
  python3 scripts/03_fault_inject.py gap          # 删备库最近数据，制造缺口/孤儿
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "pylibs"))
sys.path.insert(0, str(ROOT / "src"))

import psycopg  # noqa: E402
from psycopg import sql  # noqa: E402

from config import load_config  # noqa: E402

from offsets import OffsetReconciler  # noqa: E402
from store import Store  # noqa: E402


def disconnect(cfg) -> None:
    with psycopg.connect(cfg.replica_dsn, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT 1 FROM pg_subscription WHERE subname=%s",
            (cfg.subscription_name,),
        )
        if cur.fetchone():
            cur.execute(f"ALTER SUBSCRIPTION {cfg.subscription_name} DISABLE")
            print(f"subscription {cfg.subscription_name} disabled (slot goes inactive)")
        else:
            print("subscription not found, nothing to disable")


def drop_slot(cfg) -> None:
    with psycopg.connect(cfg.replica_dsn, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT 1 FROM pg_subscription WHERE subname=%s",
            (cfg.subscription_name,),
        )
        if cur.fetchone():
            cur.execute(f"ALTER SUBSCRIPTION {cfg.subscription_name} DISABLE")
            cur.execute(
                f"ALTER SUBSCRIPTION {cfg.subscription_name} SET (slot_name=NONE)"
            )
            cur.execute(f"DROP SUBSCRIPTION {cfg.subscription_name}")
    with psycopg.connect(cfg.primary_dsn, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute("SELECT slot_name FROM pg_replication_slots")
        names = {r[0] for r in cur.fetchall()}
        if cfg.slot_name in names:
            cur.execute(f"SELECT pg_drop_replication_slot('{cfg.slot_name}')")
            print(f"replication slot {cfg.slot_name} dropped on primary")
        else:
            print("slot already absent on primary")


def tamper(cfg, ahead: bool) -> None:
    store = Store(cfg.sqlite_path)
    try:
        rec = OffsetReconciler(cfg, store)
        delta = 0x8000000 if ahead else -0x8000000
        target = rec.tamper(delta=delta)
        print(f"redis offset tampered to {target} ({'ahead' if ahead else 'behind'})")
    finally:
        store.close()


def delete_offset(cfg) -> None:
    store = Store(cfg.sqlite_path)
    try:
        rec = OffsetReconciler(cfg, store)
        rec.delete_offset()
        print(f"redis key {rec.offset_key()} deleted")
    finally:
        store.close()


def make_gap(cfg) -> None:
    """在备库制造两天变更缺口：

    - 先删最近 400 条明细（形成缺失行）；
    - 再解除一批父表订单的外键引用并删父表、保留其子明细 -> 形成孤儿明细。
    """
    with psycopg.connect(cfg.replica_dsn) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT 1 FROM information_schema.table_constraints "
            "WHERE constraint_name=%s",
            (cfg.child_constraint_name,),
        )
        if cur.fetchone():
            cur.execute(
                sql.SQL("ALTER TABLE {} DROP CONSTRAINT {}").format(
                    sql.Identifier(*cfg.child_table.split(".")),
                    sql.Identifier(cfg.child_constraint_name),
                )
            )
        cur.execute(
            sql.SQL(
                "DELETE FROM {table} WHERE {key} > "
                "(SELECT max({key}) - 400 FROM {table})"
            ).format(
                table=sql.Identifier(*cfg.child_table.split(".")),
                key=sql.Identifier(cfg.child_key),
            )
        )
        deleted_details = cur.rowcount
        cur.execute(
            sql.SQL(
                "SELECT d.{fk} FROM {table} d GROUP BY d.{fk} "
                "HAVING count(*) > 1 ORDER BY d.{fk} DESC LIMIT 200"
            ).format(
                fk=sql.Identifier(cfg.child_fk),
                table=sql.Identifier(*cfg.child_table.split(".")),
            )
        )
        orphan_parents = [row[0] for row in cur.fetchall()]
        cur.execute(
            sql.SQL("DELETE FROM {} WHERE {} = ANY(%s)").format(
                sql.Identifier(*cfg.parent_table.split(".")),
                sql.Identifier(cfg.parent_key),
            ),
            (orphan_parents,),
        )
        deleted_orders = cur.rowcount
        conn.commit()
        cur.execute(
            sql.SQL(
                "SELECT count(*) FROM {child} d WHERE NOT EXISTS ("
                "SELECT 1 FROM {parent} o WHERE o.{pkey}=d.{fk})"
            ).format(
                child=sql.Identifier(*cfg.child_table.split(".")),
                parent=sql.Identifier(*cfg.parent_table.split(".")),
                pkey=sql.Identifier(cfg.parent_key),
                fk=sql.Identifier(cfg.child_fk),
            )
        )
        orphans = cur.fetchone()[0]
    print(
        f"replica gap: removed {deleted_orders} orders, {deleted_details} "
        f"details; orphan_details_left={orphans}"
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="fault", required=True)
    sub.add_parser("disconnect")
    sub.add_parser("drop-slot")
    p_t = sub.add_parser("tamper")
    grp = p_t.add_mutually_exclusive_group(required=True)
    grp.add_argument("--ahead", action="store_true")
    grp.add_argument("--behind", action="store_true")
    sub.add_parser("delete-offset")
    sub.add_parser("gap")
    args = parser.parse_args()
    cfg = load_config()

    if args.fault == "disconnect":
        disconnect(cfg)
    elif args.fault == "drop-slot":
        drop_slot(cfg)
    elif args.fault == "tamper":
        tamper(cfg, ahead=args.ahead)
    elif args.fault == "delete-offset":
        delete_offset(cfg)
    elif args.fault == "gap":
        make_gap(cfg)


if __name__ == "__main__":
    main()
