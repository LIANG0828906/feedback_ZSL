"""Watermark-aware streaming deduplication with bounded-input parity."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any

import pyarrow as pa

from .catalog import Catalog
from .config import ExecutorConfig
from .dialects import (
    DialectSpec,
    compile_expr,
    compile_order_item,
    quote_ident,
)
from .errors import (
    ERROR_RETRY_EXHAUSTED,
    ERROR_SQL,
    ExecutorError,
)
from .execution import (
    ExecutionResult,
    _atomic_copy_to_csv_or_parquet,
    _sha256,
    _unique_output_names,
    duck_session,
)
from .runtime import (
    FaultController,
    InjectedFault,
    JobWorkspace,
    MemoryGovernor,
    TempQuota,
)
from .sql import ColumnRef, Literal, OrderItem, Query, and_expr


@dataclass
class StreamOutcome:
    rows: list[dict[str, Any]]
    filtered_rows: int
    late_records: int
    late_same_key: int
    input_rows: int
    watermark: datetime | None


def _as_datetime(value: Any) -> datetime | None:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value
    if hasattr(value, "to_pydatetime"):
        return value.to_pydatetime()
    if isinstance(value, str):
        try:
            return datetime.fromisoformat(value)
        except ValueError:
            return None
    return None


def run_streaming_dedup(
    query: Query,
    catalog: Catalog,
    dialect: DialectSpec,
    config: ExecutorConfig,
    workspace: JobWorkspace,
    result_path: Path,
) -> ExecutionResult:
    if len(query.tables) != 1:
        raise ExecutorError(ERROR_SQL, "streaming dedup expects exactly one source table")
    if not query.distinct_on:
        raise ExecutorError(ERROR_SQL, "streaming query requires DISTINCT ON (...)")
    table_ref = query.tables[0]
    table = catalog.table(table_ref.name)
    stream_info = catalog.stream
    event_time_name = (
        stream_info.event_time_column if stream_info else "event_ts"
    )
    ingest_name = (
        stream_info.ingest_order_column if stream_info else "received_seq"
    )
    event_time = table.column(event_time_name)
    ingest = table.column(ingest_name)
    allowed_lateness = timedelta(
        seconds=(
            config.allowed_lateness_seconds
            if config.allowed_lateness_seconds is not None
            else (stream_info.allowed_lateness_seconds if stream_info else 0)
        )
    )
    memory = MemoryGovernor(config.memory_limit_bytes)
    temp = TempQuota(workspace.root, config.temp_limit_bytes)
    fault = FaultController(config, workspace.state)

    def source_mapper(alias: str, column: str) -> str:
        if alias and alias.lower() != table_ref.alias.lower():
            raise ExecutorError(
                ERROR_SQL, f"stream filter references non-source alias {alias}"
            )
        return quote_ident(table.column(column).name)

    filter_expr = and_expr(
        [item for item in [query.where] if item is not None]
    )
    dedup_columns = [
        _column_name(query, expr, catalog, {table_ref.alias})
        for expr in query.distinct_on
    ]
    def process_once():
        local_input_rows = 0
        local_filtered_rows = 0
        local_late_records = 0
        local_late_same_key = 0
        local_candidates: dict[
            tuple[Any, ...], tuple[datetime | None, int, dict[str, Any]]
        ] = {}
        local_finalized: set[tuple[Any, ...]] = set()
        local_watermark: datetime | None = None
        batch_index = 0
        with duck_session(config, workspace) as con:
            for batch in catalog.iter_batches(table, config.stream_batch_rows):
                fault.check("stream", 0, batch_index)
                batch_index += 1
                memory.reserve(batch.nbytes)
                try:
                    local_input_rows += batch.num_rows
                    arrow_table = pa.Table.from_batches([batch])
                    if filter_expr is not None:
                        con.register("_stream_batch", arrow_table)
                        where_sql = compile_expr(
                            filter_expr,
                            dialect,
                            catalog,
                            query,
                            source_mapper,
                        )
                        filtered = con.execute(
                            f"SELECT * FROM _stream_batch WHERE {where_sql}"
                        ).fetch_arrow_table()
                        con.unregister("_stream_batch")
                        arrow_table = filtered
                    if (
                        arrow_table.num_rows
                        and ingest.name in arrow_table.column_names
                    ):
                        arrow_table = arrow_table.sort_by(
                            [(ingest.name, "ascending")]
                        )
                    local_filtered_rows += arrow_table.num_rows
                    rows = arrow_table.to_pylist()
                    if config.stream_mode == "incremental":
                        batch_max: datetime | None = None
                        for row in rows:
                            ts = _as_datetime(row.get(event_time.name))
                            if ts is not None and (
                                batch_max is None or ts > batch_max
                            ):
                                batch_max = ts
                        for row in rows:
                            ts = _as_datetime(row.get(event_time.name))
                            key = tuple(row.get(name) for name in dedup_columns)
                            seq = int(row.get(ingest.name) or 0)
                            if local_watermark is not None and (
                                (ts is not None and ts < local_watermark)
                                or key in local_finalized
                            ):
                                local_late_records += 1
                                if (
                                    key in local_candidates
                                    or key in local_finalized
                                ):
                                    local_late_same_key += 1
                                continue
                            existing = local_candidates.get(key)
                            if existing is None or (
                                ts is not None
                                and (existing[0] is None or ts < existing[0])
                            ) or (
                                existing[0] == ts and seq < existing[1]
                            ):
                                local_candidates[key] = (ts, seq, row)
                        if batch_max is not None:
                            candidate_watermark = batch_max - allowed_lateness
                            local_watermark = (
                                candidate_watermark
                                if local_watermark is None
                                else max(local_watermark, candidate_watermark)
                            )
                            for key, (ts, _, _) in list(
                                local_candidates.items()
                            ):
                                if ts is not None and ts < local_watermark:
                                    local_finalized.add(key)
                    else:
                        for row in rows:
                            ts = _as_datetime(row.get(event_time.name))
                            key = tuple(row.get(name) for name in dedup_columns)
                            seq = int(row.get(ingest.name) or 0)
                            existing = local_candidates.get(key)
                            if existing is None or (
                                ts is not None
                                and (existing[0] is None or ts < existing[0])
                            ) or (
                                existing[0] == ts and seq < existing[1]
                            ):
                                local_candidates[key] = (ts, seq, row)
                finally:
                    memory.release(batch.nbytes)
        return (
            local_input_rows,
            local_filtered_rows,
            local_late_records,
            local_late_same_key,
            local_candidates,
            local_watermark,
        )

    attempts = 0
    while True:
        try:
            (
                input_rows,
                filtered_rows,
                late_records,
                late_same_key,
                candidates,
                watermark,
            ) = process_once()
            break
        except InjectedFault as exc:
            attempts += 1
            partition_id = "stream:p0000"
            workspace.state.record_retry(partition_id, attempts, str(exc))
            if attempts > config.max_retries:
                raise ExecutorError(
                    ERROR_RETRY_EXHAUSTED,
                    f"partition {partition_id} still failing after retries",
                    {
                        "partition_id": partition_id,
                        "attempts": attempts,
                        "last_error": str(exc),
                        "completed_partitions": workspace.state.completed_partition_ids(),
                    },
                ) from exc

    output_rows = [
        value[2]
        for _, value in sorted(candidates.items(), key=lambda item: repr(item[0]))
    ]
    output_names = _unique_output_names(query)
    output_aliases = {name.lower() for name in output_names}
    select_sqls = [
        f"{compile_expr(item.expr, dialect, catalog, query, source_mapper)} "
        f"AS {quote_ident(name)}"
        for name, item in zip(output_names, query.select)
    ]
    order_sqls: list[str] = []
    used: set[str] = set()
    alias_lookup = {name.lower(): name for name in output_names}
    for item in query.order_by:
        out_name = None
        if isinstance(item.expr, Literal) and item.expr.kind == "int":
            ordinal = int(item.expr.value)
            if 1 <= ordinal <= len(output_names):
                out_name = output_names[ordinal - 1]
        if isinstance(item.expr, ColumnRef) and item.expr.table is None:
            out_name = out_name or alias_lookup.get(item.expr.name.lower())
        if out_name is None:
            for name, select_item in zip(output_names, query.select):
                if select_item.expr == item.expr:
                    out_name = name
                    break
        if out_name is None:
            # DISTINCT ON often orders by an ingest sequence used only as a
            # tie-breaker. It is consumed above and need not appear in output.
            continue
        order_sqls.append(
            compile_order_item(
                OrderItem(ColumnRef(None, out_name), item.direction, item.nulls),
                dialect,
                catalog,
                query,
                output_aliases,
            )
        )
        used.add(out_name.lower())
    for name in output_names:
        if name.lower() not in used:
            order_sqls.append(
                compile_order_item(
                    OrderItem(ColumnRef(None, name), "ASC", None),
                    dialect,
                    catalog,
                    query,
                    output_aliases,
                )
            )

    if output_rows:
        source_table = pa.Table.from_pylist(output_rows, schema=table.arrow_schema)
    else:
        source_table = table.arrow_schema.empty_table()
    select_query = "SELECT " + ", ".join(select_sqls) + " FROM _dedup_source"
    if order_sqls:
        select_query += " ORDER BY " + ", ".join(order_sqls)
    if query.limit is not None:
        select_query += f" LIMIT {int(query.limit)}"
    with duck_session(config, workspace) as con:
        con.register("_dedup_source", source_table)
        output_count = int(
            con.execute(f"SELECT count(*) FROM ({select_query})").fetchone()[0]
        )
        _atomic_copy_to_csv_or_parquet(
            con, select_query, result_path, config.result_format
        )
        con.unregister("_dedup_source")
    temp.check()
    workspace.state.mark_completed("stream", 0, [result_path], output_count)
    metrics = [
        _stream_metric("stream", 0, "stream_scan", input_rows, filtered_rows),
        _stream_metric("stream", 0, "watermark_dedup", filtered_rows, min(filtered_rows, len(candidates))),
        _stream_metric(
            "stream",
            0,
            "projection_sort",
            min(filtered_rows, len(candidates)),
            output_count,
        ),
    ]
    metrics.sort(key=lambda item: (item["stage"], item["partition"], item["operator"]))
    tree = {
        "node_id": "stream_dedup",
        "operator": "distinct_on_watermark",
        "mode": config.stream_mode,
        "dedup_keys": [repr(item) for item in query.distinct_on],
        "actual_rows": output_count,
        "children": [
            {
                "node_id": "stream_scan",
                "operator": "scan",
                "table": table.name,
                "actual_rows": filtered_rows,
                "children": [],
            }
        ],
    }
    tree = {
        "node_id": "stream_output_sort",
        "operator": "projection_sort",
        "actual_rows": output_count,
        "children": [tree],
    }
    return ExecutionResult(
        result_path=result_path,
        result_rows=output_count,
        result_sha256=_sha256(result_path),
        metrics=metrics,
        actual_rows={"stream_scan": filtered_rows, "stream_dedup": output_count},
        plan_tree=tree,
        repartition_operations=0,
        repartition_partitions=0,
        max_active_partitions=1,
        peak_memory_bytes=memory.peak_bytes,
        temp_peak_bytes=temp.peak_bytes,
        spill_bytes=workspace.spill_bytes + workspace.duckdb_spill_bytes(),
        completed_partitions=workspace.state.completed_partition_ids(),
        retry_partitions=workspace.state.retry_list(),
        stream={
            "mode": config.stream_mode,
            "late_records": late_records,
            "late_same_key_records": late_same_key,
            "final_watermark": (
                watermark.isoformat()
                if watermark is not None
                else None
            ),
            "bounded_incremental_note": (
                "bounded and incremental modes use the same earliest-event rule; "
                "late duplicates that would revise an emitted key are counted and dropped"
            ),
        },
    )


def _column_name(
    query: Query,
    expr: Any,
    catalog: Catalog,
    aliases: set[str],
) -> str:
    if not isinstance(expr, ColumnRef):
        raise ExecutorError(ERROR_SQL, "DISTINCT ON currently requires column keys")
    _, info = catalog.resolve_column(query, expr, aliases)
    return info.name
def _stream_metric(
    stage: str, partition: int, operator: str, input_rows: int, output_rows: int
) -> dict[str, Any]:
    return {
        "stage": stage,
        "partition": partition,
        "operator": operator,
        "input_rows": int(input_rows),
        "output_rows": int(output_rows),
    }

