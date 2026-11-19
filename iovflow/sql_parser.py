"""Parse the supported streaming SQL subset into a queryable logical plan.

DuckDB supplies the SQL grammar/AST.  This module then enforces a deliberately
narrow three-stage shape:

    filter -> group-by-vehicle -> event-time window aggregate

Anything that would require a join, subquery, CTE, set operation, window
function, HAVING, or a second grouping dimension is rejected with a stable
business code.  There is no silent serial fallback.
"""

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass, field
from typing import Any, Callable

from .errors import EngineError
from .expr import ExprEvaluator, interval_seconds, validate_window_ast
from .util import parse_time

try:  # DuckDB is an explicit runtime dependency for SQL parsing.
    import duckdb
except Exception as _exc:  # pragma: no cover - exercised only on bad installs
    duckdb = None  # type: ignore[assignment]
    _DUCKDB_ERROR = _exc
else:
    _DUCKDB_ERROR = None


AGGREGATE_FUNCTIONS = {"count_star", "count", "sum", "min", "max", "avg"}
WINDOW_FUNCTIONS = {"time_bucket", "window_start", "tumble"}
WINDOW_OVER_TYPES = {
    "WINDOW",
    "WINDOW_NODE",
    "WINDOW_EXPRESSION",
    "WINDOW_SPEC",
}


@dataclass
class AggregateSpec:
    alias: str
    func: str
    column: str | None = None
    distinct: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "alias": self.alias,
            "func": self.func,
            "column": self.column,
            "distinct": self.distinct,
        }


@dataclass
class WindowSpec:
    alias: str
    size_seconds: int
    expr_ast: dict[str, Any]
    expr_sql: str

    def start_for(self, row: dict[str, Any]) -> int:
        evaluator = ExprEvaluator(self.expr_ast)
        value = evaluator.eval(row)
        if value is None:
            raise EngineError("IOV_INPUT_INVALID", "event_time is null")
        return int(parse_time(value))

    def to_dict(self) -> dict[str, Any]:
        return {
            "alias": self.alias,
            "size_seconds": self.size_seconds,
            "expr_ast": self.expr_ast,
            "expr_sql": self.expr_sql,
        }


