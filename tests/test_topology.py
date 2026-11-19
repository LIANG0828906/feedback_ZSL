from __future__ import annotations

import unittest
from typing import Any

from ordersync.config import EndpointConfig
from ordersync.errors import TopologyError
from ordersync.topology import TopologyValidator

from .helpers import base_config


class TopologyCursor:
    def __init__(self, *, read_only: bool) -> None:
        self.read_only = read_only
        self.description: list[tuple[Any, ...]] | None = None
        self.rows: list[tuple[Any, ...]] = []
        self.rowcount = -1

    def execute(self, sql: str, params: tuple[Any, ...] = ()) -> None:
        if "DB_NAME() AS database_name" in sql:
            self.description = [
                ("database_name",),
                ("server_name",),
                ("updateability",),
                ("is_read_only",),
            ]
            database = "OrderReplica" if self.read_only else "OrderMain"
            self.rows = [
                (
                    database,
                    "host\\instance",
                    "READ_ONLY" if self.read_only else "READ_WRITE",
                    self.read_only,
                )
            ]
        elif "UPDATE dbo.TopologyProbe" in sql:
            self.rowcount = 1
        elif "SELECT COUNT_BIG(*) FROM dbo.Orders" in sql:
            self.description = [("count",)]
            self.rows = [(20000,)]
        elif "ROLLBACK" in sql:
            self.rowcount = 0

    def fetchone(self) -> tuple[Any, ...] | None:
        return self.rows[0] if self.rows else None

    def fetchall(self) -> list[tuple[Any, ...]]:
        return self.rows

    def close(self) -> None:
        return


class TopologyConnection:
    def __init__(self, endpoint: EndpointConfig) -> None:
        self.endpoint = endpoint
        self.autocommit = False
        self.timeout = 0
        self._cursor = TopologyCursor(read_only=endpoint.name == "replica")

    def cursor(self) -> TopologyCursor:
        return self._cursor

    def commit(self) -> None:
        return

    def rollback(self) -> None:
        return

    def close(self) -> None:
        return


class TopologyTests(unittest.TestCase):
    def test_writable_replica_fails_with_e_topology(self) -> None:
        validator = TopologyValidator(base_config(), TopologyConnection)
        with self.assertRaises(TopologyError) as raised:
            validator.validate()
        self.assertEqual(raised.exception.code, "E_TOPOLOGY")
        self.assertEqual(raised.exception.details["reason"], "replica_writable")

    def test_unreachable_primary_fails_with_e_topology(self) -> None:
        def connector(endpoint: EndpointConfig) -> TopologyConnection:
            if endpoint.name == "primary":
                raise RuntimeError("connection refused")
            return TopologyConnection(endpoint)

        validator = TopologyValidator(base_config(), connector)
        with self.assertRaises(TopologyError) as raised:
            validator.validate()
        self.assertEqual(raised.exception.code, "E_TOPOLOGY")
        self.assertEqual(raised.exception.details["reason"], "primary_unreachable")


if __name__ == "__main__":
    unittest.main()

