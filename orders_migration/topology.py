from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any

from .errors import AppError, topology_error
from .sqlclient import DatabaseAccessError, DatabaseHub, tenant_shard


REQUIRED_TABLES = ("Orders", "OrderItems", "WriteReceipts")
REQUIRED_INDEXES = {
    "Orders": ("PK_Orders", "IX_Orders_TenantStatus"),
    "OrderItems": ("PK_OrderItems", "IX_OrderItems_OrderId"),
    "WriteReceipts": ("PK_WriteReceipts", "IX_WriteReceipts_OrderId"),
}


_COLUMNS_SQL = """
SELECT
    o.name AS object_name,
    c.column_id,
    c.name AS column_name,
    ty.name AS type_name,
    c.max_length,
    c.precision,
    c.scale,
    c.is_nullable,
    c.is_identity,
    c.collation_name
FROM sys.objects o
JOIN sys.columns c ON c.object_id = o.object_id
JOIN sys.types ty ON ty.user_type_id = c.user_type_id
WHERE o.type = 'U'
  AND SCHEMA_NAME(o.schema_id) = 'dbo'
  AND o.name IN ('Orders', 'OrderItems', 'WriteReceipts')
ORDER BY o.name, c.column_id
"""

_KEY_SQL = """
SELECT
    o.name AS object_name,
    kc.name AS constraint_name,
    kc.type AS constraint_type,
    ic.key_ordinal,
    c.name AS column_name,
    ic.is_descending_key
FROM sys.key_constraints kc
JOIN sys.objects o ON o.object_id = kc.parent_object_id
JOIN sys.index_columns ic
  ON ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id
JOIN sys.columns c
  ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE SCHEMA_NAME(o.schema_id) = 'dbo'
  AND o.name IN ('Orders', 'OrderItems', 'WriteReceipts')
ORDER BY o.name, kc.name, ic.key_ordinal
"""

_INDEX_SQL = """
SELECT
    o.name AS object_name,
    i.name AS index_name,
    i.type_desc,
    i.is_unique,
    i.is_unique_constraint,
    i.is_primary_key,
    i.has_filter,
    i.filter_definition,
    ic.key_ordinal,
    ic.is_included_column,
    ic.is_descending_key,
    c.name AS column_name
FROM sys.indexes i
JOIN sys.objects o ON o.object_id = i.object_id
JOIN sys.index_columns ic
  ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c
  ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE o.type = 'U'
  AND SCHEMA_NAME(o.schema_id) = 'dbo'
  AND o.name IN ('Orders', 'OrderItems', 'WriteReceipts')
  AND i.name IS NOT NULL
ORDER BY o.name, i.name, ic.key_ordinal, ic.index_column_id
"""

_FK_SQL = """
SELECT
    parent.name AS object_name,
    fk.name AS constraint_name,
    ref.name AS referenced_object,
    pc.name AS parent_column,
    rc.name AS referenced_column,
    fkc.constraint_column_id,
    fk.delete_referential_action_desc,
    fk.update_referential_action_desc
FROM sys.foreign_keys fk
JOIN sys.objects parent ON parent.object_id = fk.parent_object_id
JOIN sys.objects ref ON ref.object_id = fk.referenced_object_id
JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
JOIN sys.columns pc
  ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN sys.columns rc
  ON rc.object_id = fkc.referenced_object_id
 AND rc.column_id = fkc.referenced_column_id
WHERE SCHEMA_NAME(parent.schema_id) = 'dbo'
  AND parent.name IN ('Orders', 'OrderItems', 'WriteReceipts')
  AND ref.name IN ('Orders', 'OrderItems', 'WriteReceipts')
ORDER BY parent.name, fk.name, fkc.constraint_column_id
"""

_CHECK_SQL = """
SELECT
    o.name AS object_name,
    cc.name AS constraint_name,
    cc.definition,
    cc.is_disabled,
    cc.is_not_trusted
FROM sys.check_constraints cc
JOIN sys.objects o ON o.object_id = cc.parent_object_id
WHERE SCHEMA_NAME(o.schema_id) = 'dbo'
  AND o.name IN ('Orders', 'OrderItems', 'WriteReceipts')
ORDER BY o.name, cc.name
"""


def _normal(value: Any) -> Any:
    if isinstance(value, bytes):
        return value.hex()
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)