@dataclass
class LogicalPlan:
    sql: str
    sql_hash: str
    vehicle_column: str
    window: WindowSpec
    filter_ast: dict[str, Any] | None
    filter_sql: str
    aggregates: list[AggregateSpec]
    source_table: str
    stages: list[dict[str, Any]] = field(default_factory=list)

    @property
    def window_column(self) -> str:
        return self.window.alias

    def filter_evaluator(self) -> ExprEvaluator:
        return ExprEvaluator(self.filter_ast, self.sql)

    def window_evaluator(self) -> ExprEvaluator:
        return ExprEvaluator(self.window.expr_ast, self.sql)

    def to_dict(self) -> dict[str, Any]:
        return {
            "sql": self.sql,
            "sql_hash": self.sql_hash,
            "vehicle_column": self.vehicle_column,
            "window": self.window.to_dict(),
            "filter_ast": self.filter_ast,
            "filter_sql": self.filter_sql,
            "aggregates": [a.to_dict() for a in self.aggregates],
            "source_table": self.source_table,
            "stages": self.stages,
            "output_columns": ["vehicle_id", "window_start", "window_end"]
            + [a.alias for a in self.aggregates],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "LogicalPlan":
        win = data["window"]
        return cls(
            sql=data["sql"],
            sql_hash=data["sql_hash"],
            vehicle_column=data["vehicle_column"],
            window=WindowSpec(
                alias=win["alias"],
                size_seconds=int(win["size_seconds"]),
                expr_ast=win["expr_ast"],
                expr_sql=win["expr_sql"],
            ),
            filter_ast=data.get("filter_ast"),
            filter_sql=data.get("filter_sql", ""),
            aggregates=[AggregateSpec(**a) for a in data.get("aggregates", [])],
            source_table=data.get("source_table", "events"),
            stages=data.get("stages", []),
        )

    def default_stages(self, parallelism: int, max_shards: int = 4) -> list[dict[str, Any]]:
        """Return the default DAG metadata.  The runtime replaces window
        parallelism with the actual logical shard count."""
        return [
            {
                "stage_id": "filter",
                "stage_type": "filter",
                "ordinal": 0,
                "parallelism": parallelism,
                "max_concurrent": parallelism,
                "input_from": "source",
                "output_to": "group_by_vehicle",
            },
            {
                "stage_id": "group_by_vehicle",
                "stage_type": "group_by_vehicle",
                "ordinal": 1,
                "parallelism": parallelism,
                "max_concurrent": parallelism,
                "input_from": "filter",
                "output_to": "window_aggregate",
            },
            {
                "stage_id": "window_aggregate",
                "stage_type": "window_aggregate",
                "ordinal": 2,
                "parallelism": max(parallelism, max_shards),
                "max_concurrent": parallelism,
                "input_from": "group_by_vehicle",
                "output_to": "merge_skew",
            },
            {
                "stage_id": "merge_skew",
                "stage_type": "merge_skew",
                "ordinal": 3,
                "parallelism": 1,
                "max_concurrent": 1,
                "input_from": "window_aggregate",
                "output_to": "sink",
            },
            {
                "stage_id": "sink",
                "stage_type": "sink",
                "ordinal": 4,
                "parallelism": 1,
                "max_concurrent": 1,
                "input_from": "merge_skew",
                "output_to": "result_db",
            },
        ]


def parse_sql(sql: str, vehicle_column: str = "vehicle_id") -> LogicalPlan:
    if duckdb is None:
        raise EngineError(
            "IOV_DEPENDENCY_MISSING",
            f"duckdb is required for SQL parsing: {_DUCKDB_ERROR}",
        )
    if not sql or not sql.strip():
        raise EngineError("IOV_SQL_PARSE_ERROR", "SQL is empty")
    try:
        con = duckdb.connect()
        raw = con.execute("select json_serialize_sql(?)", [sql]).fetchone()[0]
        con.close()
    except Exception as exc:
        raise EngineError("IOV_SQL_PARSE_ERROR", f"SQL parse failed: {exc}") from exc
    import json

    try:
        payload = json.loads(raw)
    except Exception as exc:
        raise EngineError("IOV_SQL_PARSE_ERROR", f"SQL parser returned invalid JSON: {exc}") from exc
    if payload.get("error"):
        raise EngineError(
            "IOV_SQL_PARSE_ERROR",
            str(payload.get("error_message") or payload.get("error")),
        )
    statements = payload.get("statements") or []
    if len(statements) != 1:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_STRUCTURE",
            f"exactly one SELECT statement is required, got {len(statements)}",
            fragment=sql[:300],
        )
    node = statements[0].get("node") or {}
    if node.get("type") == "SET_OPERATION_NODE" or "setop_type" in node:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_SET_OPERATION",
            f"{node.get('setop_type', 'set')} operations are not supported",
            fragment=sql[:300],
        )
    if node.get("type") != "SELECT_NODE":
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_STRUCTURE",
            f"only SELECT statements are supported, got {node.get('type')}",
            fragment=sql[:300],
        )
    _reject_unsupported_query(node, sql)
    return _compile_select(node, sql, vehicle_column)


