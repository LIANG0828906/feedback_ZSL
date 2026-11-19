"""Schema-driven local CSV/Parquet catalog."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator

import pyarrow as pa
import pyarrow.compute as pc
import pyarrow.dataset as ds

from .errors import ERROR_CONFIG, ERROR_IO, ExecutorError
from .sql import ColumnRef, Query, TableRef


INTEGER_TYPES = {
    "TINYINT",
    "SMALLINT",
    "INTEGER",
    "INT",
    "BIGINT",
    "HUGEINT",
    "UTINYINT",
    "USMALLINT",
    "UINTEGER",
    "UBIGINT",
}
FLOAT_TYPES = {"REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "DECIMAL", "NUMERIC"}
STRING_TYPES = {"VARCHAR", "TEXT", "STRING", "CHAR", "BPCHAR", "UUID"}
TIMESTAMP_TYPES = {"TIMESTAMP", "DATETIME", "TIMESTAMPTZ"}
DATE_TYPES = {"DATE"}
BOOL_TYPES = {"BOOLEAN", "BOOL"}


def normalize_type_name(type_name: str) -> str:
    upper = re.sub(r"\s+", " ", type_name.strip().upper())
    upper = re.sub(r"\([^)]*\)", "", upper).strip()
    if upper == "TIMESTAMP WITHOUT TIME ZONE":
        return "TIMESTAMP"
    if upper == "TIMESTAMP WITH TIME ZONE":
        return "TIMESTAMPTZ"
    return upper


def pa_type(type_name: str) -> pa.DataType:
    base = normalize_type_name(type_name)
    if base in {"TINYINT", "SMALLINT", "INTEGER", "INT", "BIGINT", "HUGEINT"}:
        return pa.int64()
    if base in {"UTINYINT", "USMALLINT", "UINTEGER", "UBIGINT"}:
        return pa.uint64()
    if base in {"REAL", "FLOAT"}:
        return pa.float32()
    if base in {"DOUBLE", "DOUBLE PRECISION"}:
        return pa.float64()
    if base in {"DECIMAL", "NUMERIC"}:
        return pa.float64()
    if base in {"VARCHAR", "TEXT", "STRING", "CHAR", "BPCHAR", "UUID"}:
        return pa.string()
    if base == "TIMESTAMP":
        return pa.timestamp("us")
    if base == "TIMESTAMPTZ":
        return pa.timestamp("us", tz="UTC")
    if base == "DATE":
        return pa.date32()
    if base in BOOL_TYPES:
        return pa.bool_()
    if base in {"BINARY", "BLOB"}:
        return pa.binary()
    raise ExecutorError(ERROR_CONFIG, f"unsupported metadata type {type_name!r}")


def type_category(type_name: str) -> str:
    base = normalize_type_name(type_name)
    if base in INTEGER_TYPES:
        return "integer"
    if base in FLOAT_TYPES:
        return "float"
    if base in STRING_TYPES:
        return "string"
    if base in TIMESTAMP_TYPES:
        return "timestamp"
    if base in DATE_TYPES:
        return "date"
    if base in BOOL_TYPES:
        return "bool"
    return "other"


@dataclass(frozen=True)
class ColumnInfo:
    name: str
    type_name: str
    nullable: bool = True

    @property
    def category(self) -> str:
        return type_category(self.type_name)


@dataclass
class TableInfo:
    name: str
    files: tuple[Path, ...]
    format: str
    columns: dict[str, ColumnInfo]
    primary_key: tuple[str, ...] = ()
    unique_keys: tuple[tuple[str, ...], ...] = ()
    row_count: int | None = None
    column_stats: dict[str, dict[str, Any]] = field(default_factory=dict)

    def column(self, name: str) -> ColumnInfo:
        lower = name.lower()
        for col_name, info in self.columns.items():
            if col_name.lower() == lower:
                return info
        raise ExecutorError(
            ERROR_CONFIG, f"column {name!r} is not declared for table {self.name!r}"
        )

    @property
    def arrow_schema(self) -> pa.Schema:
        return pa.schema(
            [
                pa.field(name, pa_type(info.type_name), nullable=info.nullable)
                for name, info in self.columns.items()
            ]
        )

    def distinct_count(self, column: str) -> float:
        stats = self.column_stats.get(_column_lookup(self.columns, column), {})
        value = stats.get("distinct_count")
        if value is not None:
            return max(1.0, float(value))
        if column.lower() in {key.lower() for key in self.primary_key}:
            return max(1.0, float(self.row_count or 1))
        return max(1.0, float(self.row_count or 1) * 0.5)

    def is_unique_key(self, columns: tuple[str, ...]) -> bool:
        requested = tuple(col.lower() for col in columns)
        keys = [tuple(col.lower() for col in self.primary_key)]
        keys.extend(tuple(col.lower() for col in key) for key in self.unique_keys)
        return requested in keys


def _column_lookup(columns: dict[str, ColumnInfo], name: str) -> str:
    lower = name.lower()
    for key in columns:
        if key.lower() == lower:
            return key
    raise KeyError(name)


@dataclass
class StreamInfo:
    table: str
    event_time_column: str
    ingest_order_column: str
    dedup_keys: tuple[str, ...] = ()
    allowed_lateness_seconds: int = 30


class Catalog:
    def __init__(
        self,
        data_dir: str | Path,
        schema: dict[str, Any],
    ):
        self.data_dir = Path(data_dir).resolve()
        self.schema_path = self.data_dir / "schema.json"
        self.raw_schema = schema
        self.tables: dict[str, TableInfo] = {}
        self._load_tables(schema)
        self.stream = self._load_stream(schema)

    @classmethod
    def load(cls, data_dir: str | Path) -> "Catalog":
        root = Path(data_dir).resolve()
        schema_path = root / "schema.json"
        try:
            schema = json.loads(schema_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ExecutorError(
                ERROR_IO, f"cannot read schema metadata {schema_path}: {exc}"
            ) from exc
        return cls(root, schema)

    def _load_tables(self, schema: dict[str, Any]) -> None:
        raw_tables = schema.get("tables")
        if not isinstance(raw_tables, dict) or not raw_tables:
            raise ExecutorError(ERROR_CONFIG, "schema.json must define non-empty tables")
        for name, raw in raw_tables.items():
            if not isinstance(raw, dict):
                raise ExecutorError(ERROR_CONFIG, f"table {name} metadata must be object")
            files_raw = raw.get("files", [])
            if isinstance(files_raw, str):
                files_raw = [files_raw]
            files = tuple((self.data_dir / str(item)).resolve() for item in files_raw)
            if not files:
                raise ExecutorError(ERROR_CONFIG, f"table {name} has no files")
            for path in files:
                if not path.exists():
                    raise ExecutorError(ERROR_IO, f"input file does not exist: {path}")
            columns = self._parse_columns(name, raw.get("columns"))
            primary_key = tuple(raw.get("primary_key", []))
            unique_keys = tuple(tuple(item) for item in raw.get("unique_keys", []))
            if not (raw.get("format") in {"csv", "parquet"}):
                # Infer from the first suffix, but metadata should remain explicit.
                suffix = files[0].suffix.lower()
                fmt = "parquet" if suffix == ".parquet" else "csv"
            else:
                fmt = raw["format"]
            row_count = raw.get("row_count")
            stats = raw.get("stats", {})
            self.tables[name.lower()] = TableInfo(
                name=name,
                files=files,
                format=fmt,
                columns=columns,
                primary_key=primary_key,
                unique_keys=unique_keys,
                row_count=int(row_count) if row_count is not None else None,
                column_stats=stats if isinstance(stats, dict) else {},
            )

    def _parse_columns(self, table: str, raw_columns: Any) -> dict[str, ColumnInfo]:
        if isinstance(raw_columns, dict):
            return {
                name: ColumnInfo(name, str(type_name))
                for name, type_name in raw_columns.items()
            }
        if isinstance(raw_columns, list):
            out: dict[str, ColumnInfo] = {}
            for item in raw_columns:
                if not isinstance(item, dict) or "name" not in item or "type" not in item:
                    raise ExecutorError(
                        ERROR_CONFIG,
                        f"table {table} column declarations need name and type",
                    )
                out[item["name"]] = ColumnInfo(
                    str(item["name"]),
                    str(item["type"]),
                    bool(item.get("nullable", True)),
                )
            return out
        raise ExecutorError(ERROR_CONFIG, f"table {table} has invalid columns metadata")

    def _load_stream(self, schema: dict[str, Any]) -> StreamInfo | None:
        raw = schema.get("stream")
        if not raw:
            return None
        return StreamInfo(
            table=str(raw.get("table", "stream_events")),
            event_time_column=str(raw.get("event_time_column", "event_ts")),
            ingest_order_column=str(raw.get("ingest_order_column", "received_seq")),
            dedup_keys=tuple(str(x) for x in raw.get("dedup_keys", [])),
            allowed_lateness_seconds=int(raw.get("allowed_lateness_seconds", 30)),
        )

    def table(self, name: str) -> TableInfo:
        key = name.split(".")[-1].lower()
        if key not in self.tables:
            raise ExecutorError(ERROR_CONFIG, f"unknown table {name!r}")
        return self.tables[key]

    def query_tables(self, query: Query) -> dict[str, TableRef]:
        return query.table_map()

    def resolve_column(
        self, query: Query, ref: ColumnRef, aliases: set[str] | None = None
    ) -> tuple[str, ColumnInfo]:
        candidates: list[tuple[str, ColumnInfo]] = []
        allowed = aliases or set(query.table_map())
        for table_ref in query.tables:
            if table_ref.alias not in allowed:
                continue
            if ref.table and table_ref.alias.lower() != ref.table.lower():
                continue
            info = self.table(table_ref.name)
            lower = ref.name.lower()
            for name, col in info.columns.items():
                if name.lower() == lower:
                    candidates.append((table_ref.alias, col))
                    break
        if not candidates:
            raise ExecutorError(
                ERROR_CONFIG,
                f"unknown column {ref.name!r} in table set {sorted(allowed)}",
            )
        if len(candidates) > 1:
            raise ExecutorError(
                ERROR_CONFIG,
                f"ambiguous column {ref.name!r}; qualify it with a table alias",
            )
        return candidates[0]

    def ensure_statistics(self) -> None:
        for info in self.tables.values():
            if info.row_count is None:
                info.row_count = self.count_rows(info)
            if not info.column_stats:
                self._compute_statistics(info)
            else:
                available = {name.lower() for name in info.column_stats}
                for col in info.columns:
                    if col.lower() not in available:
                        self._compute_column_statistic(info, col)

    def _dataset(self, info: TableInfo) -> ds.Dataset:
        try:
            return ds.dataset(
                [str(path) for path in info.files],
                format=info.format,
                schema=info.arrow_schema,
            )
        except Exception as exc:
            raise ExecutorError(
                ERROR_IO, f"cannot open table {info.name}: {exc}"
            ) from exc

    def count_rows(self, info: TableInfo) -> int:
        try:
            return int(self._dataset(info).count_rows())
        except Exception as exc:
            raise ExecutorError(
                ERROR_IO, f"cannot count table {info.name}: {exc}"
            ) from exc

    def _compute_statistics(self, info: TableInfo) -> None:
        for column in info.columns:
            self._compute_column_statistic(info, column)

    def _compute_column_statistic(self, info: TableInfo, column: str) -> None:
        arrays: list[pa.Array] = []
        try:
            for batch in self.iter_batches(info, 16384, columns=[column]):
                arrays.append(batch.column(0))
            if arrays:
                combined = pa.concat_arrays(arrays)
                distinct = int(pc.count_distinct(combined).as_py())
                null_count = int(combined.null_count)
            else:
                distinct = 0
                null_count = 0
        except Exception as exc:
            raise ExecutorError(
                ERROR_IO,
                f"cannot compute statistics for {info.name}.{column}: {exc}",
            ) from exc
        info.column_stats[_column_lookup(info.columns, column)] = {
            "distinct_count": max(1, distinct),
            "null_count": null_count,
        }

    def iter_batches(
        self,
        info: TableInfo,
        batch_rows: int,
        columns: list[str] | None = None,
    ) -> Iterator[pa.RecordBatch]:
        try:
            dataset = self._dataset(info)
            selected = columns or list(info.columns)
            for batch in dataset.to_batches(columns=selected, batch_size=batch_rows):
                if info.format == "csv":
                    arrays = []
                    changed = False
                    for name, array in zip(batch.schema.names, batch.columns):
                        col = info.column(name)
                        if col.nullable and pa.types.is_string(array.type):
                            mask = pc.equal(array, pa.scalar(""))
                            if bool(pc.any(mask).as_py()):
                                array = pc.if_else(mask, pa.nulls(len(array), type=array.type), array)
                                changed = True
                        arrays.append(array)
                    if changed:
                        batch = pa.RecordBatch.from_arrays(arrays, schema=batch.schema)
                yield batch
        except Exception as exc:
            raise ExecutorError(
                ERROR_IO, f"cannot read table {info.name}: {exc}"
            ) from exc