def _rows(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [{key: _normal(value) for key, value in row.items()} for row in rows]


@dataclass(frozen=True)
class DatabaseSchema:
    key: str
    database: str
    columns: list[dict[str, Any]]
    keys: list[dict[str, Any]]
    indexes: list[dict[str, Any]]
    foreign_keys: list[dict[str, Any]]
    checks: list[dict[str, Any]]
    signature: str


@dataclass(frozen=True)
class TopologyReport:
    shard_count: int
    database_names: dict[str, str]
    schema_signature: str
    tenant_shards: dict[str, int]

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "shardCount": self.shard_count,
            "databases": self.database_names,
            "schemaSignature": self.schema_signature,
            "tenantShards": self.tenant_shards,
        }


class TopologyValidator:
    def __init__(self, hub: DatabaseHub, shard_count: int):
        self.hub = hub
        self.shard_count = shard_count

    def validate(self) -> TopologyReport:
        schemas: dict[str, DatabaseSchema] = {}
        failures: list[dict[str, Any]] = []
        for key in ["legacy", *[f"shard{i}" for i in range(self.shard_count)]]:
            try:
                schemas[key] = self._read_schema(key)
            except DatabaseAccessError as exc:
                failures.append(
                    {
                        "database": key,
                        "reason": "connection_or_query_failed",
                        "message": str(exc.original),
                    }
                )
            except Exception as exc:
                failures.append(
                    {
                        "database": key,
                        "reason": "schema_query_failed",
                        "message": str(exc),
                    }
                )

        if failures:
            raise topology_error(
                "one or more databases are unavailable; service did not load",
                failures=failures,
            )

        reference = schemas["legacy"]
        mismatches: list[dict[str, Any]] = []
        for key, schema in schemas.items():
            object_names = {
                row["object_name"]
                for row in schema.columns
                if row["object_name"] in REQUIRED_TABLES
            }
            missing = sorted(set(REQUIRED_TABLES) - object_names)
            if missing:
                mismatches.append(
                    {"database": key, "reason": "missing_tables", "tables": missing}
                )
            index_names = {
                row["index_name"] for row in schema.indexes if row["index_name"]
            }
            for table, required in REQUIRED_INDEXES.items():
                for index in required:
                    if index not in index_names:
                        mismatches.append(
                            {
                                "database": key,
                                "reason": "missing_index",
                                "table": table,
                                "index": index,
                            }
                        )
            if schema.signature != reference.signature:
                mismatches.append(
                    {
                        "database": key,
                        "reason": "schema_signature_mismatch",
                        "expected": reference.signature,
                        "actual": schema.signature,
                    }
                )

        if mismatches:
            raise topology_error(
                "legacy and shard schemas are not identical; service did not load",
                mismatches=mismatches,
                databases={key: value.database for key, value in schemas.items()},
            )

        names = {key: value.database for key, value in schemas.items()}
        return TopologyReport(
            shard_count=self.shard_count,
            database_names=names,
            schema_signature=reference.signature,
            tenant_shards={
                "formula": "tenant_id % shard_count",
                "exampleTenantId": 7,
                "exampleShard": tenant_shard(7, self.shard_count),
            },
        )

    def locate_shard(self, tenant_id: int) -> int:
        if tenant_id < 0:
            raise AppError(
                "E_VALIDATION", "tenantId must be non-negative", http_status=400
            )
        return tenant_shard(tenant_id, self.shard_count)

    def _read_schema(self, key: str) -> DatabaseSchema:
        with self.hub.read(key) as session:
            columns = _rows(session.query(_COLUMNS_SQL))
            keys = _rows(session.query(_KEY_SQL))
            indexes = _rows(session.query(_INDEX_SQL))
            foreign_keys = _rows(session.query(_FK_SQL))
            checks = _rows(session.query(_CHECK_SQL))
        canonical = json.dumps(
            {
                "columns": columns,
                "keys": keys,
                "indexes": indexes,
                "foreignKeys": foreign_keys,
                "checks": checks,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return DatabaseSchema(
            key=key,
            database=self.hub.database_name(key),
            columns=columns,
            keys=keys,
            indexes=indexes,
            foreign_keys=foreign_keys,
            checks=checks,
            signature=hashlib.sha256(canonical).hexdigest(),
        )