def _compile_select(node: dict[str, Any], sql: str, vehicle_column: str) -> LogicalPlan:
    select_list = node.get("select_list") or []
    group_exprs = node.get("group_expressions") or []
    group_sets = node.get("group_sets") or []
    if not select_list:
        raise EngineError("IOV_SQL_UNSUPPORTED_STRUCTURE", "SELECT list is empty", fragment=sql)
    if not group_exprs:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_GROUP",
            "a GROUP BY vehicle and event-time window is required",
            fragment=sql[:300],
        )
    if not group_sets or any(len(gs) != len(group_exprs) for gs in group_sets) or len(group_sets) != 1:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_GROUP",
            "GROUPING SETS/ROLLUP/CUBE are not supported",
            fragment=sql[:300],
        )

    aliases: dict[str, dict[str, Any]] = {}
    for item in select_list:
        alias = str(item.get("alias") or "")
        if alias:
            if alias in aliases:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_STRUCTURE",
                    f"duplicate select alias {alias!r}",
                    fragment=_fragment(item, sql),
                )
            aliases[alias] = item

    resolved_groups: list[dict[str, Any]] = []
    for group in group_exprs:
        if group.get("class") == "COLUMN_REF":
            names = group.get("column_names") or []
            name = names[-1] if names else ""
            if name in aliases:
                resolved_groups.append(aliases[name])
                continue
        resolved_groups.append(group)

    vehicle_groups: list[dict[str, Any]] = []
    window_groups: list[dict[str, Any]] = []
    for group in resolved_groups:
        if _is_window_expr(group):
            window_groups.append(group)
        elif _is_column_ref(group):
            vehicle_groups.append(group)
        else:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_GROUP",
                "only a vehicle column and an event-time window expression may be grouped",
                fragment=_fragment(group, sql),
            )
    if len(vehicle_groups) != 1:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_GROUP",
            f"expected exactly one vehicle grouping key, got {len(vehicle_groups)}",
            fragment=sql[:300],
        )
    if len(window_groups) != 1:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_WINDOW",
            f"expected exactly one event-time window grouping expression, got {len(window_groups)}",
            fragment=sql[:300],
        )
    vehicle_node = vehicle_groups[0]
    window_node = window_groups[0]
    vehicle_name = (vehicle_node.get("column_names") or [""])[-1]
    if not vehicle_name:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_GROUP",
            "vehicle grouping key is not a plain column",
            fragment=_fragment(vehicle_node, sql),
        )
    # The configured name is the preferred vehicle key.  If the SQL uses a
    # different plain column we accept it and expose it canonically as
    # vehicle_id in the result schema.
    if vehicle_column and vehicle_name != vehicle_column:
        # Still accept vehicle_id/vin/vehicle as synonyms; reject an arbitrary
        # second grouping dimension elsewhere.
        if vehicle_name.lower() not in ("vehicle_id", "vehicle", "vin", "device_id"):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_GROUP",
                f"grouping column {vehicle_name!r} is not a vehicle identifier",
                fragment=_fragment(vehicle_node, sql),
            )

    _scan_unsupported(window_node, sql, in_window=True)
    validate_window_ast(window_node, sql)
    window_expr_sql = _fragment(window_node, sql)
    window_alias = str(window_node.get("alias") or "window_start")
    size = _window_size(window_node, sql)
    if window_alias == vehicle_name:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_STRUCTURE",
            "window alias collides with the vehicle grouping column",
            fragment=window_expr_sql,
        )

    aggregates: list[AggregateSpec] = []
    for item in select_list:
        if _is_aggregate(item):
            aggregates.append(_aggregate_spec(item, sql))
        elif _is_window_expr(item):
            continue
        elif _is_column_ref(item):
            name = (item.get("column_names") or [""])[-1]
            if name != vehicle_name:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_GROUP",
                    f"selected column {name!r} is neither the vehicle key nor an aggregate",
                    fragment=_fragment(item, sql),
                )
        else:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_STRUCTURE",
                "SELECT items must be the vehicle key, the window expression, or aggregates",
                fragment=_fragment(item, sql),
            )
    if not aggregates:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_AGGREGATE",
            "at least one aggregate is required",
            fragment=sql,
        )
    seen_aliases: set[str] = set()
    for agg in aggregates:
        if agg.alias in ("vehicle_id", "window_start", "window_end", window_alias, vehicle_name):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_STRUCTURE",
                f"aggregate alias {agg.alias!r} collides with a canonical result column",
                fragment=sql,
            )
        if agg.alias in seen_aliases:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_STRUCTURE",
                f"duplicate aggregate alias {agg.alias!r}",
                fragment=sql,
            )
        seen_aliases.add(agg.alias)

    where = node.get("where_clause")
    if where is not None:
        _scan_unsupported(where, sql, in_window=False)
        ExprEvaluator(where, sql)

    sql_hash = hashlib.sha256(sql.encode("utf-8")).hexdigest()
    return LogicalPlan(
        sql=sql,
        sql_hash=sql_hash,
        vehicle_column=vehicle_name,
        window=WindowSpec(
            alias=window_alias,
            size_seconds=size,
            expr_ast=window_node,
            expr_sql=window_expr_sql,
        ),
        filter_ast=where,
        filter_sql=_fragment(where, sql) if where else "",
        aggregates=aggregates,
        source_table=str((node.get("from_table") or {}).get("table_name") or "events"),
    )


def _reject_unsupported_query(node: dict[str, Any], sql: str) -> None:
    cte_map = (node.get("cte_map") or {}).get("map") or []
    if cte_map:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_CTE",
            "CTEs are not supported by the three-stage planner",
            fragment=sql[:300],
        )
    for modifier in node.get("modifiers") or []:
        mtype = str(modifier.get("type") or modifier)
        if "DISTINCT" in mtype:
            code = "IOV_SQL_UNSUPPORTED_GROUP"
        elif "ORDER" in mtype or "LIMIT" in mtype or "SAMPLE" in mtype:
            code = "IOV_SQL_UNSUPPORTED_STRUCTURE"
        else:
            code = "IOV_SQL_UNSUPPORTED_STRUCTURE"
        raise EngineError(
            code,
            f"SELECT modifier {mtype!r} is not supported by the stage plan",
            fragment=_fragment(modifier, sql),
        )
    for key in ("having", "qualify", "sample"):
        if node.get(key) is not None:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_STRUCTURE",
                f"{key.upper()} is not part of the supported stage set",
                fragment=_fragment(node.get(key), sql),
            )
    if node.get("aggregate_handling", "STANDARD_HANDLING") != "STANDARD_HANDLING":
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_AGGREGATE",
            "GROUPING SETS style aggregate handling is not supported",
            fragment=_fragment(node, sql),
        )
    from_table = node.get("from_table")
    if not from_table:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_STRUCTURE",
            "a FROM table is required",
            fragment=sql[:300],
        )
    _check_from_table(from_table, sql)
    where = node.get("where_clause")
    if where:
        _scan_unsupported(where, sql, in_window=False)
    # Also inspect the select list and grouping expressions so a SQL window
    # function (OVER), subquery, or other cross-row construct is rejected at
    # plan time rather than failing later inside a worker.
    _scan_unsupported(node, sql, in_window=False)


