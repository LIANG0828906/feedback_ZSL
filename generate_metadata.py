#!/usr/bin/env python3
"""Generate the deterministic warehouse metadata and data database."""

from __future__ import annotations

import argparse
import json
import os
import random
import sqlite3
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

from warehouse_model import (
    METRIC_CONFIG,
    REPORT_CONFIG,
    TABLE_DESCRIPTIONS,
    TABLE_LAYERS,
    TABLE_SCHEMA,
    TASKS,
    quote_identifier,
)


DEFAULT_SEED = 20261119
DEFAULT_DB = "metadata.db"
RUN_BASE = datetime(2026, 1, 15, 2, 0, 0, tzinfo=timezone.utc)


METADATA_DDL = """
PRAGMA foreign_keys = ON;

CREATE TABLE meta_info (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE schema_tables (
    snapshot_id TEXT NOT NULL,
    table_name TEXT NOT NULL,
    layer TEXT NOT NULL,
    description TEXT NOT NULL,
    row_count INTEGER NOT NULL,
    PRIMARY KEY (snapshot_id, table_name)
);

CREATE TABLE schema_columns (
    snapshot_id TEXT NOT NULL,
    table_name TEXT NOT NULL,
    column_name TEXT NOT NULL,
    data_type TEXT NOT NULL,
    ordinal_position INTEGER NOT NULL,
    nullable INTEGER NOT NULL,
    PRIMARY KEY (snapshot_id, table_name, column_name),
    FOREIGN KEY (snapshot_id, table_name)
        REFERENCES schema_tables(snapshot_id, table_name)
);

CREATE TABLE tasks (
    task_name TEXT PRIMARY KEY,
    task_order INTEGER NOT NULL UNIQUE,
    output_table TEXT NOT NULL,
    description TEXT NOT NULL,
    task_sql TEXT NOT NULL,
    last_run_id TEXT NOT NULL,
    last_run_status TEXT NOT NULL,
    last_run_started_at TEXT NOT NULL,
    last_run_finished_at TEXT NOT NULL,
    last_run_rows INTEGER NOT NULL
);

CREATE TABLE task_runs (
    run_id TEXT PRIMARY KEY,
    task_name TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    output_rows INTEGER NOT NULL,
    FOREIGN KEY (task_name) REFERENCES tasks(task_name)
);

CREATE TABLE report_task_map (
    report_id TEXT NOT NULL,
    report_name TEXT NOT NULL,
    owner TEXT NOT NULL,
    task_name TEXT NOT NULL,
    description TEXT NOT NULL,
    PRIMARY KEY (report_id, task_name)
);

CREATE TABLE metric_defs (
    metric_id TEXT PRIMARY KEY,
    metric_name TEXT NOT NULL,
    source_table TEXT NOT NULL,
    query_sql TEXT NOT NULL,
    threshold_pct REAL NOT NULL,
    description TEXT NOT NULL
);

CREATE TABLE change_requests (
    change_id TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL UNIQUE,
    table_name TEXT NOT NULL,
    column_name TEXT NOT NULL,
    change_type TEXT NOT NULL,
    new_type TEXT,
    inferred_new_type INTEGER NOT NULL DEFAULT 0,
    drift_threshold REAL NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT
);

CREATE TABLE impact_results (
    change_id TEXT PRIMARY KEY,
    result_json TEXT NOT NULL,
    affected_task_count INTEGER NOT NULL,
    direct_count INTEGER NOT NULL,
    transitive_count INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (change_id) REFERENCES change_requests(change_id)
);

CREATE TABLE drift_checks (
    drift_id TEXT PRIMARY KEY,
    change_id TEXT NOT NULL,
    threshold_pct REAL NOT NULL,
    status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (change_id, threshold_pct),
    FOREIGN KEY (change_id) REFERENCES change_requests(change_id)
);

CREATE TABLE rollback_plans (
    change_id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (change_id) REFERENCES change_requests(change_id)
);

CREATE TABLE audit_events (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    change_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    event_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (change_id) REFERENCES change_requests(change_id)
);

CREATE INDEX idx_tasks_order ON tasks(task_order);
CREATE INDEX idx_report_task ON report_task_map(task_name);
CREATE INDEX idx_drift_change ON drift_checks(change_id, created_at);
CREATE INDEX idx_audit_change ON audit_events(change_id, event_id);
"""


REGIONS = ["EAST", "NORTH", "SOUTH", "WEST", "CENTRAL", "OVERSEAS"]
SEGMENTS = ["ENTERPRISE", "SMB", "CONSUMER"]
CHANNELS = ["WEB", "MOBILE", "STORE", "PARTNER", "MARKETPLACE"]
STATUSES = ["COMPLETED", "COMPLETED", "COMPLETED", "COMPLETED", "REFUNDED", "CANCELLED"]
CATEGORIES = [
    "HOME",
    "OFFICE",
    "ELECTRONICS",
    "OUTDOOR",
    "APPAREL",
    "GROCERY",
    "BEAUTY",
    "TOYS",
]
COUPONS = ["NONE", "WELCOME10", "SPRING5", "VIP15", "FREESHIP"]
CURRENCIES = ["CNY", "USD"]


