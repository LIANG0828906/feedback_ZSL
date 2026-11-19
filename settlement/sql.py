from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass, field
from datetime import date, datetime
from typing import Any, Iterable

from .errors import SqlParseError
from .timeutil import datetime_to_us, parse_date, parse_datetime, us_to_datetime


_IDENT = r'(?:"[^"]+"|`[^`]+`|[A-Za-z_][A-Za-z0-9_]*)'
_QUALIFIED_IDENT = rf'(?:{_IDENT}\s*\.\s*)?{_IDENT}'
_AGG_SUM_RE = re.compile(
    rf"\bsum\s*\(\s*(?:{_IDENT}\s*\.\s*)?({_IDENT})\s*\)", re.IGNORECASE
)
_DATE_TRUNC_RE = re.compile(
    rf"\bdate_trunc\s*\(\s*'([^']+)'\s*,\s*(?:{_IDENT}\s*\.\s*)?event_time\s*\)",
    re.IGNORECASE,
)
_TIME_BUCKET_RE = re.compile(
    rf"\btime_bucket\s*\(\s*'([^']+)'\s*,\s*(?:{_IDENT}\s*\.\s*)?event_time\s*\)",
    re.IGNORECASE,
)
_CAST_DATE_RE = re.compile(
    rf"\bcast\s*\(\s*(?:{_IDENT}\s*\.\s*)?event_time\s+as\s+date\s*\)",
    re.IGNORECASE,
)
_DATE_FN_RE = re.compile(
    rf"\bdate\s*\(\s*(?:{_IDENT}\s*\.\s*)?event_time\s*\)", re.IGNORECASE
)


def _strip_sql_comments(sql: str) -> str:
    sql = re.sub(r"/\*.*?\*/", " ", sql, flags=re.DOTALL)
    sql = re.sub(r"--[^\n]*", " ", sql)
    return sql


def _scan_top_level(sql: str) -> list[tuple[int, int, str]]:
    words: list[tuple[int, int, str]] = []
    depth = 0
    index = 0
    length = len(sql)
    while index < length:
        char = sql[index]
        if char in ("'", '"', "`"):
            quote = char
            index += 1
            while index < length:
                if sql[index] == quote:
                    if index + 1 < length and sql[index + 1] == quote:
                        index += 2
                        continue
                    index += 1
                    break
                index += 1
            continue
        if char == "(":
            depth += 1
            index += 1
            continue
        if char == ")":
            depth = max(0, depth - 1)
            index += 1
            continue
        if depth == 0 and (char.isalpha() or char == "_"):
            start = index
            index += 1
            while index < length and (sql[index].isalnum() or sql[index] in ("_", "$")):
                index += 1
            words.append((start, index - start, sql[start:index].upper()))
            continue
        index += 1
    return words


def _top_level_sections(sql: str) -> dict[str, tuple[int, int]]:
    words = _scan_top_level(sql)
    sections: dict[str, tuple[int, int]] = {}
    index = 0
    while index < len(words):
        position, width, word = words[index]
        if word in {"SELECT", "FROM", "WHERE", "HAVING", "LIMIT", "OFFSET"}:
            sections.setdefault(word, (position, position + width))
        elif word in {"GROUP", "ORDER"} and index + 1 < len(words):
            next_position, next_width, next_word = words[index + 1]
            if next_word == "BY":
                name = f"{word} BY"
                sections.setdefault(name, (position, next_position + next_width))
                index += 1
        index += 1
    return sections


def _segment(sql: str, start: int, end: int) -> str:
    return sql[start:end].strip()


