"""Columnar, hash-repartitioned execution over local files."""

from __future__ import annotations

import hashlib
import os
import shutil
import tempfile
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import duckdb
import pyarrow.parquet as pq

from .catalog import TableInfo
from .config import ExecutorConfig
from .dialects import (
    _int_div_sql,
    _mod_sql,
    compile_expr,
    compile_order_item,
    expr_category,
    literal_sql,
    quote_ident,
    sql_string,
)
from .errors import ERROR_IO, ERROR_MEMORY_LIMIT, ERROR_SQL, ExecutorError
from .planner import (
    BasePlan,
    JoinPlan,
    PartialAggPlan,
    PlanningResult,
    PlanNode,
    plan_to_tree,
)
from .runtime import (
    FaultController,
    JobWorkspace,
    MemoryGovernor,
    PartitionScheduler,
    TaskResult,
    TempQuota,
    directory_size,
    parquet_rows,
    scan_for_fault,
)
from .sql import (
    AGGREGATE_NAMES,
    Between,
    BinaryOp,
    Cast,
    ColumnRef,
    Expr,
    FuncCall,
    IsNull,
    Literal,
    OrderItem,
    Query,
    Star,
    UnaryOp,
    and_expr,
)


@dataclass
class Relation:
    name: str
    files: tuple[Path, ...]
    aliases: frozenset[str]
    column_map: dict[tuple[str, str], str]
    phys_columns: tuple[str, ...]
    tables: dict[str, TableInfo]
    partial: PartialAggPlan | None = None

    def physical(self, alias: str, column: str) -> str:
        key = (alias.lower(), column.lower())
        try:
            return self.column_map[key]
        except KeyError as exc:
            raise ExecutorError(
                ERROR_SQL,
                f"column {alias}.{column} is not available after pushdown",
            ) from exc

    @property
    def rows(self) -> int:
        return sum(parquet_rows(path) for path in self.files)

    def merged(self, other: "Relation", name: str) -> "Relation":
        overlap = set(self.column_map) & set(other.column_map)
        if overlap:
            raise ExecutorError(
                ERROR_SQL, f"duplicate physical column mapping: {sorted(overlap)[:1]}"
            )
        return Relation(
            name=name,
            files=self.files + other.files,
            aliases=self.aliases | other.aliases,
            column_map={**self.column_map, **other.column_map},
            phys_columns=self.phys_columns + other.phys_columns,
            tables={**self.tables, **other.tables},
            partial=None,
        )


@dataclass
class ExecutionResult:
    result_path: Path
    result_rows: int
    result_sha256: str
    metrics: list[dict[str, Any]]
    actual_rows: dict[str, int]
    plan_tree: dict[str, Any]
    repartition_operations: int
    repartition_partitions: int
    max_active_partitions: int
    peak_memory_bytes: int
    temp_peak_bytes: int
    spill_bytes: int
    completed_partitions: list[str]
    retry_partitions: list[dict[str, Any]]
    stream: dict[str, Any] | None = None


def _safe_name(name: str) -> str:
    return "".join(ch if ch.isalnum() or ch in {"-", "_"} else "_" for ch in name)


def _read_parquet_sql(files: Iterable[Path]) -> str:
    paths = ", ".join(sql_string(str(Path(path).resolve()).replace("\\", "/")) for path in files)
    return f"read_parquet([{paths}], union_by_name=true)"


def _copy_sql(query: str, path: Path) -> str:
    return (
        f"COPY ({query}) TO {sql_string(str(path.resolve()).replace(chr(92), '/'))} "
        "(FORMAT PARQUET, COMPRESSION ZSTD)"
    )


def _atomic_copy_to_csv_or_parquet(
    con: duckdb.DuckDBPyConnection, query: str, target: Path, fmt: str
) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    tmp = target.with_name(target.name + ".tmp")
    if tmp.exists():
        tmp.unlink()
    path = str(tmp.resolve()).replace("\\", "/")
    if fmt == "csv":
        sql = (
            f"COPY ({query}) TO {sql_string(path)} "
            "(FORMAT CSV, HEADER TRUE, DELIMITER ',', QUOTE '\"', "
            "ESCAPE '\"', NULL '')"
        )
    else:
        sql = _copy_sql(query, tmp)
    con.execute(sql)
    os.replace(tmp, target)


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