def _check_from_table(table: dict[str, Any], sql: str) -> None:
    ttype = str(table.get("type") or "")
    if "SUBQUERY" in ttype:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_SUBQUERY",
            "subquery FROM sources are not supported",
            fragment=_fragment(table, sql),
        )
    if "JOIN" in ttype or ttype == "JOIN":
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_JOIN",
            "JOIN sources are not supported; the plan is a single event stream",
            fragment=_fragment(table, sql),
        )
    if ttype != "BASE_TABLE":
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_STRUCTURE",
            f"unsupported FROM source type {ttype!r}",
            fragment=_fragment(table, sql),
        )
    for key in ("sample", "at_clause"):
        if table.get(key) is not None:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_STRUCTURE",
                f"FROM modifier {key} is not supported",
                fragment=_fragment(table, sql),
            )


def _scan_unsupported(node: Any, sql: str, in_window: bool) -> None:
    if not isinstance(node, dict):
        return
    cls = str(node.get("class") or "")
    typ = str(node.get("type") or "")
    if cls in ("SUBQUERY", "SUBQUERY_NODE"):
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_SUBQUERY",
            "subqueries are not supported by the three-stage planner",
            fragment=_fragment(node, sql),
        )
    if cls == "SET_OPERATION" or typ == "SET_OPERATION_NODE" or "SET_OPERATION" in typ:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_SET_OPERATION",
            "UNION/INTERSECT/EXCEPT are not supported",
            fragment=_fragment(node, sql),
        )
    if cls in WINDOW_OVER_TYPES or node.get("window") not in (None, {}):
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_WINDOW",
            "SQL window functions (OVER) are not supported; use an event-time window aggregate",
            fragment=_fragment(node, sql),
        )
    if cls == "FUNCTION":
        fn = str(node.get("function_name", "")).lower()
        if fn in ("row_number", "rank", "dense_rank", "lag", "lead", "ntile", "first_value", "last_value"):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_WINDOW",
                f"window function {fn!r} is not supported",
                fragment=_fragment(node, sql),
            )
        if fn in AGGREGATE_FUNCTIONS and in_window:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_WINDOW",
                "aggregate functions cannot appear inside the window grouping expression",
                fragment=_fragment(node, sql),
            )
    for key, value in node.items():
        if key in ("query_location", "query_location_length", "alias"):
            continue
        if isinstance(value, dict):
            _scan_unsupported(value, sql, in_window)
        elif isinstance(value, list):
            for item in value:
                if isinstance(item, dict):
                    _scan_unsupported(item, sql, in_window)


def _is_column_ref(node: dict[str, Any]) -> bool:
    return node.get("class") == "COLUMN_REF"


def _is_window_expr(node: dict[str, Any]) -> bool:
    if node.get("class") != "FUNCTION":
        return False
    fn = str(node.get("function_name", "")).lower()
    if fn in WINDOW_FUNCTIONS:
        return True
    # Also accept a scalar expression that clearly uses epoch/event_time and
    # arithmetic, e.g. to_timestamp(floor(epoch(event_time)/300)*300).
    text = str(node.get("_text") or "")
    if "event_time" in text and any(x in text for x in ("floor", "date_trunc", "time_bucket")):
        return True
    defs = _collect_function_names(node)
    return bool({"epoch", "date_trunc"} & defs) and _references_event_time(node)


def _is_aggregate(node: dict[str, Any]) -> bool:
    return node.get("class") == "FUNCTION" and str(
        node.get("function_name", "")
    ).lower() in AGGREGATE_FUNCTIONS


