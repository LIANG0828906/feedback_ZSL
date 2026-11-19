#!/usr/bin/env python3
"""Create the legacy/shard databases and load the deterministic 20k seed."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from orders_migration.config import Config
from orders_migration.sqlclient import (
    connection_with_database,
    open_connection,
    parse_connection_pairs,
)


def split_batches(script: str) -> list[str]:
    batches: list[str] = []
    current: list[str] = []
    for line in script.splitlines():
        if line.strip().upper() == "GO":
            text = "\n".join(current).strip()
            if text:
                batches.append(text)
            current = []
        else:
            current.append(line)
    text = "\n".join(current).strip()
    if text:
        batches.append(text)
    return batches


def connect(connection_string: str, config: Config) -> tuple[Any, str]:
    return open_connection(
        connection_string,
        config.database.driver,
        config.database.connect_timeout_seconds,
        max(120, config.database.command_timeout_seconds),
    )


def execute_batches(
    connection: Any, dialect: str, script: str
) -> None:
    cursor = connection.cursor()
    try:
        for batch in split_batches(script):
            sql = batch.replace("?", "%s") if dialect == "pymssql" else batch
            cursor.execute(sql)
            while cursor.nextset():
                pass
            connection.commit()
    finally:
        cursor.close()


def create_database(
    admin_connection: Any, dialect: str, name: str, fresh: bool
) -> None:
    cursor = admin_connection.cursor()
    try:
        cursor.execute(
            "SELECT DB_ID(%s)" if dialect == "pymssql" else "SELECT DB_ID(?)",
            (name,),
        )
        exists = cursor.fetchone()[0] is not None
        if exists and fresh:
            quoted = "[" + name.replace("]", "]]") + "]"
            cursor.execute(
                f"ALTER DATABASE {quoted} SET SINGLE_USER WITH ROLLBACK IMMEDIATE"
            )
            cursor.execute(f"DROP DATABASE {quoted}")
            exists = False
        if not exists:
            quoted = "[" + name.replace("]", "]]") + "]"
            cursor.execute(f"CREATE DATABASE {quoted}")
        admin_connection.commit()
    finally:
        cursor.close()


def database_name(connection_string: str, fallback: str) -> str:
    pairs = parse_connection_pairs(connection_string)
    return (
        pairs.get("database")
        or pairs.get("dbq")
        or pairs.get("initial catalog")
        or fallback
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default=str(ROOT / "config.local.json"))
    parser.add_argument("--fresh", action="store_true")
    parser.add_argument("--no-create", action="store_true")
    parser.add_argument("--no-seed", action="store_true")
    args = parser.parse_args()

    config = Config.load(args.config)
    legacy_cs = config.database.legacy_connection_string
    shard_cs = config.database.shard_connection_string(0)
    legacy_name = database_name(legacy_cs, "OrdersMigrationLegacy")
    shard_names = [
        database_name(config.database.shard_connection_string(index), f"OrdersShard{index}")
        for index in range(config.shard_count)
    ]

    if not args.no_create:
        legacy_admin, dialect = connect(
            connection_with_database(legacy_cs, "master"), config
        )
        try:
            if dialect == "pymssql":
                legacy_admin.autocommit(True)
            else:
                legacy_admin.autocommit = True
            create_database(legacy_admin, dialect, legacy_name, args.fresh)
        finally:
            legacy_admin.close()
        shard_admin, dialect = connect(
            connection_with_database(shard_cs, "master"), config
        )
        try:
            if dialect == "pymssql":
                shard_admin.autocommit(True)
            else:
                shard_admin.autocommit = True
            for name in shard_names:
                create_database(shard_admin, dialect, name, args.fresh)
        finally:
            shard_admin.close()

    schema = (ROOT / "sql" / "01_schema.sql").read_text(encoding="utf-8")
    targets = [(legacy_cs, legacy_name)] + [
        (config.database.shard_connection_string(index), shard_names[index])
        for index in range(config.shard_count)
    ]
    for connection_string, name in targets:
        connection, dialect = connect(connection_string, config)
        try:
            execute_batches(connection, dialect, schema)
            print(f"[schema] {name}")
        finally:
            connection.close()

    if not args.no_seed:
        seed = (ROOT / "sql" / "02_seed_legacy.sql").read_text(encoding="utf-8")
        connection, dialect = connect(legacy_cs, config)
        try:
            execute_batches(connection, dialect, seed)
            cursor = connection.cursor()
            cursor.execute("SELECT COUNT(*) FROM dbo.Orders")
            orders = int(cursor.fetchone()[0])
            cursor.execute("SELECT COUNT(*) FROM dbo.OrderItems")
            items = int(cursor.fetchone()[0])
            cursor.close()
            print(f"[seed] {legacy_name}: orders={orders} items={items}")
            if orders != 20000:
                raise RuntimeError(f"expected 20000 orders, got {orders}")
        finally:
            connection.close()

    print(
        f"[ready] legacy={legacy_name} shards="
        + ",".join(shard_names)
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