def _split_top_level(text: str, delimiter: str = ",") -> list[str]:
    pieces: list[str] = []
    depth = 0
    quote: str | None = None
    start = 0
    index = 0
    while index < len(text):
        char = text[index]
        if quote is not None:
            if char == quote:
                if index + 1 < len(text) and text[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in ("'", '"', "`"):
            quote = char
            index += 1
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth = max(0, depth - 1)
        elif depth == 0 and text.startswith(delimiter, index):
            pieces.append(text[start:index].strip())
            start = index + len(delimiter)
            index = start
            continue
        index += 1
    pieces.append(text[start:].strip())
    return [piece for piece in pieces if piece]


def _has_top_level_word(text: str, word: str) -> bool:
    target = word.upper()
    for position, width, candidate in _scan_top_level(text):
        if candidate == target:
            return True
    return False


def _split_and_terms(text: str) -> list[str]:
    terms: list[str] = []
    depth = 0
    quote: str | None = None
    start = 0
    index = 0
    pending_between = False
    while index < len(text):
        char = text[index]
        if quote is not None:
            if char == quote:
                if index + 1 < len(text) and text[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in ("'", '"', "`"):
            quote = char
            index += 1
            continue
        if char == "(":
            depth += 1
            index += 1
            continue
        if char == ")":
            depth = max(0, depth - 1)
            index += 1
            continue
        if depth == 0 and (char.isalpha() or char == "_"):
            end = index + 1
            while end < len(text) and (text[end].isalnum() or text[end] == "_"):
                end += 1
            word = text[index:end].upper()
            if word == "BETWEEN":
                pending_between = True
            elif word == "AND":
                if pending_between:
                    pending_between = False
                else:
                    terms.append(text[start:index].strip())
                    start = end
            index = end
            continue
        index += 1
    terms.append(text[start:].strip())
    return [term for term in terms if term]


def _strip_outer_parens(text: str) -> str:
    value = text.strip()
    while value.startswith("(") and value.endswith(")"):
        depth = 0
        quote: str | None = None
        balanced = True
        for index, char in enumerate(value):
            if quote is not None:
                if char == quote:
                    quote = None
                continue
            if char in ("'", '"', "`"):
                quote = char
            elif char == "(":
                depth += 1
            elif char == ")":
                depth -= 1
                if depth == 0 and index != len(value) - 1:
                    balanced = False
                    break
        if not balanced or depth != 0:
            break
        value = value[1:-1].strip()
    return value


def _unquote_identifier(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ('"', "`"):
        return value[1:-1]
    return value


def _normalize_field(value: str) -> str:
    text = value.strip()
    text = re.sub(r"\s+", " ", text)
    parts = text.split(".")
    if len(parts) == 2:
        text = parts[-1]
    text = _unquote_identifier(text).lower()
    aliases = {
        "day": "partition_date",
        "dt": "partition_date",
        "event_date": "partition_date",
        "partition": "partition_date",
        "partition_day": "partition_date",
        "vehicle": "vehicle_id",
        "trip": "trip_id",
        "source": "source_type",
    }
    return aliases.get(text, text)


def _parse_select_alias(item: str) -> tuple[str, str]:
    match = re.match(rf"^(.*?)\s+as\s+({_IDENT})\s*$", item, re.IGNORECASE | re.DOTALL)
    if match:
        return match.group(1).strip(), _unquote_identifier(match.group(2))
    # A trailing bare identifier is accepted as an alias, but only when the
    # preceding expression is clearly an aggregate or function call.
    match = re.match(rf"^(.*[\)\]])\s+({_IDENT})\s*$", item, re.DOTALL)
    if match:
        return match.group(1).strip(), _unquote_identifier(match.group(2))
    return item.strip(), ""


def _parse_interval(value: str) -> tuple[int, str]:
    text = value.strip().lower()
    match = re.fullmatch(r"(\d+)?\s*(second|minute|hour|day|week)s?", text)
    if not match:
        raise SqlParseError(
            f"unsupported window interval {value!r}; expected seconds, minutes, hours, days or weeks",
            details={"interval": value},
        )
    count = int(match.group(1) or "1")
    unit = match.group(2)
    seconds = count * {
        "second": 1,
        "minute": 60,
        "hour": 3600,
        "day": 86400,
        "week": 604800,
    }[unit]
    return seconds, f"{count} {unit}{'' if count == 1 else 's'}"


@dataclass(frozen=True)
class Predicate:
    raw: str
    field: str
    op: str
    values: tuple[Any, ...]
    safe_to_push: bool
    push_reason: str
    retained_reason: str | None = None

    @property
    def pushed(self) -> bool:
        return self.safe_to_push

    def to_dict(self) -> dict[str, Any]:
        physical = self.pushed and self.field in {
            "partition_date",
            "vehicle_id",
            "trip_id",
        }
        return {
            "expression": self.raw,
            "field": self.field,
            "operator": self.op,
            "values": [_json_value(value) for value in self.values],
            "pushed_to_scan": self.pushed,
            "physical_parquet_pushdown": physical,
            "reason": self.push_reason if self.pushed else self.retained_reason,
        }

    def matches(self, event: dict[str, Any]) -> bool:
        if self.field == "window_state":
            # Late/watermark/closed predicates are enforced by the executor's
            # window-close pipeline, not by the source scanner.
            return True
        if self.field == "window_start":
            actual = event.get("window_start_us")
        elif self.field == "event_time":
            actual = event.get("event_time_us")
        else:
            actual = event.get(self.field)
        if self.op == "IS NULL":
            return actual is None
        if self.op == "IS NOT NULL":
            return actual is not None
        if actual is None:
            return False
        if self.op in {"IN", "NOT IN"}:
            hit = actual in self.values
            return hit if self.op == "IN" else not hit
        if self.op == "LIKE":
            pattern = str(self.values[0])
            regex = "".join(
                ".*" if char == "%" else "." if char == "_" else re.escape(char)
                for char in pattern
            )
            return re.fullmatch(regex, str(actual), flags=re.DOTALL) is not None
        if self.op == "BETWEEN":
            return self.values[0] <= actual <= self.values[1]
        if self.op == "NOT BETWEEN":
            return not (self.values[0] <= actual <= self.values[1])
        right = self.values[0] if self.values else None
        if self.op in {"=", "=="}:
            return actual == right
        if self.op in {"!=", "<>"}:
            return actual != right
        if self.op == ">":
            return actual > right
        if self.op == ">=":
            return actual >= right
        if self.op == "<":
            return actual < right
        if self.op == "<=":
            return actual <= right
        raise SqlParseError(f"unsupported predicate operator {self.op!r}")


@dataclass
class SettlementQuery:
    sql: str
    source_sql: str
    predicates: list[Predicate]
    window_seconds: int
    window_label: str
    window_expression: str
    vehicle_expression: str
    aliases: dict[str, str]
    has_having: bool = False
    has_order_by: bool = False
    has_limit: bool = False
    extras: dict[str, Any] = field(default_factory=dict)

    @property
    def sql_sha256(self) -> str:
        canonical = re.sub(r"\s+", " ", self.sql.strip()).strip().lower()
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()

    @property
    def pushed_predicates(self) -> list[Predicate]:
        return [predicate for predicate in self.predicates if predicate.pushed]

    @property
    def retained_predicates(self) -> list[Predicate]:
        return [predicate for predicate in self.predicates if not predicate.pushed]

    def matches(self, event: dict[str, Any]) -> bool:
        return all(predicate.matches(event) for predicate in self.predicates)

    def scan_filter(self):
        """Return a pyarrow dataset expression for partition/key predicates."""

        import pyarrow.compute as pc
        import pyarrow.dataset as ds

        expressions = []
        for predicate in self.pushed_predicates:
            field_name = {
                "partition_date": "partition_date",
                "vehicle_id": "vehicle_id",
                "trip_id": "trip_id",
            }.get(predicate.field)
            if field_name is None:
                continue
            column = ds.field(field_name)
            try:
                if predicate.op in {"=", "=="}:
                    expressions.append(column == predicate.values[0])
                elif predicate.op in {"!=", "<>"}:
                    expressions.append(column != predicate.values[0])
                elif predicate.op == ">":
                    expressions.append(column > predicate.values[0])
                elif predicate.op == ">=":
                    expressions.append(column >= predicate.values[0])
                elif predicate.op == "<":
                    expressions.append(column < predicate.values[0])
                elif predicate.op == "<=":
                    expressions.append(column <= predicate.values[0])
                elif predicate.op == "IN":
                    expressions.append(column.isin(list(predicate.values)))
                elif predicate.op == "NOT IN":
                    expressions.append(~column.isin(list(predicate.values)))
                elif predicate.op == "BETWEEN":
                    expressions.append(
                        (column >= predicate.values[0]) & (column <= predicate.values[1])
                    )
                elif predicate.op == "NOT BETWEEN":
                    expressions.append(
                        (column < predicate.values[0]) | (column > predicate.values[1])
                    )
                elif predicate.op == "IS NULL":
                    expressions.append(column.is_null())
                elif predicate.op == "IS NOT NULL":
                    expressions.append(~column.is_null())
                elif predicate.op == "LIKE":
                    pattern = str(predicate.values[0])
                    if "_" in pattern:
                        continue
                    if pattern.startswith("%") and pattern.endswith("%") and len(pattern) >= 3:
                        expressions.append(pc.match_substring(column, pattern[1:-1]))
                    elif pattern.endswith("%"):
                        expressions.append(pc.starts_with(column, pattern[:-1]))
                    elif pattern.startswith("%"):
                        expressions.append(pc.ends_with(column, pattern[1:]))
                    else:
                        expressions.append(column == pattern)
            except Exception as exc:  # pragma: no cover - pyarrow version guard
                raise SqlParseError(
                    f"could not convert pushed predicate to a Parquet filter: {predicate.raw}",
                    details={"error": str(exc)},
                ) from exc
        if not expressions:
            return None
        result = expressions[0]
        for expression in expressions[1:]:
            result = result & expression
        # Keep pc referenced so old pyarrow versions keep scalar helpers loaded.
        _ = pc
        return result

    @property
    def output_aliases(self) -> dict[str, str]:
        return {
            "vehicle_id": self.aliases.get("vehicle", "vehicle_id"),
            "window_start": self.aliases.get("window", "window_start"),
            "mileage_delta": self.aliases.get("mileage", "mileage_delta"),
            "energy_delta": self.aliases.get("energy", "energy_delta"),
            "energy_per_100km": self.aliases.get("ratio", "energy_per_100km"),
        }

    def plan_summary(self) -> dict[str, Any]:
        return {
            "source": "vehicle_events (telemetry_events UNION charging_supplements)",
            "window": {
                "expression": self.window_expression,
                "interval_seconds": self.window_seconds,
                "label": self.window_label,
            },
            "join_keys": ["vehicle_id", "trip_id"],
            "group_by": ["vehicle_id", "window_start"],
            "pushed_predicates": [predicate.to_dict() for predicate in self.pushed_predicates],
            "retained_predicates": [predicate.to_dict() for predicate in self.retained_predicates],
            "pushed_aggregates": [
                {
                    "expression": "SUM(mileage_delta)",
                    "reason": "associative integer partial sum; merged after key redistribution",
                },
                {
                    "expression": "SUM(energy_delta)",
                    "reason": "associative integer partial sum; merged after key redistribution",
                },
            ],
            "retained_operations": [
                {
                    "expression": "event_time / window_start predicates",
                    "reason": "must be evaluated after watermark and window-close classification",
                },
                {
                    "expression": "energy_per_100km",
                    "reason": "ratio is computed only after all partial vehicle/window sums merge",
                },
            ],
            "local_preaggregation": {
                "enabled": True,
                "grain": ["processing_slot", "window_start", "vehicle_id", "trip_id", "source_type"],
                "merge_grain": ["window_start", "vehicle_id"],
                "numeric_representation": "fixed-point integer micro-units",
            },
            "ordering": "deterministic by vehicle_id, window_start",
            "aliases": self.output_aliases,
            "sql_sha256": self.sql_sha256,
        }


def parse_sql(sql: str) -> SettlementQuery:
    source_sql = _strip_sql_comments(sql).strip().rstrip(";").strip()
    if not source_sql:
        raise SqlParseError("settlement SQL is empty")
    if source_sql.lstrip().upper().startswith("WITH "):
        raise SqlParseError("common table expressions are not supported by the local planner")
    sections = _top_level_sections(source_sql)
    if "SELECT" not in sections or "FROM" not in sections:
        raise SqlParseError("settlement SQL must contain a top-level SELECT and FROM")
    select_start = sections["SELECT"][1]
    from_start = sections["FROM"][0]
    if select_start >= from_start:
        raise SqlParseError("could not order the SELECT and FROM clauses")
    select_text = _segment(source_sql, select_start, from_start)

    following = [
        (position, name)
        for name, (position, _end) in sections.items()
        if position > sections["FROM"][0]
    ]
    following.sort()
    source_end = following[0][0] if following else len(source_sql)
    from_text = _segment(source_sql, sections["FROM"][1], source_end)
    source_match = re.fullmatch(
        rf"(?:{_IDENT})(?:\s+(?:as\s+)?{_IDENT})?", from_text, re.IGNORECASE | re.DOTALL
    )
    if not source_match:
        raise SqlParseError(
            "the FROM clause must name the logical relation vehicle_events (optionally aliased)",
            details={"from": from_text},
        )
    source_name = _unquote_identifier(from_text.split()[0]).lower()
    if source_name != "vehicle_events":
        raise SqlParseError(
            "unsupported source relation; submit SQL against vehicle_events",
            details={"source": source_name},
        )

    where_text = ""
    if "WHERE" in sections:
        where_start = sections["WHERE"][1]
        later = [
            position
            for name, (position, _end) in sections.items()
            if position > sections["WHERE"][0]
        ]
        where_end = min(later) if later else len(source_sql)
        where_text = _segment(source_sql, where_start, where_end)

    group_text = ""
    if "GROUP BY" in sections:
        group_start = sections["GROUP BY"][1]
        later = [
            position
            for name, (position, _end) in sections.items()
            if position > sections["GROUP BY"][0]
        ]
        group_end = min(later) if later else len(source_sql)
        group_text = _segment(source_sql, group_start, group_end)
    if not group_text:
        raise SqlParseError("settlement SQL must contain GROUP BY vehicle_id and a window expression")

    select_items = [_parse_select_alias(item) for item in _split_top_level(select_text)]
    aliases: dict[str, str] = {}
    alias_to_expression: dict[str, str] = {}
    vehicle_expression = ""
    window_expression = ""
    window_seconds = 0
    window_label = ""
    for expression, alias in select_items:
        normalized = expression.strip()
        alias_to_expression[alias.lower()] = normalized
        identifier = _normalize_field(normalized) if re.fullmatch(_QUALIFIED_IDENT, normalized.strip()) else ""
        if identifier == "vehicle_id" and not vehicle_expression:
            vehicle_expression = normalized
            aliases["vehicle"] = alias or "vehicle_id"
        interval = _detect_window(normalized)
        if interval is not None and not window_expression:
            window_seconds, window_label = interval
            window_expression = normalized
            aliases["window"] = alias or "window_start"
        simple_sum = re.fullmatch(
            rf"\s*sum\s*\(\s*(?:{_IDENT}\s*\.\s*)?({_IDENT})\s*\)\s*",
            normalized,
            re.IGNORECASE,
        )
        if simple_sum:
            sum_field = _normalize_field(simple_sum.group(1))
            if sum_field in {"mileage_delta", "mileage_km", "mileage"}:
                aliases["mileage"] = alias or "mileage_delta"
            elif sum_field in {"energy_delta", "energy_kwh", "energy"}:
                aliases["energy"] = alias or "energy_delta"
        lowered = normalized.lower()
        if (
            alias
            and ("100" in lowered)
            and "sum" in lowered
            and "mileage" in lowered
            and "energy" in lowered
        ):
            aliases["ratio"] = alias

    if not vehicle_expression:
        raise SqlParseError("SELECT must project the vehicle identifier")
    if not window_expression:
        raise SqlParseError(
            "SELECT must project a supported window expression such as date_trunc('hour', event_time)"
        )

    group_seen_vehicle = False
    group_seen_window = False
    for item in _split_top_level(group_text):
        candidate = item.strip()
        if candidate.isdigit():
            ordinal = int(candidate) - 1
            if 0 <= ordinal < len(select_items):
                candidate = select_items[ordinal][0]
        lowered = candidate.lower()
        if lowered in alias_to_expression:
            candidate = alias_to_expression[lowered]
        field = _normalize_field(candidate) if re.fullmatch(_QUALIFIED_IDENT, candidate.strip()) else ""
        if field == "vehicle_id":
            group_seen_vehicle = True
            continue
        if _detect_window(candidate) is not None:
            group_seen_window = True
            continue
        raise SqlParseError(
            "GROUP BY may only contain vehicle_id and the window expression",
            details={"group_item": item},
        )
    if not group_seen_vehicle or not group_seen_window:
        raise SqlParseError("GROUP BY must contain both vehicle_id and the window expression")

    predicates: list[Predicate] = []
    if where_text:
        if _has_top_level_word(where_text, "OR"):
            raise SqlParseError("top-level OR predicates are not supported by the conservative planner")
        for term in _split_and_terms(where_text):
            predicates.append(_parse_predicate(term))
    if "HAVING" in sections:
        raise SqlParseError("HAVING is not supported by the local settlement planner")
    if "LIMIT" in sections:
        raise SqlParseError("LIMIT is not supported; the executor publishes the complete window set")

    return SettlementQuery(
        sql=source_sql,
        source_sql=source_sql,
        predicates=predicates,
        window_seconds=window_seconds,
        window_label=window_label,
        window_expression=window_expression,
        vehicle_expression=vehicle_expression,
        aliases=aliases,
        has_having="HAVING" in sections,
        has_order_by="ORDER BY" in sections,
        has_limit="LIMIT" in sections,
    )


def _detect_window(expression: str) -> tuple[int, str] | None:
    for regex in (_DATE_TRUNC_RE, _TIME_BUCKET_RE):
        match = regex.search(expression)
        if match:
            return _parse_interval(match.group(1))
    if _CAST_DATE_RE.search(expression) or _DATE_FN_RE.search(expression):
        return 86400, "1 day"
    return None


def _parse_predicate(term: str) -> Predicate:
    raw = _strip_outer_parens(term.strip())
    if not raw:
        raise SqlParseError("empty WHERE predicate")
    match = re.match(
        r"^(.*?)\s*(IS\s+NOT\s+NULL|IS\s+NULL|NOT\s+IN|IN|NOT\s+BETWEEN|BETWEEN|LIKE|>=|<=|<>|!=|==|=|>|<)\s*(.*)$",
        raw,
        re.IGNORECASE | re.DOTALL,
    )
    if not match:
        raise SqlParseError(f"could not parse WHERE predicate {raw!r}")
    left, operator, right = match.group(1).strip(), match.group(2).upper(), match.group(3).strip()
    operator = re.sub(r"\s+", " ", operator)
    field_name = _normalize_field(left)
    if _detect_window(left) is not None:
        field_name = "window_start"
    elif field_name not in {
        "vehicle_id",
        "trip_id",
        "partition_date",
        "event_time",
        "window_start",
        "source_type",
        "window_state",
    }:
        lowered_left = left.lower()
        if any(
            token in lowered_left
            for token in ("watermark", "late", "closed", "window_end", "ingest")
        ):
            field_name = "window_state"
        else:
            raise SqlParseError(
                f"unsupported predicate field {left!r}",
                details={"predicate": raw},
            )

    if operator in {"IS NULL", "IS NOT NULL"}:
        values: tuple[Any, ...] = ()
    elif operator == "IN" or operator == "NOT IN":
        values = tuple(_parse_literal(value, field_name) for value in _parse_in_values(right))
    elif operator in {"BETWEEN", "NOT BETWEEN"}:
        between_parts = _split_and_terms(right)
        if len(between_parts) != 2:
            raise SqlParseError(f"malformed BETWEEN predicate {raw!r}")
        values = (
            _parse_literal(between_parts[0], field_name),
            _parse_literal(between_parts[1], field_name),
        )
    elif operator == "LIKE":
        values = (_parse_literal(right, field_name),)
    else:
        values = (_parse_literal(right, field_name),)

    window_dependent = field_name in {"event_time", "window_start", "window_state"}
    if field_name in {"partition_date", "vehicle_id", "trip_id", "source_type"}:
        safe = True
        reason = "partition or join key is independent of window closure and watermark"
        retained_reason = None
    else:
        safe = False
        reason = ""
        retained_reason = (
            "event-time/window predicate is retained until after watermark and window-close evaluation"
        )

    return Predicate(
        raw=raw,
        field=field_name,
        op=operator,
        values=values,
        safe_to_push=safe,
        push_reason=reason,
        retained_reason=retained_reason,
    )


def _parse_in_values(right: str) -> list[str]:
    text = right.strip()
    if text.startswith("(") and text.endswith(")"):
        text = text[1:-1]
    return _split_top_level(text)


def _parse_literal(value: str, field_name: str) -> Any:
    text = value.strip()
    upper = text.upper()
    if upper == "NULL":
        return None
    if upper in {"TRUE", "FALSE"}:
        return upper == "TRUE"
    typed = re.match(
        r"^(?:TIMESTAMP|DATETIME|DATE|TIME)\s*'((?:''|[^'])*)'$", text, re.IGNORECASE | re.DOTALL
    )
    if typed:
        text = "'" + typed.group(1) + "'"
    if len(text) >= 2 and text[0] == "'" and text[-1] == "'":
        unescaped = text[1:-1].replace("''", "'")
        if field_name in {"event_time", "window_start"}:
            return datetime_to_us(parse_datetime(unescaped))
        if field_name == "partition_date":
            return parse_date(unescaped)
        return unescaped
    if re.fullmatch(r"[+-]?\d+", text):
        return int(text)
    if re.fullmatch(r"[+-]?(?:\d+\.\d*|\.\d+|\d+[eE][+-]?\d+)", text):
        return float(text)
    # A quoted identifier on the right side is never treated as a column;
    # doing so would silently change the meaning of the settlement predicate.
    raise SqlParseError(f"unsupported literal {value!r} in predicate")


def _json_value(value: Any) -> Any:
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, date):
        return value.isoformat()
    return value


def predicate_identity(predicate: Predicate) -> str:
    return f"{predicate.field}:{predicate.op}:{predicate.values!r}"


def predicates_to_summary(predicates: Iterable[Predicate]) -> list[dict[str, Any]]:
    return sorted(
        (predicate.to_dict() for predicate in predicates),
        key=lambda item: (item["field"], item["operator"], item["expression"]),
    )

