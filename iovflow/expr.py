"""A deliberately small, safe expression interpreter.

DuckDB is used only to turn SQL into a JSON AST.  Filtering and window
expressions are evaluated by this module so the same code runs in the batch and
stream paths.  Only deterministic scalar expressions are accepted.
"""

from __future__ import annotations

import math
import re
from datetime import datetime, timezone
from typing import Any, Callable

from .errors import EngineError
from .util import UTC, parse_time, ts_iso


_ALLOWED_CLASSES = {
    "COLUMN_REF",
    "CONSTANT",
    "COMPARISON",
    "CONJUNCTION",
    "BETWEEN",
    "OPERATOR",
    "FUNCTION",
    "CAST",
    "CASE",
}

_AGGREGATE_FUNCTIONS = {
    "count_star",
    "count",
    "sum",
    "min",
    "max",
    "avg",
}

_FUNCTIONS = {
    "lower",
    "upper",
    "trim",
    "ltrim",
    "rtrim",
    "length",
    "len",
    "abs",
    "floor",
    "ceil",
    "ceiling",
    "round",
    "coalesce",
    "nullif",
    "greatest",
    "least",
    "epoch",
    "to_timestamp",
    "strftime",
    "date_trunc",
    "substr",
    "substring",
    "concat",
    "time_bucket",
    "window_start",
    "tumble",
}

_NON_DETERMINISTIC = {
    "now",
    "current_timestamp",
    "random",
    "uuid",
    "today",
}


class IntervalValue:
    """Marker produced by CAST(... AS INTERVAL).

    Keeping the marker avoids confusing ``time_bucket(INTERVAL '5 minutes',
    ts)`` with the custom ``window_start(ts, 300)`` form once the interval has
    been converted to an integer number of seconds.
    """

    __slots__ = ("text",)

    def __init__(self, text: Any) -> None:
        self.text = text

    def seconds(self) -> int:
        return interval_seconds(self.text)

    def __repr__(self) -> str:
        return f"INTERVAL({self.text!r})"


def _literal(node: dict[str, Any]) -> Any:
    lit = node.get("literal", {})
    kind = str(lit.get("kind", "")).upper()
    text = lit.get("text")
    if kind in ("NULL", "VALUE_NULL"):
        return None
    if kind in ("TRUE", "BOOLEAN"):
        return str(text).lower() in ("true", "1")
    if kind in ("FALSE",):
        return False
    if kind in ("INTEGER", "BIGINT", "HUGEINT", "UBIGINT", "UINTEGER"):
        try:
            return int(text)
        except (TypeError, ValueError):
            return 0
    if kind in ("FLOAT", "DOUBLE", "DECIMAL", "REAL"):
        try:
            return float(text)
        except (TypeError, ValueError):
            return 0.0
    if kind == "STRING":
        return "" if text is None else str(text)
    # DuckDB versions differ in literal naming.  Fall back to a JSON-ish
    # conversion so normal constants continue to work.
    if text is None:
        return None
    if isinstance(text, (int, float, bool)):
        return text
    return str(text)


def interval_seconds(value: Any) -> int:
    if isinstance(value, IntervalValue):
        return value.seconds()
    if isinstance(value, (int, float)):
        return int(value)
    text = str(value).strip().lower()
    m = re.fullmatch(r"(-?\d+(?:\.\d+)?)\s*(second|seconds|sec|s)?", text)
    if m:
        return int(float(m.group(1)))
    m = re.fullmatch(r"(-?\d+(?:\.\d+)?)\s*(minute|minutes|min|m)", text)
    if m:
        return int(float(m.group(1)) * 60)
    m = re.fullmatch(r"(-?\d+(?:\.\d+)?)\s*(hour|hours|hr|h)", text)
    if m:
        return int(float(m.group(1)) * 3600)
    m = re.fullmatch(r"(-?\d+(?:\.\d+)?)\s*(day|days|d)", text)
    if m:
        return int(float(m.group(1)) * 86400)
    m = re.fullmatch(r"(-?\d+):(\d+)(?::(\d+))?", text)
    if m:
        h, mi, sec = int(m.group(1)), int(m.group(2)), int(m.group(3) or 0)
        return h * 3600 + mi * 60 + sec
    raise EngineError(
        "IOV_SQL_UNSUPPORTED_WINDOW",
        f"unsupported interval literal {value!r}",
        fragment=str(value),
    )