@contextmanager
def duck_session(config: ExecutorConfig, workspace: JobWorkspace):
    spill_dir = Path(
        tempfile.mkdtemp(prefix="duck-", dir=str(workspace.duckdb_spill_dir))
    )
    con = duckdb.connect()
    try:
        con.execute("SET threads=1")
        con.execute(f"SET memory_limit='{config.duckdb_memory_limit_mb}MB'")
        con.execute(
            "SET temp_directory="
            + sql_string(str(spill_dir).replace("\\", "/"))
        )
        try:
            per_worker_temp_mb = max(
                32, config.temp_limit_mb // config.max_workers
            )
            con.execute(
                f"SET max_temp_directory_size='{per_worker_temp_mb}MB'"
            )
        except duckdb.Error:
            # Older DuckDB 1.x builds may not expose the setting; the
            # application-level quota still checks after every partition.
            pass
        con.execute("SET preserve_insertion_order=false")
        con.execute("SET enable_progress_bar=false")
        yield con
    finally:
        try:
            con.close()
        finally:
            used = directory_size(spill_dir)
            if used:
                workspace.record_spill_bytes(used)
            shutil.rmtree(spill_dir, ignore_errors=True)


def _unique_output_names(query: Query) -> list[str]:
    seen: dict[str, int] = {}
    out: list[str] = []
    for item in query.select:
        base = item.output_name or "expr"
        count = seen.get(base.lower(), 0) + 1
        seen[base.lower()] = count
        out.append(base if count == 1 else f"{base}_{count}")
    return out


def _partial_group_name(index: int) -> str:
    return f"__partial_g{index}"


def _partial_state_name(index: int, kind: str) -> str:
    return f"__partial_a{index}_{kind}"


