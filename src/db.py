"""PostgreSQL 连接与稳定业务错误码。

读取某一实例失败时抛出 SlotGuardError，携带错误码、实例名与查询对象，
由上层决定如何记录，绝不中断另一实例的汇总。
"""
from __future__ import annotations

from contextlib import contextmanager
from typing import Any, Iterator

import psycopg
from psycopg import sql


class SlotGuardError(Exception):
    def __init__(self, code: str, instance: str, target: str, message: str):
        self.code = code
        self.instance = instance
        self.target = target
        self.message = message

    def __str__(self) -> str:
        return (
            f"[{self.code}] instance={self.instance or 'unreachable'} "
            f"target={self.target}: {self.message}"
        )


def connect(dsn: str, connect_timeout: int, autocommit: bool = False):
    full_dsn = dsn
    if "connect_timeout" not in dsn:
        full_dsn = f"{dsn} connect_timeout={connect_timeout}"
    try:
        return psycopg.connect(full_dsn, autocommit=autocommit)
    except psycopg.Error as exc:
        raise SlotGuardError(
            "SG-E1001", "unknown", "connect",
            f"database connection failed: {exc.diag.message_primary or exc}",
        ) from exc


@contextmanager
def instance_conn(
    dsn: str, connect_timeout: int, instance: str, autocommit: bool = False
) -> Iterator[psycopg.Connection[Any]]:
    try:
        conn = connect(dsn, connect_timeout, autocommit=autocommit)
    except SlotGuardError as exc:
        exc.instance = instance
        raise
    try:
        yield conn
    finally:
        conn.close()


def safe_query(
    conn: psycopg.Connection[Any],
    instance: str,
    target: str,
    sql: str,
    params: tuple[Any, ...] = (),
) -> list[tuple[Any, ...]]:
    try:
        with conn.cursor() as cur:
            cur.execute(sql, params)
            return cur.fetchall()
    except psycopg.Error as exc:
        raise SlotGuardError(
            "SG-E1002", instance, target, f"query failed: {exc.diag.message_primary or exc}"
        ) from exc


def safe_query_one(
    conn: psycopg.Connection[Any],
    instance: str,
    target: str,
    sql: str,
    params: tuple[Any, ...] = (),
) -> tuple[Any, ...] | None:
    rows = safe_query(conn, instance, target, sql, params)
    return rows[0] if rows else None


def lsn_int(lsn_text: str | None) -> int | None:
    if not lsn_text:
        return None
    high, low = lsn_text.split("/")
    return (int(high, 16) << 32) | int(low, 16)


def lsn_text(value: int | None) -> str | None:
    if value is None:
        return None
    return f"{value >> 32:X}/{value & 0xFFFFFFFF:08X}"


def table_identifier(value: str) -> sql.Composed:
    """Return a safely quoted schema-qualified identifier."""
    parts = value.split(".")
    if len(parts) == 1:
        return sql.Identifier(parts[0])
    if len(parts) == 2:
        return sql.Identifier(parts[0], parts[1])
    raise ValueError(f"invalid table identifier: {value!r}")


def stable_error(exc: BaseException) -> str:
    if isinstance(exc, SlotGuardError):
        return str(exc)
    if isinstance(exc, psycopg.Error):
        return exc.diag.message_primary or str(exc)
    return str(exc)