def _truth(value: Any) -> bool | None:
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        return value.lower() not in ("", "0", "false", "no", "null")
    return bool(value)


def _sql_like(value: Any, pattern: Any) -> bool:
    if value is None or pattern is None:
        return False
    regex = []
    for ch in str(pattern):
        if ch == "%":
            regex.append(".*")
        elif ch == "_":
            regex.append(".")
        else:
            regex.append(re.escape(ch))
    return re.fullmatch("".join(regex), str(value), flags=re.DOTALL) is not None


def _cmp(op: str, left: Any, right: Any) -> bool:
    if left is None or right is None:
        return False
    if op == "COMPARE_EQUAL":
        return left == right
    if op == "COMPARE_NOTEQUAL":
        return left != right
    if op == "COMPARE_GREATERTHAN":
        return left > right
    if op == "COMPARE_GREATERTHANOREQUALTO":
        return left >= right
    if op == "COMPARE_LESSTHAN":
        return left < right
    if op == "COMPARE_LESSTHANOREQUALTO":
        return left <= right
    if op == "COMPARE_DISTINCT_FROM":
        return left != right
    if op == "COMPARE_NOT_DISTINCT_FROM":
        return left == right
    raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported comparison {op}")


def _arith(op: str, values: list[Any]) -> Any:
    if any(v is None for v in values):
        return None
    if op == "+":
        return sum(values)
    if op == "-":
        if len(values) == 1:
            return -values[0]
        return values[0] - values[1]
    if op == "*":
        result = 1
        for v in values:
            result *= v
        return result
    if op == "/":
        if values[1] == 0:
            return None
        return values[0] / values[1]
    if op in ("%", "MOD"):
        if values[1] == 0:
            return None
        return values[0] % values[1]
    if op == "||":
        return "".join(str(v) for v in values)
    raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported operator {op}")