class RuntimeExecutor:
    def __init__(
        self,
        planning: PlanningResult,
        config: ExecutorConfig,
        workspace: JobWorkspace,
    ):
        self.planning = planning
        self.query = planning.query
        self.catalog = planning.catalog
        self.dialect = planning.dialect
        self.config = config
        self.workspace = workspace
        self.memory = MemoryGovernor(config.memory_limit_bytes)
        self.temp = TempQuota(workspace.root, config.temp_limit_bytes)
        self.fault = FaultController(config, workspace.state)
        self.scheduler = PartitionScheduler(
            config, workspace.state, self.fault, self.memory, self.temp
        )
        self.metrics: list[dict[str, Any]] = []
        self.actual_rows: dict[str, int] = {}
        self.repartition_operations = 0
        self.repartition_partitions = 0
        self.cache_files: dict[str, Path] = {}
        self.table_rows: dict[str, int] = {}
        self._join_counter = 0

    def execute(self, result_path: Path) -> ExecutionResult:
        self._materialize_tables()
        relation = self._execute_node(self.planning.root)
        final_relation, final_per_partition = self._final_aggregate(relation)
        output_names = _unique_output_names(self.query)
        rows = self._global_sort(final_relation, output_names, result_path)
        self.metrics.sort(
            key=lambda item: (
                str(item.get("stage", "")),
                int(item.get("partition", 0)),
                str(item.get("operator", "")),
                int(item.get("input_rows", 0)),
                int(item.get("output_rows", 0)),
            )
        )
        tree = plan_to_tree(
            self.planning.root, self.actual_rows, self.config.partitions
        )
        tree = {
            "node_id": "output_sort",
            "operator": "sort",
            "estimated_rows": self.actual_rows.get("final_aggregate"),
            "actual_rows": rows,
            "children": [
                {
                    "node_id": "final_aggregate",
                    "operator": "aggregate",
                    "estimated_rows": self.planning.root.row_estimate,
                    "actual_rows": self.actual_rows.get("final_aggregate"),
                    "children": [tree],
                }
            ],
        }
        return ExecutionResult(
            result_path=result_path,
            result_rows=rows,
            result_sha256=_sha256(result_path),
            metrics=self.metrics,
            actual_rows=self.actual_rows,
            plan_tree=tree,
            repartition_operations=self.repartition_operations,
            repartition_partitions=self.repartition_partitions,
            max_active_partitions=self.scheduler._max_active,
            peak_memory_bytes=self.memory.peak_bytes,
            temp_peak_bytes=self.temp.peak_bytes,
            spill_bytes=self.workspace.spill_bytes + self.workspace.duckdb_spill_bytes(),
            completed_partitions=self.workspace.state.completed_partition_ids(),
            retry_partitions=self.workspace.state.retry_list(),
        )

    # ------------------------------------------------------------------
    # Input materialization
    # ------------------------------------------------------------------
    def _materialize_tables(self) -> None:
        for alias, base in sorted(self.planning.bases.items()):
            table = base.table
            key = table.name.lower()
            if key in self.cache_files:
                self.table_rows[alias] = parquet_rows(self.cache_files[key])
                continue
            path = self.workspace.cache_dir / f"{_safe_name(table.name)}.parquet"
            tmp = path.with_name(path.name + ".tmp")
            if tmp.exists():
                tmp.unlink()
            writer = pq.ParquetWriter(
                tmp, table.arrow_schema, compression="zstd", use_dictionary=True
            )
            rows = 0
            try:
                for batch in self.catalog.iter_batches(
                    table, self.config.batch_rows
                ):
                    if batch.schema != table.arrow_schema:
                        batch = batch.cast(table.arrow_schema)
                    writer.write_batch(batch)
                    rows += batch.num_rows
            except Exception as exc:
                writer.close()
                raise ExecutorError(
                    ERROR_IO, f"failed to materialize {table.name}: {exc}"
                ) from exc
            writer.close()
            os.replace(tmp, path)
            self.temp.check()
            self.cache_files[key] = path
            self.table_rows[alias] = rows
            table.row_count = rows

    # ------------------------------------------------------------------
    # Base preparation and pushdown
    # ------------------------------------------------------------------
    def _execute_node(self, node: PlanNode) -> Relation:
        if isinstance(node, BasePlan):
            return self._prepare_base(node)
        return self._execute_join(node)

    def _prepare_base(self, base: BasePlan) -> Relation:
        stage_id = f"prepare_{_safe_name(base.alias)}"
        table = base.table
        cache = self.cache_files[table.name.lower()]
        column_pairs = [
            (base.alias, col)
            for col in table.columns
        ]
        column_map = {
            (alias.lower(), column.lower()): f"{_safe_name(alias)}__{_safe_name(column)}"
            for alias, column in column_pairs
        }
        partial = base.partial
        phys_columns = [column_map[(base.alias.lower(), col.lower())] for col in table.columns]
        if partial:
            phys_columns = [
                _partial_group_name(idx) for idx in range(len(partial.group_exprs))
            ]
            for idx, spec in enumerate(partial.aggregates):
                for kind in spec.state_kinds:
                    phys_columns.append(_partial_state_name(idx, kind))

        def task(partition: int) -> TaskResult:
            source_rows = scan_for_fault(
                [cache],
                stage_id,
                partition,
                self.fault,
                self.memory,
                self.config.batch_rows,
            )
            out_path = self.workspace.prepared_dir / f"{_safe_name(base.alias)}.parquet"
            tmp = out_path.with_name(out_path.name + ".tmp")
            if tmp.exists():
                tmp.unlink()
            with duck_session(self.config, self.workspace) as con:
                def source_mapper(table_alias: str, column: str) -> str:
                    if table_alias and table_alias.lower() != base.alias.lower():
                        raise ExecutorError(
                            ERROR_SQL,
                            f"filter references non-local table {table_alias}",
                        )
                    info = table.column(column)
                    return quote_ident(info.name)

                def filtered_mapper(table_alias: str, column: str) -> str:
                    if table_alias and table_alias.lower() != base.alias.lower():
                        raise ExecutorError(
                            ERROR_SQL,
                            f"expression references non-local table {table_alias}",
                        )
                    info = table.column(column)
                    return quote_ident(
                        column_map[(base.alias.lower(), info.name.lower())]
                    )

                select_parts = [
                    f"{quote_ident(info.name)} AS {quote_ident(column_map[(base.alias.lower(), name.lower())])}"
                    for name, info in table.columns.items()
                ]
                filter_expr = and_expr(base.filters)
                where_sql = ""
                if filter_expr is not None:
                    where_sql = " WHERE " + compile_expr(
                        filter_expr,
                        self.dialect,
                        self.catalog,
                        self.query,
                        source_mapper,
                    )
                con.execute(
                    "CREATE TEMP TABLE filtered AS SELECT "
                    + ", ".join(select_parts)
                    + f" FROM {_read_parquet_sql([cache])}"
                    + where_sql
                )
                filtered_rows = int(con.execute("SELECT count(*) FROM filtered").fetchone()[0])
                if partial:
                    group_sqls = [
                        compile_expr(
                            expr,
                            self.dialect,
                            self.catalog,
                            self.query,
                            filtered_mapper,
                        )
                        for expr in partial.group_exprs
                    ]
                    agg_parts: list[str] = []
                    for idx, spec in enumerate(partial.aggregates):
                        agg_parts.extend(
                            f"{sql} AS {quote_ident(name)}"
                            for name, sql in self._aggregate_state_select(
                                idx, spec, filtered_mapper
                            )
                        )
                    select_sql = ", ".join(
                        [
                            f"{sql} AS {quote_ident(_partial_group_name(idx))}"
                            for idx, sql in enumerate(group_sqls)
                        ]
                        + agg_parts
                    )
                    con.execute(
                        "CREATE TEMP TABLE prepared AS SELECT "
                        + select_sql
                        + " FROM filtered GROUP BY "
                        + ", ".join(group_sqls)
                        + " ORDER BY ALL"
                    )
                    prepared_rows = int(
                        con.execute("SELECT count(*) FROM prepared").fetchone()[0]
                    )
                else:
                    con.execute("CREATE TEMP TABLE prepared AS SELECT * FROM filtered")
                    prepared_rows = filtered_rows
                con.execute(_copy_sql("SELECT * FROM prepared ORDER BY ALL", tmp))
                self._check_memory_relation(tmp)
            os.replace(tmp, out_path)
            self.temp.check()
            stage_metrics = [
                self._metric(stage_id, partition, "scan", source_rows, filtered_rows),
                self._metric(stage_id, partition, "filter", source_rows, filtered_rows),
            ]
            if partial:
                stage_metrics.append(
                    self._metric(
                        stage_id, partition, "partial_aggregate", filtered_rows, prepared_rows
                    )
                )
                width = 32 * len(partial.group_exprs) + 24 * sum(
                    len(spec.state_kinds) for spec in partial.aggregates
                )
                estimated_groups = max(1, int(filtered_rows * 0.5))
                estimated_state = estimated_groups * width
                if (
                    self.config.force_spill
                    or estimated_state > self.config.agg_memory_threshold_bytes
                    or out_path.stat().st_size > self.config.agg_memory_threshold_bytes
                ):
                    spill_path = (
                        self.workspace.spill_dir
                        / "partial_aggregate"
                        / out_path.name
                    )
                    spill_path.parent.mkdir(parents=True, exist_ok=True)
                    os.replace(out_path, spill_path)
                    out_path = spill_path
                    self.workspace.record_spill_bytes(out_path.stat().st_size)
            self.actual_rows[f"scan_{base.alias}"] = filtered_rows
            if partial:
                self.actual_rows[f"scan_{base.alias}_partial_agg"] = prepared_rows
            return TaskResult((out_path,), prepared_rows, stage_metrics)

        outcome = self.scheduler.run_stage(stage_id, 1, task)
        result = outcome.partitions[0]
        self.metrics.extend(result.metrics)
        effective_column_map = dict(column_map)
        if partial:
            effective_column_map = {}
            for idx, group_expr in enumerate(partial.group_exprs):
                if isinstance(group_expr, ColumnRef) and group_expr.table:
                    effective_column_map[
                        (group_expr.table.lower(), group_expr.name.lower())
                    ] = _partial_group_name(idx)
        return Relation(
            name=base.alias,
            files=result.output_paths,
            aliases=frozenset({base.alias}),
            column_map=effective_column_map,
            phys_columns=tuple(phys_columns),
            tables={base.alias: table},
            partial=partial,
        )

    def _aggregate_state_select(
        self, spec_index: int, spec, source_mapper
    ) -> list[tuple[str, str]]:
        expr: FuncCall = spec.expr
        name = expr.name.upper()
        if name == "COUNT":
            if expr.args and isinstance(expr.args[0], Star):
                body = "COUNT(*)"
            elif expr.args:
                body = (
                    "COUNT("
                    + compile_expr(
                        expr.args[0],
                        self.dialect,
                        self.catalog,
                        self.query,
                        source_mapper,
                    )
                    + ")"
                )
            else:
                body = "COUNT(*)"
            return [(_partial_state_name(spec_index, "count"), body)]
        if not expr.args:
            raise ExecutorError(ERROR_SQL, f"{name} requires an argument")
        arg = compile_expr(
            expr.args[0],
            self.dialect,
            self.catalog,
            self.query,
            source_mapper,
        )
        if name == "SUM":
            category = expr_category(
                expr.args[0], self.dialect, self.catalog, self.query
            )
            body = f"SUM({arg})"
            if category == "integer":
                body = f"CAST({body} AS BIGINT)"
            return [(_partial_state_name(spec_index, "sum"), body)]
        if name == "AVG":
            return [
                (_partial_state_name(spec_index, "sum"), f"SUM({arg})"),
                (_partial_state_name(spec_index, "count"), f"COUNT({arg})"),
            ]
        if name in {"MIN", "MAX"}:
            return [
                (
                    _partial_state_name(spec_index, name.lower()),
                    f"{name}({arg})",
                )
            ]
        raise ExecutorError(ERROR_SQL, f"unsupported partial aggregate {name}")

    def _check_memory_relation(self, path: Path) -> None:
        size = path.stat().st_size if path.exists() else 0
        # A vectorized partition should fit comfortably inside its worker slice.
        per_worker_limit = max(
            32 * 1024 * 1024,
            self.config.memory_limit_bytes // self.config.max_workers,
        )
        if size > per_worker_limit:
            raise ExecutorError(
                ERROR_MEMORY_LIMIT,
                f"partition output {path.name} exceeds memory limit",
                {"bytes": size, "limit_bytes": per_worker_limit},
            )

    # ------------------------------------------------------------------
    # Hash repartitioned joins
    # ------------------------------------------------------------------
    def _execute_join(self, node: JoinPlan) -> Relation:
        left = self._execute_node(node.left)
        right = self._execute_node(node.right)
        self._join_counter += 1
        stage_id = f"join_{self._join_counter:02d}"
        partitions = self.config.partitions
        left_keys, right_keys = self._join_keys(node, left, right)
        left_sources = list(left.files)
        right_sources = list(right.files)
        left_total = sum(parquet_rows(path) for path in left_sources)
        right_total = sum(parquet_rows(path) for path in right_sources)
        left_repart_dir = self.workspace.repart_dir / stage_id / "left"
        right_repart_dir = self.workspace.repart_dir / stage_id / "right"
        left_repart_dir.mkdir(parents=True, exist_ok=True)
        right_repart_dir.mkdir(parents=True, exist_ok=True)

        mapper = self._relation_mapper(left, right)
        condition_sqls = [
            compile_expr(cond, self.dialect, self.catalog, self.query, mapper)
            for cond in node.conditions
        ]

        def task(partition: int) -> TaskResult:
            scan_for_fault(
                left_sources + right_sources,
                stage_id,
                partition,
                self.fault,
                self.memory,
                self.config.batch_rows,
            )
            left_out = left_repart_dir / f"p{partition:04d}.parquet"
            right_out = right_repart_dir / f"p{partition:04d}.parquet"
            join_out = self.workspace.stage_dir / stage_id / f"p{partition:04d}.parquet"
            join_out.parent.mkdir(parents=True, exist_ok=True)
            with duck_session(self.config, self.workspace) as con:
                self._write_hash_partition(
                    con, left_sources, left_keys, partitions, partition, left_out
                )
                self._write_hash_partition(
                    con, right_sources, right_keys, partitions, partition, right_out
                )
                left_rows = parquet_rows(left_out)
                right_rows = parquet_rows(right_out)
                join_query = (
                    f"SELECT * FROM {_read_parquet_sql([left_out])} AS l "
                    f"JOIN {_read_parquet_sql([right_out])} AS r ON "
                    + " AND ".join(condition_sqls)
                    + " ORDER BY ALL"
                )
                con.execute("CREATE TEMP TABLE joined AS " + join_query)
                joined_rows = int(
                    con.execute("SELECT count(*) FROM joined").fetchone()[0]
                )
                tmp = join_out.with_name(join_out.name + ".tmp")
                if tmp.exists():
                    tmp.unlink()
                con.execute(_copy_sql("SELECT * FROM joined", tmp))
                self._check_memory_relation(tmp)
            os.replace(tmp, join_out)
            self.temp.check()
            metrics = [
                self._metric(
                    stage_id, partition, "repartition_left", left_total, left_rows
                ),
                self._metric(
                    stage_id, partition, "repartition_right", right_total, right_rows
                ),
                self._metric(
                    stage_id,
                    partition,
                    "hash_join",
                    left_rows + right_rows,
                    joined_rows,
                ),
            ]
            return TaskResult((join_out,), joined_rows, metrics)

        outcome = self.scheduler.run_stage(stage_id, partitions, task)
        self.repartition_operations += 2
        self.repartition_partitions += partitions * 2
        files = tuple(path for result in outcome.partitions for path in result.output_paths)
        actual = sum(result.rows for result in outcome.partitions)
        self.actual_rows[node.node_id] = actual
        self.metrics.extend(
            metric for result in outcome.partitions for metric in result.metrics
        )
        return Relation(
            name=stage_id,
            files=files,
            aliases=left.aliases | right.aliases,
            column_map={**left.column_map, **right.column_map},
            phys_columns=left.phys_columns + right.phys_columns,
            tables={**left.tables, **right.tables},
            partial=None,
        )

    def _join_keys(
        self, node: JoinPlan, left: Relation, right: Relation
    ) -> tuple[list[str], list[str]]:
        left_keys: list[str] = []
        right_keys: list[str] = []
        for condition in node.conditions:
            if not isinstance(condition, BinaryOp) or condition.op != "=":
                continue
            if not isinstance(condition.left, ColumnRef) or not isinstance(
                condition.right, ColumnRef
            ):
                continue
            lhs = condition.left
            rhs = condition.right
            if not lhs.table or not rhs.table:
                continue
            if lhs.table in left.aliases and rhs.table in right.aliases:
                left_keys.append(left.physical(lhs.table, lhs.name))
                right_keys.append(right.physical(rhs.table, rhs.name))
            elif rhs.table in left.aliases and lhs.table in right.aliases:
                left_keys.append(left.physical(rhs.table, rhs.name))
                right_keys.append(right.physical(lhs.table, lhs.name))
        if not left_keys or not right_keys:
            raise ExecutorError(ERROR_SQL, "hash join requires an equi-join key")
        return list(dict.fromkeys(left_keys)), list(dict.fromkeys(right_keys))

    def _relation_mapper(self, *relations: Relation):
        maps = {}
        for relation in relations:
            maps.update(relation.column_map)

        def mapper(alias: str, column: str) -> str:
            key = (alias.lower(), column.lower())
            if key not in maps:
                raise ExecutorError(
                    ERROR_SQL, f"column {alias}.{column} missing from join input"
                )
            return quote_ident(maps[key])

        return mapper

    def _write_hash_partition(
        self,
        con: duckdb.DuckDBPyConnection,
        sources: list[Path],
        keys: list[str],
        partition_count: int,
        partition: int,
        target: Path,
    ) -> None:
        if not sources:
            raise ExecutorError(ERROR_IO, "hash repartition has no input files")
        tmp = target.with_name(target.name + ".tmp")
        if tmp.exists():
            tmp.unlink()
        if partition_count == 1:
            query = f"SELECT * FROM {_read_parquet_sql(sources)} ORDER BY ALL"
        elif keys:
            hash_args = ", ".join(quote_ident(key) for key in keys)
            query = (
                f"SELECT * FROM {_read_parquet_sql(sources)} "
                f"WHERE (hash({hash_args}) % {partition_count}) = {partition} "
                "ORDER BY ALL"
            )
        else:
            query = (
                f"SELECT * FROM {_read_parquet_sql(sources)} "
                + ("WHERE TRUE " if partition == 0 else "WHERE FALSE ")
                + "ORDER BY ALL"
            )
        con.execute(_copy_sql(query, tmp))
        self._check_memory_relation(tmp)
        os.replace(tmp, target)

    # ------------------------------------------------------------------
    # Final aggregate, spill, and deterministic output
    # ------------------------------------------------------------------
    def _final_aggregate(self, relation: Relation) -> tuple[Relation, list[Path]]:
        stage_id = "final_aggregate"
        output_names = _unique_output_names(self.query)
        group_exprs = [self._resolve_group_expr(expr) for expr in self.query.group_by]
        group_sqls = [
            self._final_expr_sql(expr, relation) for expr in group_exprs
        ]
        select_sqls: list[str] = []
        for name, item in zip(output_names, self.query.select):
            body = self._final_expr_sql(item.expr, relation)
            select_sqls.append(f"{body} AS {quote_ident(name)}")
        having_sql = ""
        if self.query.having is not None:
            having_sql = " HAVING " + self._final_expr_sql(
                self.query.having, relation
            )
        source_total = relation.rows
        partitions = self.config.partitions if group_sqls else 1
        source_sql = _read_parquet_sql(relation.files)

        def task(partition: int) -> TaskResult:
            scan_for_fault(
                list(relation.files),
                stage_id,
                partition,
                self.fault,
                self.memory,
                self.config.batch_rows,
            )
            where_sql = ""
            if group_sqls and partitions > 1:
                where_sql = (
                    " WHERE (hash("
                    + ", ".join(group_sqls)
                    + f") % {partitions}) = {partition}"
                )
            partition_input = source_total
            if where_sql:
                with duck_session(self.config, self.workspace) as con:
                    partition_input = int(
                        con.execute(
                            f"SELECT count(*) FROM {source_sql}{where_sql}"
                        ).fetchone()[0]
                    )
            query = (
                "SELECT "
                + ("DISTINCT " if self.query.distinct else "")
                + ", ".join(select_sqls)
                + f" FROM {source_sql}"
                + where_sql
            )
            if group_sqls:
                query += " GROUP BY " + ", ".join(group_sqls)
            query += having_sql
            query += " ORDER BY ALL"
            out_path = self.workspace.stage_dir / stage_id / f"p{partition:04d}.parquet"
            out_path.parent.mkdir(parents=True, exist_ok=True)
            tmp = out_path.with_name(out_path.name + ".tmp")
            if tmp.exists():
                tmp.unlink()
            with duck_session(self.config, self.workspace) as con:
                con.execute("CREATE TEMP TABLE agg AS " + query)
                output_rows = int(
                    con.execute("SELECT count(*) FROM agg").fetchone()[0]
                )
                con.execute(_copy_sql("SELECT * FROM agg", tmp))
            os.replace(tmp, out_path)
            self.temp.check()
            if (
                self.config.force_spill
                or out_path.stat().st_size > self.config.agg_memory_threshold_bytes
            ):
                spill_path = (
                    self.workspace.spill_dir / "final_aggregate" / out_path.name
                )
                spill_path.parent.mkdir(parents=True, exist_ok=True)
                os.replace(out_path, spill_path)
                out_path = spill_path
                self.workspace.record_spill_bytes(out_path.stat().st_size)
            return TaskResult(
                (out_path,),
                output_rows,
                [
                    self._metric(
                        stage_id,
                        partition,
                        "final_aggregate",
                        partition_input,
                        output_rows,
                    )
                ],
            )

        outcome = self.scheduler.run_stage(stage_id, partitions, task)
        if group_sqls:
            self.repartition_operations += 1
            self.repartition_partitions += partitions
        files = tuple(path for result in outcome.partitions for path in result.output_paths)
        actual = sum(result.rows for result in outcome.partitions)
        self.actual_rows["final_aggregate"] = actual
        self.metrics.extend(
            metric for result in outcome.partitions for metric in result.metrics
        )
        return (
            Relation(
                name="final",
                files=files,
                aliases=relation.aliases,
                column_map=relation.column_map,
                phys_columns=tuple(output_names),
                tables=relation.tables,
                partial=None,
            ),
            files,
        )

    def _resolve_group_expr(self, expr: Expr) -> Expr:
        if isinstance(expr, ColumnRef) and expr.table is None:
            for item in self.query.select:
                if (
                    item.alias
                    and item.alias.lower() == expr.name.lower()
                ):
                    return item.expr
        return expr

    def _final_expr_sql(self, expr: Expr, relation: Relation) -> str:
        partial = relation.partial or self.planning.partial
        if isinstance(expr, FuncCall) and expr.name.upper() in AGGREGATE_NAMES:
            if partial is None:
                return compile_expr(
                    expr,
                    self.dialect,
                    self.catalog,
                    self.query,
                    self._relation_mapper(relation),
                )
            idx = partial.aggregate_index.get(expr)
            if idx is None:
                raise ExecutorError(
                    ERROR_SQL, f"aggregate {expr.name} was not prepared for pushdown"
                )
            name = expr.name.upper()
            if name == "SUM":
                body = f"SUM({quote_ident(_partial_state_name(idx, 'sum'))})"
                if expr.args and expr_category(
                    expr.args[0], self.dialect, self.catalog, self.query
                ) == "integer":
                    body = f"CAST({body} AS BIGINT)"
                return body
            if name == "COUNT":
                return (
                    "COALESCE(CAST(SUM("
                    + quote_ident(_partial_state_name(idx, "count"))
                    + ") AS BIGINT), 0)"
                )
            if name == "AVG":
                return (
                    "(CAST(SUM("
                    + quote_ident(_partial_state_name(idx, "sum"))
                    + ") AS DOUBLE) / NULLIF(SUM("
                    + quote_ident(_partial_state_name(idx, "count"))
                    + "), 0))"
                )
            if name in {"MIN", "MAX"}:
                return (
                    f"{name}("
                    + quote_ident(_partial_state_name(idx, name.lower()))
                    + ")"
                )
            raise ExecutorError(ERROR_SQL, f"unsupported aggregate {name}")
        if partial and expr in partial.fact_group_exprs:
            idx = partial.group_index[expr]
            return quote_ident(_partial_group_name(idx))
        if isinstance(expr, Literal):
            return literal_sql(expr.value, expr.kind)
        if isinstance(expr, ColumnRef):
            alias, info = self.catalog.resolve_column(
                self.query, expr, set(relation.aliases)
            )
            return quote_ident(relation.physical(alias, info.name))
        if isinstance(expr, Star):
            return "*"
        if isinstance(expr, UnaryOp):
            inner = self._final_expr_sql(expr.operand, relation)
            if expr.op.upper() == "NOT":
                return f"(NOT {inner})"
            return f"({expr.op}{inner})"
        if isinstance(expr, BinaryOp):
            left = self._final_expr_sql(expr.left, relation)
            right = self._final_expr_sql(expr.right, relation)
            op = expr.op.upper()
            if op == "DIV":
                return _int_div_sql(left, right)
            if op in {"MOD", "%"}:
                return _mod_sql(left, right)
            if op == "/":
                category = expr_category(expr, self.dialect, self.catalog, self.query)
                if self.dialect.name == "postgres" and category == "integer":
                    return _int_div_sql(left, right)
                return f"(({left}) / ({right}))"
            if op == "AND":
                return f"(({left}) AND ({right}))"
            if op == "OR":
                return f"(({left}) OR ({right}))"
            return f"(({left}) {op} ({right}))"
        if isinstance(expr, IsNull):
            inner = self._final_expr_sql(expr.expr, relation)
            return f"(({inner}) IS {'NOT ' if expr.negated else ''}NULL)"
        if isinstance(expr, Between):
            inner = self._final_expr_sql(expr.expr, relation)
            low = self._final_expr_sql(expr.low, relation)
            high = self._final_expr_sql(expr.high, relation)
            return f"(({inner}) {'NOT ' if expr.negated else ''}BETWEEN ({low}) AND ({high}))"
        if isinstance(expr, Cast):
            inner = self._final_expr_sql(expr.expr, relation)
            return f"CAST(({inner}) AS {expr.target})"
        if isinstance(expr, FuncCall):
            name = expr.name.upper()
            args = [self._final_expr_sql(arg, relation) for arg in expr.args]
            if name == "DATE":
                return f"CAST(({args[0]}) AS DATE)"
            if name == "DATE_FORMAT":
                fn = "strftime" if self.dialect.name == "mysql" else "to_char"
                return f"{fn}(({args[0]}), ({args[1]}))"
            if name == "DATE_TRUNC":
                if self.dialect.name == "mysql":
                    return f"(CASE WHEN lower(CAST({args[0]} AS VARCHAR)) = 'day' THEN CAST(({args[1]}) AS DATE) ELSE date_trunc(({args[0]}), ({args[1]})) END)"
                return f"date_trunc(({args[0]}), ({args[1]}))"
            if name == "MOD":
                return _mod_sql(args[0], args[1])
            distinct = "DISTINCT " if expr.distinct else ""
            return f"{name}({distinct}{', '.join(args)})"
        raise ExecutorError(ERROR_SQL, f"cannot compile final expression {expr!r}")

    def _global_sort(
        self, relation: Relation, output_names: list[str], target: Path
    ) -> int:
        order_sqls: list[str] = []
        used: set[str] = set()
        output_aliases = {name.lower() for name in output_names}
        alias_lookup = {name.lower(): name for name in output_names}
        for item in self.query.order_by:
            out_name: str | None = None
            if isinstance(item.expr, Literal) and item.expr.kind == "int":
                ordinal = int(item.expr.value)
                if 1 <= ordinal <= len(output_names):
                    out_name = output_names[ordinal - 1]
            if isinstance(item.expr, ColumnRef) and item.expr.table is None:
                out_name = out_name or alias_lookup.get(item.expr.name.lower())
            if out_name is None:
                for name, select_item in zip(output_names, self.query.select):
                    if select_item.expr == item.expr:
                        out_name = name
                        break
            if out_name is None:
                raise ExecutorError(
                    ERROR_SQL,
                    "ORDER BY must reference an output alias or a selected expression",
                )
            order_sqls.append(
                compile_order_item(
                    OrderItem(ColumnRef(None, out_name), item.direction, item.nulls),
                    self.dialect,
                    self.catalog,
                    self.query,
                    output_aliases,
                )
            )
            used.add(out_name.lower())
        # Stable tie-break: every remaining output column, using dialect defaults.
        for name in output_names:
            if name.lower() in used:
                continue
            order_sqls.append(
                compile_order_item(
                    OrderItem(ColumnRef(None, name), "ASC", None),
                    self.dialect,
                    self.catalog,
                    self.query,
                    output_aliases,
                )
            )
        source = _read_parquet_sql(relation.files)
        query = f"SELECT * FROM {source}"
        if order_sqls:
            query += " ORDER BY " + ", ".join(order_sqls)
        if self.query.limit is not None:
            query += f" LIMIT {int(self.query.limit)}"
        with duck_session(self.config, self.workspace) as con:
            count = int(con.execute(f"SELECT count(*) FROM ({query})").fetchone()[0])
            _atomic_copy_to_csv_or_parquet(
                con, query, target, self.config.result_format
            )
        self.metrics.append(
            self._metric(
                "output",
                0,
                "sort",
                sum(result.rows for result in []) if False else relation.rows,
                count,
            )
        )
        return count

    # ------------------------------------------------------------------
    # Reporting helpers
    # ------------------------------------------------------------------
    def _metric(
        self,
        stage: str,
        partition: int,
        operator: str,
        input_rows: int,
        output_rows: int,
    ) -> dict[str, Any]:
        return {
            "stage": stage,
            "partition": partition,
            "operator": operator,
            "input_rows": int(input_rows),
            "output_rows": int(output_rows),
        }

