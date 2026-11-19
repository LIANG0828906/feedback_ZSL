from __future__ import annotations

import unittest
from typing import Any

from ordersync.config import EndpointConfig
from ordersync.db import BoundedConnectionPool, PoolExhaustedError, SqlClient


class FakeCursor:
    def __init__(self) -> None:
        self.description = [("value", int, None, None, None, None, None)]
        self.timeout = 0
        self.executed: list[tuple[str, tuple[Any, ...]]] = []

    def execute(self, sql: str, params: tuple[Any, ...] = ()) -> None:
        self.executed.append((sql, params))

    def fetchall(self) -> list[tuple[int]]:
        return [(7,)]

    def fetchone(self) -> tuple[int]:
        return (7,)

    def close(self) -> None:
        return


class FakeConnection:
    def __init__(self) -> None:
        self.autocommit = False
        self.timeout = 0
        self.commits = 0
        self.rollbacks = 0
        self.closed = False
        self.cursor_instance = FakeCursor()

    def cursor(self) -> FakeCursor:
        return self.cursor_instance

    def commit(self) -> None:
        self.commits += 1

    def rollback(self) -> None:
        self.rollbacks += 1

    def close(self) -> None:
        self.closed = True


class DbTests(unittest.TestCase):
    def setUp(self) -> None:
        self.endpoint = EndpointConfig(
            name="primary",
            connection_string="fake",
            database="OrderMain",
            pool_size=1,
            pool_acquire_timeout_ms=5,
        )

    def test_pool_bound_is_a_hard_limit(self) -> None:
        created: list[FakeConnection] = []

        def connector(endpoint: EndpointConfig) -> FakeConnection:
            connection = FakeConnection()
            created.append(connection)
            return connection

        pool = BoundedConnectionPool(self.endpoint, connector)
        first = pool.acquire(0)
        with self.assertRaises(PoolExhaustedError):
            pool.acquire(0.001)
        pool.release(first)
        second = pool.acquire(0.001)
        self.assertIs(first, second)
        self.assertEqual(len(created), 1)
        pool.close()

    def test_query_releases_connection(self) -> None:
        created: list[FakeConnection] = []

        def connector(endpoint: EndpointConfig) -> FakeConnection:
            connection = FakeConnection()
            created.append(connection)
            return connection

        client = SqlClient(self.endpoint, connector)
        rows = client.query("SELECT 7 AS value")
        self.assertEqual(rows, [{"value": 7}])
        self.assertEqual(len(created), 1)
        second = client.query_one("SELECT 7 AS value")
        self.assertEqual(second, {"value": 7})
        self.assertEqual(len(created), 1)
        self.assertEqual(created[0].commits, 2)
        client.close()
        self.assertTrue(created[0].closed)


if __name__ == "__main__":
    unittest.main()

