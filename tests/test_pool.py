from __future__ import annotations

import time
from types import SimpleNamespace

import pytest

from orders_migration.config import DatabaseConfig
from orders_migration.errors import AppError
from orders_migration.sqlclient import ConnectionPool, ManagedConnection


def test_pool_hard_limit_returns_e_conn_pool(monkeypatch: pytest.MonkeyPatch) -> None:
    db = DatabaseConfig(
        driver="pymssql",
        legacy_connection_string="server=unused",
        shard_connection_string_template="server=unused;database=s{shard}",
        shard_count=1,
        connect_timeout_seconds=1,
        command_timeout_seconds=1,
    )
    pool = ConnectionPool(db, max_connections=1, acquire_timeout_ms=20,
                          idle_ttl_seconds=0)

    def fake_connect(key: str, connection_string: str) -> ManagedConnection:
        now = time.monotonic()
        return ManagedConnection(
            key,
            SimpleNamespace(close=lambda: None),
            "pymssql",
            now,
            now,
        )

    monkeypatch.setattr(pool, "_connect", fake_connect)
    first = pool.acquire("legacy", "unused")
    with pytest.raises(AppError) as caught:
        pool.acquire("legacy", "unused")
    assert caught.value.code == "E_CONN_POOL"
    pool.release(first)
    second = pool.acquire("legacy", "unused")
    assert second is first
    pool.release(second)
    pool.close()
