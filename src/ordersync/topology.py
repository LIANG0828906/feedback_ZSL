from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any

from .config import EndpointConfig, ServiceConfig
from .db import Connector, DbConnection, pyodbc_connector
from .errors import TopologyError
from .repository import WATERMARK_NAME, WATERMARK_QUERY


REQUIRED_TABLES: dict[str, tuple[str, ...]] = {
    "Orders": (
        "OrderNo",
        "TenantId",
        "Status",
        "Amount",
        "ItemCount",
        "CreatedAtUtc",
        "UpdatedAtUtc",
        "LastWriteWatermark",
    ),
    "OrderLines": (
        "OrderNo",
        "LineNo",
        "TenantId",
        "Sku",
        "Quantity",
        "UnitPrice",
        "LineAmount",
        "WriteWatermark",
    ),
    "ConsistencyWatermark": (
        "Name",
        "AppliedWatermark",
        "UpdatedAtUtc",
    ),
    "TopologyProbe": (
        "ProbeId",
        "TouchedAtUtc",
    ),
}

SCHEMA_QUERY = """
SELECT
    LOWER(t.name) AS table_name,
    c.column_id,
    LOWER(c.name) AS column_name,
    LOWER(ty.name) AS data_type,
    c.max_length,
    c.precision,
    c.scale,
    c.is_nullable,
    c.is_identity,
    c.is_computed
FROM sys.tables AS t
JOIN sys.columns AS c ON c.object_id = t.object_id
JOIN sys.types AS ty ON ty.user_type_id = c.user_type_id
WHERE SCHEMA_NAME(t.schema_id) = N'dbo'
  AND t.name IN (N'Orders', N'OrderLines', N'ConsistencyWatermark', N'TopologyProbe')
ORDER BY table_name, c.column_id
"""

PRIMARY_KEY_QUERY = """
SELECT
    LOWER(t.name) AS table_name,
    ic.key_ordinal,
    LOWER(c.name) AS column_name,
    ic.is_descending_key
FROM sys.tables AS t
JOIN sys.indexes AS i ON i.object_id = t.object_id
JOIN sys.index_columns AS ic
  ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns AS c
  ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE SCHEMA_NAME(t.schema_id) = N'dbo'
  AND i.is_primary_key = 1
  AND t.name IN (N'Orders', N'OrderLines', N'ConsistencyWatermark', N'TopologyProbe')
ORDER BY table_name, ic.key_ordinal
"""


@dataclass(frozen=True)
class TopologySnapshot:
    replication_mode: str
    schema_digest: str
    primary_watermark: int
    replica_watermark: int


def _row_value(row: dict[str, Any], key: str) -> Any:
    if key in row:
        return row[key]
    folded = key.casefold()
    for candidate, value in row.items():
        if str(candidate).casefold() == folded:
            return value
    raise KeyError(key)