def utc_text(value: datetime) -> str:
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def create_data_tables(conn: sqlite3.Connection) -> None:
    for table_name, columns in TABLE_SCHEMA.items():
        definitions = ", ".join(
            f"{quote_identifier(column_name)} {data_type}"
            for column_name, data_type in columns
        )
        conn.execute(
            f"CREATE TABLE {quote_identifier(table_name)} ({definitions})"
        )


def populate_raw_data(
    conn: sqlite3.Connection, seed: int, customers: int, products: int, orders: int
) -> None:
    rng = random.Random(seed)

    customer_rows = []
    for customer_id in range(1, customers + 1):
        signup = datetime(2025, 1, 1) + timedelta(days=rng.randrange(0, 540))
        customer_rows.append(
            (
                customer_id,
                f"Customer {customer_id:04d}",
                REGIONS[(customer_id * 7 + rng.randrange(len(REGIONS))) % len(REGIONS)],
                SEGMENTS[(customer_id * 5 + rng.randrange(len(SEGMENTS))) % len(SEGMENTS)],
                signup.strftime("%Y-%m-%d"),
            )
        )
    conn.executemany(
        "INSERT INTO raw_customers "
        "(customer_id, customer_name, region, segment, signup_date) "
        "VALUES (?, ?, ?, ?, ?)",
        customer_rows,
    )

    product_rows = []
    for product_id in range(1, products + 1):
        list_price = round(8 + rng.random() * 480, 2)
        product_rows.append(
            (
                product_id,
                f"Product {product_id:03d}",
                CATEGORIES[(product_id * 3 + rng.randrange(len(CATEGORIES))) % len(CATEGORIES)],
                list_price,
                round(list_price * (0.42 + rng.random() * 0.28), 2),
            )
        )
    conn.executemany(
        "INSERT INTO raw_products "
        "(product_id, product_name, category, list_price, cost) "
        "VALUES (?, ?, ?, ?, ?)",
        product_rows,
    )

    order_rows = []
    item_rows = []
    first_order = datetime(2026, 1, 1, 8, 0, 0)
    for order_id in range(1, orders + 1):
        customer_id = rng.randrange(1, customers + 1)
        channel = CHANNELS[rng.randrange(len(CHANNELS))]
        status = STATUSES[rng.randrange(len(STATUSES))]
        order_dt = first_order + timedelta(
            days=rng.randrange(0, 90),
            minutes=rng.randrange(0, 12 * 60),
        )
        currency = CURRENCIES[0] if rng.random() < 0.84 else CURRENCIES[1]
        order_rows.append(
            (
                order_id,
                customer_id,
                channel,
                status,
                order_dt.strftime("%Y-%m-%dT%H:%M:%S"),
                currency,
            )
        )

        line_count = rng.randrange(1, 5)
        for line_no in range(1, line_count + 1):
            product_id = rng.randrange(1, products + 1)
            quantity = rng.randrange(1, 6)
            list_price = product_rows[product_id - 1][3]
            unit_price = round(list_price * (0.92 + rng.random() * 0.16), 2)
            gross = quantity * unit_price
            if rng.random() < 0.24:
                discount = round(gross * (0.03 + rng.random() * 0.17), 2)
            else:
                discount = 0.0
            tax = round((gross - discount) * (0.05 + rng.random() * 0.04), 2)
            coupon = COUPONS[rng.randrange(len(COUPONS))]
            item_rows.append(
                (
                    order_id,
                    line_no,
                    product_id,
                    quantity,
                    unit_price,
                    discount,
                    tax,
                    coupon,
                )
            )
    conn.executemany(
        "INSERT INTO raw_orders "
        "(order_id, customer_id, channel, status, order_ts, currency) "
        "VALUES (?, ?, ?, ?, ?, ?)",
        order_rows,
    )
    conn.executemany(
        "INSERT INTO raw_order_items "
        "(order_id, line_no, product_id, quantity, unit_price, "
        "discount_amount, tax_amount, coupon_code) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        item_rows,
    )


def table_count(conn: sqlite3.Connection, table_name: str) -> int:
    return int(
        conn.execute(
            f"SELECT COUNT(*) FROM {quote_identifier(table_name)}"
        ).fetchone()[0]
    )


def materialize_tasks(conn: sqlite3.Connection) -> list[dict]:
    runs = []
    for task_order, task in enumerate(TASKS, 1):
        started = RUN_BASE + timedelta(minutes=task_order - 1)
        finished = started + timedelta(seconds=21)
        conn.executescript(task["sql"])
        rows = table_count(conn, task["output_table"])
        run_id = f"run_20260115_020000_{task_order:03d}"
        runs.append(
            {
                "run_id": run_id,
                "task_name": task["task_name"],
                "task_order": task_order,
                "started_at": utc_text(started),
                "finished_at": utc_text(finished),
                "output_rows": rows,
            }
        )
    conn.commit()
    return runs