class ExprEvaluator:
    def __init__(self, ast: dict[str, Any] | None, sql: str = "") -> None:
        self.ast = ast
        self.sql = sql
        if ast is not None:
            self.validate(ast)

    def validate(self, node: Any) -> None:
        if not isinstance(node, dict):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_EXPRESSION",
                "expression node is not an object",
                fragment=str(node)[:300],
            )
        cls = node.get("class") or node.get("type")
        if cls in ("SUBQUERY", "SUBQUERY_NODE"):
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_SUBQUERY",
                "subqueries are not supported by the three-stage planner",
                fragment=_node_fragment(node, self.sql),
            )
        if cls not in _ALLOWED_CLASSES:
            raise EngineError(
                "IOV_SQL_UNSUPPORTED_EXPRESSION",
                f"unsupported expression class {cls!r}",
                fragment=_node_fragment(node, self.sql),
            )
        if cls == "FUNCTION":
            fn = str(node.get("function_name", "")).lower()
            if node.get("filter"):
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_AGGREGATE",
                    "aggregate FILTER clauses are not supported",
                    fragment=_node_fragment(node, self.sql),
                )
            if node.get("order_bys", {}).get("orders"):
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_AGGREGATE",
                    "ordered aggregates are not supported",
                    fragment=_node_fragment(node, self.sql),
                )
            if fn in _NON_DETERMINISTIC:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_EXPRESSION",
                    f"non-deterministic function {fn!r} is not supported",
                    fragment=_node_fragment(node, self.sql),
                )
            if fn not in _FUNCTIONS and fn not in _AGGREGATE_FUNCTIONS and fn not in {
                "+",
                "-",
                "*",
                "/",
                "%",
                "||",
                "~~",
                "!~~",
            }:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_EXPRESSION",
                    f"unsupported function {fn!r}",
                    fragment=_node_fragment(node, self.sql),
                )
            for arg in node.get("arguments", []):
                self.validate(arg.get("expression"))
        elif cls == "OPERATOR":
            typ = str(node.get("type", ""))
            if typ not in {
                "OPERATOR_NOT",
                "COMPARE_IN",
                "COMPARE_NOT_IN",
                "OPERATOR_IS_NULL",
                "OPERATOR_IS_NOT_NULL",
                "COMPARE_LIKE",
                "COMPARE_NOT_LIKE",
                "OPERATOR_IS_DISTINCT_FROM",
                "OPERATOR_IS_NOT_DISTINCT_FROM",
                "OPERATOR_CAST",
            }:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_EXPRESSION",
                    f"unsupported operator {typ}",
                    fragment=_node_fragment(node, self.sql),
                )
            for child in node.get("children", []):
                self.validate(child)
        elif cls == "COMPARISON":
            self.validate(node.get("left"))
            self.validate(node.get("right"))
        elif cls == "CONJUNCTION":
            for child in node.get("children", []):
                self.validate(child)
        elif cls == "BETWEEN":
            self.validate(node.get("input"))
            self.validate(node.get("lower"))
            self.validate(node.get("upper"))
        elif cls == "CAST":
            self.validate(node.get("child"))
        elif cls == "CASE":
            for check in node.get("case_checks", []):
                self.validate(check.get("when_expr"))
                self.validate(check.get("then_expr"))
            if node.get("else_expr") is not None:
                self.validate(node.get("else_expr"))

    def eval(self, row: dict[str, Any]) -> Any:
        if self.ast is None:
            return True
        return self._eval(self.ast, row)

    def _eval(self, node: dict[str, Any], row: dict[str, Any]) -> Any:
        cls = node.get("class")
        if cls == "COLUMN_REF":
            names = node.get("column_names") or []
            name = names[-1] if names else ""
            return row.get(name)
        if cls == "CONSTANT":
            return _literal(node)
        if cls == "COMPARISON":
            return _cmp(
                str(node.get("type", "")),
                self._eval(node.get("left"), row),
                self._eval(node.get("right"), row),
            )
        if cls == "CONJUNCTION":
            typ = node.get("type")
            values = [_truth(self._eval(child, row)) for child in node.get("children", [])]
            if typ == "CONJUNCTION_AND":
                if any(v is False for v in values):
                    return False
                if any(v is None for v in values):
                    return None
                return True
            if typ == "CONJUNCTION_OR":
                if any(v is True for v in values):
                    return True
                if any(v is None for v in values):
                    return None
                return False
            raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported conjunction {typ}")
        if cls == "BETWEEN":
            value = self._eval(node.get("input"), row)
            lo = self._eval(node.get("lower"), row)
            hi = self._eval(node.get("upper"), row)
            if value is None or lo is None or hi is None:
                return False
            return lo <= value <= hi
        if cls == "OPERATOR":
            typ = str(node.get("type", ""))
            children = node.get("children", [])
            vals = [self._eval(child, row) for child in children]
            if typ == "OPERATOR_NOT":
                t = _truth(vals[0] if vals else None)
                return None if t is None else not t
            if typ in ("COMPARE_IN", "COMPARE_NOT_IN"):
                found = any(vals[0] == v for v in vals[1:] if v is not None)
                return found if typ == "COMPARE_IN" else not found
            if typ == "OPERATOR_IS_NULL":
                return vals[0] is None
            if typ == "OPERATOR_IS_NOT_NULL":
                return vals[0] is not None
            if typ == "COMPARE_LIKE":
                return _sql_like(vals[0], vals[1])
            if typ == "COMPARE_NOT_LIKE":
                return not _sql_like(vals[0], vals[1])
            if typ in ("OPERATOR_IS_DISTINCT_FROM",):
                return vals[0] != vals[1]
            if typ in ("OPERATOR_IS_NOT_DISTINCT_FROM",):
                return vals[0] == vals[1]
            if typ == "OPERATOR_CAST":
                return vals[0]
            raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported operator {typ}")
        if cls == "CAST":
            value = self._eval(node.get("child"), row)
            type_name = str(node.get("type_expr", {}).get("type_name", "")).upper()
            if type_name in ("INTERVAL",):
                return IntervalValue(value)
            if type_name in ("INTEGER", "BIGINT", "INT", "SMALLINT", "HUGEINT"):
                return None if value is None else int(value)
            if type_name in ("DOUBLE", "FLOAT", "REAL", "DECIMAL"):
                return None if value is None else float(value)
            if type_name in ("VARCHAR", "TEXT", "STRING"):
                return None if value is None else str(value)
            return value
        if cls == "CASE":
            for check in node.get("case_checks", []):
                if _truth(self._eval(check.get("when_expr"), row)) is True:
                    return self._eval(check.get("then_expr"), row)
            return self._eval(node.get("else_expr"), row) if node.get("else_expr") else None
        if cls == "FUNCTION":
            fn = str(node.get("function_name", "")).lower()
            args = [self._eval(arg.get("expression"), row) for arg in node.get("arguments", [])]
            return self._function(fn, args)
        raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported expression class {cls}")

    def _function(self, fn: str, args: list[Any]) -> Any:
        if fn in ("+", "-", "*", "/", "%", "||"):
            return _arith(fn, args)
        if fn in ("~~",):
            return _sql_like(args[0], args[1]) if len(args) > 1 else False
        if fn in ("!~~",):
            return (not _sql_like(args[0], args[1])) if len(args) > 1 else True
        if fn == "lower":
            return None if args[0] is None else str(args[0]).lower()
        if fn == "upper":
            return None if args[0] is None else str(args[0]).upper()
        if fn in ("trim", "ltrim", "rtrim"):
            if args[0] is None:
                return None
            value = str(args[0])
            return value.strip() if fn == "trim" else (value.lstrip() if fn == "ltrim" else value.rstrip())
        if fn in ("length", "len"):
            return None if args[0] is None else len(str(args[0]))
        if fn == "abs":
            return None if args[0] is None else abs(args[0])
        if fn == "floor":
            return None if args[0] is None else math.floor(args[0])
        if fn in ("ceil", "ceiling"):
            return None if args[0] is None else math.ceil(args[0])
        if fn == "round":
            if args[0] is None:
                return None
            digits = int(args[1]) if len(args) > 1 and args[1] is not None else 0
            return round(args[0], digits)
        if fn == "coalesce":
            return next((a for a in args if a is not None), None)
        if fn == "nullif":
            return None if len(args) > 1 and args[0] == args[1] else (args[0] if args else None)
        if fn == "greatest":
            vals = [a for a in args if a is not None]
            return max(vals) if vals else None
        if fn == "least":
            vals = [a for a in args if a is not None]
            return min(vals) if vals else None
        if fn == "epoch":
            return None if args[0] is None else float(parse_time(args[0]))
        if fn == "to_timestamp":
            return None if args[0] is None else ts_iso(parse_time(args[0]))
        if fn == "strftime":
            if args[0] is None or len(args) < 2:
                return None
            dt = datetime.fromtimestamp(parse_time(args[1]), tz=UTC)
            return dt.strftime(str(args[0]))
        if fn == "date_trunc":
            if args[0] is None or len(args) < 2:
                return None
            dt = datetime.fromtimestamp(parse_time(args[1]), tz=UTC)
            unit = str(args[0]).lower()
            if unit in ("minute", "minutes"):
                dt = dt.replace(second=0, microsecond=0)
            elif unit in ("hour", "hours"):
                dt = dt.replace(minute=0, second=0, microsecond=0)
            elif unit in ("day", "days"):
                dt = dt.replace(hour=0, minute=0, second=0, microsecond=0)
            elif unit in ("month", "months"):
                dt = dt.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
            elif unit in ("year", "years"):
                dt = dt.replace(month=1, day=1, hour=0, minute=0, second=0, microsecond=0)
            else:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported date_trunc unit {unit!r}"
                )
            return ts_iso(int(dt.timestamp()))
        if fn in ("substr", "substring"):
            if args[0] is None:
                return None
            start = int(args[1]) if len(args) > 1 else 1
            length = int(args[2]) if len(args) > 2 else None
            return str(args[0])[start - 1 : (start - 1 + length if length is not None else None)]
        if fn == "concat":
            return "".join("" if a is None else str(a) for a in args)
        if fn in ("time_bucket", "tumble"):
            if len(args) != 2:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_WINDOW", f"{fn} expects two arguments"
                )
            if fn == "time_bucket":
                width = interval_seconds(args[0])
                value = args[1]
            elif _looks_interval(args[0]) or isinstance(args[0], IntervalValue):
                width = interval_seconds(args[0])
                value = args[1]
            else:
                value = args[0]
                width = interval_seconds(args[1])
            return (parse_time(value) // width) * width
        if fn == "window_start":
            if len(args) != 2:
                raise EngineError(
                    "IOV_SQL_UNSUPPORTED_WINDOW", "window_start expects (timestamp, seconds)"
                )
            # Contract: window_start(event_time, width_seconds_or_interval).
            value, width = args[0], args[1]
            seconds = interval_seconds(width)
            return (parse_time(value) // seconds) * seconds
        raise EngineError("IOV_SQL_UNSUPPORTED_EXPRESSION", f"unsupported function {fn!r}")


def _looks_interval(value: Any) -> bool:
    if isinstance(value, IntervalValue):
        return True
    if isinstance(value, str):
        text = value.strip().lower()
        return (
            re.fullmatch(
                r"-?\d+(?:\.\d+)?\s*(?:second|seconds|sec|secs|s|minute|minutes|min|mins|m|hour|hours|hr|hrs|h|day|days|d)?",
                text,
            )
            is not None
            or re.fullmatch(r"-?\d+:\d+(?::\d+)?", text) is not None
        )
    return False


def _node_fragment(node: dict[str, Any], sql: str) -> str:
    try:
        loc = int(node.get("query_location", -1))
        length = int(node.get("query_location_length", 0))
        if 0 <= loc < len(sql):
            return sql[loc : loc + max(length, 1)]
    except Exception:
        pass
    return f"<{node.get('class', node.get('type', 'unknown'))}>"


def validate_window_ast(ast: dict[str, Any], sql: str) -> None:
    """Validate a window expression and ensure it references an event column."""
    evaluator = ExprEvaluator(ast, sql)
    evaluator.validate(ast)
    if not _references_column(ast, ("event_time", "ts", "timestamp")):
        raise EngineError(
            "IOV_SQL_UNSUPPORTED_WINDOW",
            "window expression must reference the event-time column",
            fragment=_node_fragment(ast, sql),
        )


def _references_column(node: Any, names: tuple[str, ...]) -> bool:
    if not isinstance(node, dict):
        return False
    if node.get("class") == "COLUMN_REF":
        cols = node.get("column_names") or []
        return bool(cols) and str(cols[-1]).lower() in names
    for value in node.values():
        if isinstance(value, dict) and _references_column(value, names):
            return True
        if isinstance(value, list):
            for item in value:
                if isinstance(item, dict) and _references_column(item, names):
                    return True
    return False