class TopologyValidator:
    def __init__(
        self,
        config: ServiceConfig,
        connector: Connector = pyodbc_connector,
    ) -> None:
        self.config = config
        self._connector = connector

    def validate(self, *, check_replication_registration: bool = False) -> TopologySnapshot:
        primary_meta = self._endpoint_meta(
            self.config.primary, role="primary", require_writable=True
        )
        replica_meta = self._endpoint_meta(
            self.config.replica, role="replica", require_writable=False
        )

        if primary_meta["server_name"] == replica_meta["server_name"]:
            raise TopologyError(
                "same_server",
                "Primary and replica must be hosted by different SQL Server instances",
            )

        primary_schema = self._schema_signature(self.config.primary)
        replica_schema = self._schema_signature(self.config.replica)
        if primary_schema != replica_schema:
            raise TopologyError(
                "schema_mismatch",
                "Primary and replica order table structures differ",
            )

        digest = hashlib.sha256(
            json.dumps(primary_schema, sort_keys=True, separators=(",", ":")).encode(
                "utf-8"
            )
        ).hexdigest()

        primary_watermark = self._read_watermark(self.config.primary)
        replica_watermark = self._read_watermark(self.config.replica)
        if replica_watermark > primary_watermark:
            raise TopologyError(
                "replica_ahead_of_primary",
                "Replica applied watermark is ahead of the configured primary",
            )

        if check_replication_registration:
            self._verify_replication_registration()

        return TopologySnapshot(
            replication_mode=self.config.replication.mode,
            schema_digest=digest,
            primary_watermark=primary_watermark,
            replica_watermark=replica_watermark,
        )

    def _endpoint_meta(
        self, endpoint: EndpointConfig, *, role: str, require_writable: bool
    ) -> dict[str, Any]:
        connection = self._open(endpoint, role)
        cursor = None
        try:
            cursor = connection.cursor()
            cursor.execute(
                """
                SELECT
                    DB_NAME() AS database_name,
                    @@SERVERNAME AS server_name,
                    CONVERT(nvarchar(60),
                        DATABASEPROPERTYEX(DB_NAME(), 'Updateability')) AS updateability,
                    CONVERT(bit,
                        (SELECT is_read_only FROM sys.databases WHERE name = DB_NAME()))
                        AS is_read_only
                """
            )
            row = cursor.fetchone()
            if row is None:
                raise TopologyError(
                    f"{role}_metadata_unavailable",
                    f"{role.capitalize()} database metadata is unavailable",
                )
            columns = [column[0] for column in cursor.description or ()]
            meta = dict(zip(columns, row))
            connection.commit()
        except TopologyError:
            raise
        except Exception:
            raise TopologyError(
                f"{role}_unreachable",
                f"{role.capitalize()} SQL Server instance is unreachable",
            ) from None
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

        database_name = str(meta["database_name"])
        if database_name.casefold() != endpoint.database.casefold():
            raise TopologyError(
                f"{role}_database_mismatch",
                f"{role.capitalize()} connection is attached to an unexpected database",
            )
        is_read_only = bool(meta["is_read_only"])
        if role == "replica" and not is_read_only:
            raise TopologyError(
                "replica_writable",
                "Replica database must be configured read-only",
            )
        if role == "primary" and is_read_only:
            raise TopologyError(
                "primary_read_only",
                "Primary database is read-only",
            )

        self._probe_write_access(
            endpoint, role=role, expect_writable=require_writable
        )
        self._probe_readable(endpoint, role=role)
        return {
            "database_name": database_name,
            "server_name": str(meta["server_name"]),
            "is_read_only": is_read_only,
        }

    def _probe_write_access(
        self, endpoint: EndpointConfig, *, role: str, expect_writable: bool
    ) -> None:
        connection = self._open(endpoint, role)
        cursor = None
        update_succeeded = False
        try:
            cursor = connection.cursor()
            cursor.execute("BEGIN TRANSACTION")
            cursor.execute(
                "UPDATE dbo.TopologyProbe "
                "SET TouchedAtUtc = SYSUTCDATETIME() WHERE ProbeId = 1"
            )
            update_succeeded = int(cursor.rowcount) == 1
            cursor.execute("ROLLBACK TRANSACTION")
        except Exception as exc:
            try:
                if cursor is not None:
                    cursor.execute("IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION")
            except Exception:
                pass
            if expect_writable:
                raise TopologyError(
                    "primary_not_writable",
                    "Primary database failed the write probe",
                ) from None
            return
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

        if not expect_writable:
            raise TopologyError(
                "replica_writable",
                "Replica database accepted an update; it must be read-only",
            )
        if not update_succeeded:
            raise TopologyError(
                "primary_probe_missing",
                "Primary topology probe row is missing",
            )

    def _probe_readable(self, endpoint: EndpointConfig, *, role: str) -> None:
        connection = self._open(endpoint, role)
        cursor = None
        try:
            cursor = connection.cursor()
            cursor.execute("SELECT COUNT_BIG(*) FROM dbo.Orders")
            row = cursor.fetchone()
            if row is None:
                raise TopologyError(
                    f"{role}_unreadable",
                    f"{role.capitalize()} order table is not readable",
                )
            connection.commit()
        except TopologyError:
            raise
        except Exception:
            raise TopologyError(
                f"{role}_unreadable",
                f"{role.capitalize()} order table is not readable",
            ) from None
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

    def _schema_signature(self, endpoint: EndpointConfig) -> dict[str, Any]:
        columns = self._query_dicts(endpoint, SCHEMA_QUERY, role="schema")
        primary_keys = self._query_dicts(
            endpoint, PRIMARY_KEY_QUERY, role="schema"
        )
        by_table: dict[str, list[str]] = {}
        for column in columns:
            table_name = str(_row_value(column, "table_name"))
            by_table.setdefault(table_name, []).append(
                str(_row_value(column, "column_name"))
            )
        for table_name, required in REQUIRED_TABLES.items():
            found = by_table.get(table_name.casefold(), [])
            missing = [
                name for name in required if name.casefold() not in found
            ]
            if missing:
                raise TopologyError(
                    "schema_missing",
                    f"Required table {table_name} has missing columns",
                )

        normalized_columns = []
        for column in columns:
            normalized_columns.append(
                {
                    "table": str(_row_value(column, "table_name")),
                    "column_id": int(_row_value(column, "column_id")),
                    "column": str(_row_value(column, "column_name")),
                    "type": str(_row_value(column, "data_type")),
                    "max_length": int(_row_value(column, "max_length")),
                    "precision": int(_row_value(column, "precision")),
                    "scale": int(_row_value(column, "scale")),
                    "nullable": bool(_row_value(column, "is_nullable")),
                    "identity": bool(_row_value(column, "is_identity")),
                    "computed": bool(_row_value(column, "is_computed")),
                }
            )
        normalized_keys = []
        for key in primary_keys:
            normalized_keys.append(
                {
                    "table": str(_row_value(key, "table_name")),
                    "ordinal": int(_row_value(key, "key_ordinal")),
                    "column": str(_row_value(key, "column_name")),
                    "descending": bool(_row_value(key, "is_descending_key")),
                }
            )
        return {"columns": normalized_columns, "primary_keys": normalized_keys}

    def _verify_replication_registration(self) -> None:
        mode = self.config.replication.mode
        if mode == "log_shipping":
            queries = (
                """
                SELECT TOP (1) secondary_database
                FROM msdb.dbo.log_shipping_secondary_databases
                WHERE secondary_database = ?
                """,
                """
                SELECT TOP (1) secondary_database
                FROM msdb.dbo.log_shipping_monitor_secondary
                WHERE secondary_database = ?
                """,
            )
            rows: list[dict[str, Any]] = []
            for sql in queries:
                try:
                    rows = self._query_dicts(
                        self.config.replica,
                        sql,
                        (self.config.replica.database,),
                        role="replication_registration",
                    )
                except TopologyError:
                    rows = []
                if rows:
                    break
            if not rows:
                raise TopologyError(
                    "log_shipping_not_registered",
                    "Replica is not registered as a log shipping secondary database",
                )
            return

        sql = """
            SELECT TOP (1) publisher_db, publication
            FROM dbo.MSreplication_subscriptions
            WHERE publisher_db = ? AND publication = ?
        """
        try:
            rows = self._query_dicts(
                self.config.replica,
                sql,
                (
                    self.config.primary.database,
                    self.config.replication.publication,
                ),
                role="replication_registration",
            )
        except TopologyError:
            rows = []
        if not rows:
            raise TopologyError(
                "transactional_not_registered",
                "Replica is not registered for the configured transactional publication",
            )

    def _query_dicts(
        self,
        endpoint: EndpointConfig,
        sql: str,
        params: tuple[Any, ...] = (),
        *,
        role: str,
    ) -> list[dict[str, Any]]:
        connection = self._open(endpoint, role)
        cursor = None
        try:
            cursor = connection.cursor()
            cursor.execute(sql, params)
            columns = [column[0] for column in cursor.description or ()]
            rows = [dict(zip(columns, row)) for row in cursor.fetchall()]
            connection.commit()
            return rows
        except Exception:
            raise TopologyError(
                f"{role}_query_failed",
                f"{role.capitalize()} metadata query failed",
            ) from None
        finally:
            if cursor is not None:
                try:
                    cursor.close()
                except Exception:
                    pass
            try:
                connection.close()
            except Exception:
                pass

    def _read_watermark(self, endpoint: EndpointConfig) -> int:
        rows = self._query_dicts(
            endpoint,
            WATERMARK_QUERY,
            (WATERMARK_NAME,),
            role="watermark",
        )
        if not rows:
            raise TopologyError(
                "watermark_missing",
                "Consistency watermark row is missing",
            )
        row = rows[0]
        value = _row_value(row, "applied_watermark")
        try:
            return int(value)
        except (TypeError, ValueError):
            raise TopologyError(
                "watermark_invalid",
                "Consistency watermark is not an integer",
            ) from None

    def _open(self, endpoint: EndpointConfig, role: str) -> DbConnection:
        try:
            connection = self._connector(endpoint)
            connection.timeout = endpoint.query_timeout_seconds
            return connection
        except Exception:
            raise TopologyError(
                f"{role}_unreachable",
                f"{role.capitalize()} SQL Server instance is unreachable",
            ) from None

