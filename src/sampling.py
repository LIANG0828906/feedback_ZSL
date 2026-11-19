"""Read-only consistency sampling using a stable primary-key hash."""
from __future__ import annotations

import time
from decimal import Decimal
from typing import Any

from psycopg import sql

from config import Config
from db import connect, table_identifier
from store import Store

ORDER_COLUMNS = ["status", "total_amount"]
DETAIL_COLUMNS = ["status", "amount", "unit_price", "quantity"]


class Sampler:
    def __init__(self, cfg: Config, store: Store):
        self.cfg = cfg
        self.store = store

    @staticmethod
    def _norm(value: Any) -> str | None:
        if value is None:
            return None
        if isinstance(value, Decimal):
            return format(value, "f")
        return str(value)

    def run_round(self, round_id: str) -> dict[str, Any]:
        n = self.cfg.sample_order_count
        parent = table_identifier(self.cfg.parent_table)
        child = table_identifier(self.cfg.child_table)

        # Sample keys are selected only on the primary using the configured
        # seed.  The exact same keys are then queried on the replica, which
        # makes missing rows visible instead of silently changing the sample.
        order_sql = sql.SQL(
            "SELECT {key}, status, total_amount FROM {table} "
            "ORDER BY hashtextextended({key}::text, %s) LIMIT %s"
        ).format(
            key=sql.Identifier(self.cfg.parent_key), table=parent
        )
        order_by_keys_sql = sql.SQL(
            "SELECT {key}, status, total_amount FROM {table} "
            "WHERE {key} = ANY(%s)"
        ).format(
            key=sql.Identifier(self.cfg.parent_key), table=parent
        )
        detail_by_orders_sql = sql.SQL(
            "SELECT {key}, {fk}, status, amount, unit_price, quantity "
            "FROM {table} WHERE {fk} = ANY(%s) ORDER BY {key}"
        ).format(
            key=sql.Identifier(self.cfg.child_key),
            fk=sql.Identifier(self.cfg.child_fk),
            table=child,
        )

        with connect(
            self.cfg.primary_dsn, self.cfg.connect_timeout_s, autocommit=True
        ) as p, connect(
            self.cfg.replica_dsn, self.cfg.connect_timeout_s, autocommit=True
        ) as r:
            pc, rc = p.cursor(), r.cursor()
            pc.execute(order_sql, (self.cfg.sample_seed, n))
            primary_orders = pc.fetchall()
            order_ids = [row[0] for row in primary_orders]

            if order_ids:
                rc.execute(order_by_keys_sql, (order_ids,))
                replica_orders = rc.fetchall()
                pc.execute(detail_by_orders_sql, (order_ids,))
                primary_details = pc.fetchall()
                rc.execute(detail_by_orders_sql, (order_ids,))
                replica_details = rc.fetchall()
            else:
                replica_orders = []
                primary_details = []
                replica_details = []

        mismatches: list[dict[str, Any]] = []
        now = time.time()

        def add(
            table: str, key_name: str, key: Any, column: str,
            primary_val: Any, replica_val: Any,
        ) -> None:
            mismatches.append(
                {
                    "round_id": round_id,
                    "table_name": table,
                    "primary_key": f"{key_name}={key}",
                    "column_name": column,
                    "primary_val": self._norm(primary_val),
                    "replica_val": self._norm(replica_val),
                    "created_at": now,
                }
            )

        def compare(
            table: str, key_name: str, value_columns: list[str],
            primary_rows: list[tuple[Any, ...]],
            replica_rows: list[tuple[Any, ...]],
            label_columns: list[str],
        ) -> None:
            p_map = {row[0]: row for row in primary_rows}
            r_map = {row[0]: row for row in replica_rows}
            for key, prow in p_map.items():
                rrow = r_map.get(key)
                if rrow is None:
                    add(table, key_name, key, "__row__", "present", "missing")
                    continue
                pvals = dict(zip(label_columns, prow[1:]))
                rvals = dict(zip(label_columns, rrow[1:]))
                for column in value_columns:
                    if self._norm(pvals[column]) != self._norm(rvals[column]):
                        add(
                            table, key_name, key, column,
                            pvals[column], rvals[column],
                        )
            for key in r_map.keys() - p_map.keys():
                add(table, key_name, key, "__row__", "missing", "present")

        compare(
            self.cfg.parent_table,
            self.cfg.parent_key,
            ORDER_COLUMNS,
            primary_orders,
            replica_orders,
            ORDER_COLUMNS,
        )
        compare(
            self.cfg.child_table,
            self.cfg.child_key,
            DETAIL_COLUMNS,
            primary_details,
            replica_details,
            ["fk", *DETAIL_COLUMNS],
        )

        for mismatch in mismatches:
            self.store.insert_sample_mismatch(mismatch)
        self.store.inc_counter("sample_mismatches_total", len(mismatches))
        self.store.inc_counter("sample_rounds_total")
        return {
            "round_id": round_id,
            "orders_sampled": len(primary_orders),
            "details_sampled": len(primary_details),
            "mismatches": mismatches,
        }