def _aggregate_spec(node: dict[str, Any], sql: str) -> AggregateSpec:
    fn = str(node.get("function_name", "")).lower()
    args = node.get("arguments") or []
    alias = str(node.get("alias") or "")
    column: str | None = None
    if fn != "count_star":
        if len(args) != 1:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_AGGREGATE",
                f"{fn} expects exactly one column argument",
                fragment=_fragment(node, sql),
            )
        expr = args[0].get("expression") or {}
        if fn == "count" and not node.get("distinct") and expr.get("class") == "CONSTANT":
            lit = expr.get("literal") or {}
            if str(lit.get("text")) not in ("0", "NULL", "None"):
                fn = "count_star"
                column = None
                args = []
        if fn == "count_star":
            pass
        elif not _is_column_ref(expr):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_AGGREGATE",
                f"{fn} over an expression is not supported; aggregate a plain column",
                fragment=_fragment(expr, sql),
            )
        else:
            column = (expr.get("column_names") or [""])[-1]
    if node.get("filter") is not None:
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_AGGREGATE",
            "aggregate FILTER clauses are not supported",
            fragment=_fragment(node, sql),
        )
    if node.get("order_bys", {}).get("orders"):
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_AGGREGATE",
            "ordered aggregates are not supported",
            fragment=_fragment(node, sql),
        )
    distinct = bool(node.get("distinct"))
    if distinct and fn not in ("count", "sum", "avg"):
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_AGGREGATE",
            f"DISTINCT {fn} is not supported",
            fragment=_fragment(node, sql),
        )
    if not alias:
        if fn == "count_star":
            alias = "count"
        else:
            alias = f"{fn}_{column}"
    return AggregateSpec(alias=alias, func=fn, column=column, distinct=distinct)


def _window_size(node: dict[str, Any], sql: str) -> int:
    fn = str(node.get("function_name", "")).lower()
    args = [a.get("expression") or {} for a in node.get("arguments", [])]
    if fn in WINDOW_FUNCTIONS:
        candidates: list[Any] = []
        for arg in args:
            if arg.get("class") == "CAST":
                type_name = str(arg.get("type_expr", {}).get("type_name", "")).upper()
                if type_name == "INTERVAL":
                    candidates.append(_constant_value(arg.get("child")))
            elif arg.get("class") == "CONSTANT":
                candidates.append(_constant_value(arg))
        for value in candidates:
            try:
                seconds = interval_seconds(value)
            except EngineError:
                continue
            if seconds > 0 and seconds <= 7 * 86400:
                # Rediscover interval text: "300" means 300 seconds.
                return seconds
    text = _fragment(node, sql)
    m = re.search(r"/\s*(\d+)", text)
    if m and int(m.group(1)) > 0:
        return int(m.group(1))
    m = re.search(r"(\d+)\s*(?:seconds?|secs?|s)\b", text, re.IGNORECASE)
    if m:
        return int(m.group(1))
    raise EngineError(
        "IOV_SQL_UNSUPPORTED_WINDOW",
        "cannot determine the event-time window size from the SQL expression",
        fragment=text,
    )


def _constant_value(node: Any) -> Any:
    if not isinstance(node, dict):
        return None
    if node.get("class") == "CONSTANT":
        lit = node.get("literal") or {}
        text = lit.get("text")
        try:
            return int(text) if str(lit.get("kind", "")).upper() in (
                "INTEGER",
                "BIGINT",
                "HUGEINT",
            ) else text
        except (TypeError, ValueError):
            return text
    return None


def _collect_function_names(node: Any) -> set[str]:
    out: set[str] = set()
    if isinstance(node, dict):
        if node.get("class") == "FUNCTION":
            out.add(str(node.get("function_name", "")).lower())
        for value in node.values():
            out |= _collect_function_names(value)
    elif isinstance(node, list):
        for item in node:
            out |= _collect_function_names(item)
    return out


def _references_event_time(node: Any) -> bool:
    if isinstance(node, dict):
        if node.get("class") == "COLUMN_REF":
            cols = node.get("column_names") or []
            return bool(cols) and str(cols[-1]).lower() in ("event_time", "ts", "timestamp")
        return any(_references_event_time(v) for v in node.values())
    if isinstance(node, list):
        return any(_references_event_time(v) for v in node)
    return False


def _fragment(node: Any, sql: str) -> str:
    if not isinstance(node, dict):
        return str(node)[:300]
    try:
        loc = int(node.get("query_location", -1))
        length = int(node.get("query_location_length", 0))
        if 0 <= loc < len(sql):
            end = loc + length if length > 0 else min(len(sql), loc + 120)
            return sql[loc:end].strip()
    except Exception:
        pass
    return f"<{node.get('class', node.get('type', 'unknown'))}>"