def write_metadata(conn: sqlite3.Connection, seed: int, runs: list[dict]) -> None:
    snapshot_id = "baseline"
    conn.executemany(
        "INSERT INTO meta_info (key, value) VALUES (?, ?)",
        [
            ("format_version", "1"),
            ("seed", str(seed)),
            ("snapshot_id", snapshot_id),
            ("generator", "generate_metadata.py"),
            ("description", "Deterministic local warehouse metadata"),
        ],
    )

    for task_order, (task, run) in enumerate(zip(TASKS, runs), 1):
        conn.execute(
            "INSERT INTO tasks ("
            "task_name, task_order, output_table, description, task_sql, "
            "last_run_id, last_run_status, last_run_started_at, "
            "last_run_finished_at, last_run_rows"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                task["task_name"],
                task_order,
                task["output_table"],
                task["description"],
                task["sql"],
                run["run_id"],
                "SUCCESS",
                run["started_at"],
                run["finished_at"],
                run["output_rows"],
            ),
        )
        conn.execute(
            "INSERT INTO task_runs ("
            "run_id, task_name, status, started_at, finished_at, output_rows"
            ") VALUES (?, ?, ?, ?, ?, ?)",
            (
                run["run_id"],
                task["task_name"],
                "SUCCESS",
                run["started_at"],
                run["finished_at"],
                run["output_rows"],
            ),
        )

    for table_name, layer in TABLE_LAYERS.items():
        conn.execute(
            "INSERT INTO schema_tables ("
            "snapshot_id, table_name, layer, description, row_count"
            ") VALUES (?, ?, ?, ?, ?)",
            (
                snapshot_id,
                table_name,
                layer,
                TABLE_DESCRIPTIONS[table_name],
                table_count(conn, table_name),
            ),
        )
        for position, (column_name, data_type) in enumerate(
            TABLE_SCHEMA[table_name], 1
        ):
            conn.execute(
                "INSERT INTO schema_columns ("
                "snapshot_id, table_name, column_name, data_type, "
                "ordinal_position, nullable"
                ") VALUES (?, ?, ?, ?, ?, ?)",
                (
                    snapshot_id,
                    table_name,
                    column_name,
                    data_type,
                    position,
                    1,
                ),
            )

    for report in REPORT_CONFIG:
        conn.execute(
            "INSERT INTO report_task_map ("
            "report_id, report_name, owner, task_name, description"
            ") VALUES (?, ?, ?, ?, ?)",
            (
                report["report_id"],
                report["report_name"],
                report["owner"],
                report["task_name"],
                report["description"],
            ),
        )

    for metric in METRIC_CONFIG:
        conn.execute(
            "INSERT INTO metric_defs ("
            "metric_id, metric_name, source_table, query_sql, "
            "threshold_pct, description"
            ") VALUES (?, ?, ?, ?, ?, ?)",
            (
                metric["metric_id"],
                metric["metric_name"],
                metric["source_table"],
                metric["query_sql"],
                metric["threshold_pct"],
                metric["description"],
            ),
        )


def generate_database(
    output: Path,
    seed: int = DEFAULT_SEED,
    customers: int = 200,
    products: int = 48,
    orders: int = 600,
) -> Path:
    if customers < 1 or products < 1 or orders < 1:
        raise ValueError("customers, products and orders must be positive")

    output = output.resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=output.name + ".", suffix=".tmp", dir=str(output.parent)
    )
    os.close(fd)
    temporary = Path(temporary_name)
    try:
        conn = sqlite3.connect(temporary)
        try:
            conn.executescript(METADATA_DDL)
            create_data_tables(conn)
            populate_raw_data(conn, seed, customers, products, orders)
            conn.commit()
            runs = materialize_tasks(conn)
            write_metadata(conn, seed, runs)
            conn.execute("PRAGMA optimize")
            conn.commit()
        finally:
            conn.close()
        os.replace(temporary, output)
    except BaseException:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise
    return output


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", default=DEFAULT_DB, help="output SQLite path")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--customers", type=int, default=200)
    parser.add_argument("--products", type=int, default=48)
    parser.add_argument("--orders", type=int, default=600)
    args = parser.parse_args()

    path = generate_database(
        Path(args.db),
        seed=args.seed,
        customers=args.customers,
        products=args.products,
        orders=args.orders,
    )
    conn = sqlite3.connect(path)
    try:
        counts = {
            table: table_count(conn, table)
            for table in TABLE_LAYERS
        }
        summary = {
            "database": path.name,
            "seed": args.seed,
            "tables": len(TABLE_LAYERS),
            "tasks": len(TASKS),
            "reports": len({item["report_id"] for item in REPORT_CONFIG}),
            "metrics": len(METRIC_CONFIG),
            "row_counts": counts,
        }
        print(json.dumps(summary, indent=2, ensure_ascii=False))
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
