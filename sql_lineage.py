"""SQL metadata loading, column lineage, impact and drift analysis."""

from __future__ import annotations

import hashlib
import heapq
import json
import math
import re
import sqlite3
from collections import OrderedDict, defaultdict, deque
from dataclasses import dataclass, field
from typing import Any, Iterable

from warehouse_model import quote_identifier


SQL_KEYWORDS = {
    "ALL",
    "AND",
    "AS",
    "ASC",
    "BETWEEN",
    "BY",
    "CASE",
    "CAST",
    "COALESCE",
    "COUNT",
    "CURRENT_DATE",
    "CURRENT_TIMESTAMP",
    "DELETE",
    "DESC",
    "DISTINCT",
    "ELSE",
    "END",
    "EXISTS",
    "FALSE",
    "FROM",
    "FULL",
    "GROUP",
    "HAVING",
    "IN",
    "INNER",
    "INSERT",
    "INTO",
    "IS",
    "JOIN",
    "LEFT",
    "LIKE",
    "LIMIT",
    "MAX",
    "MIN",
    "NOT",
    "NULL",
    "NULLIF",
    "OFFSET",
    "ON",
    "OR",
    "ORDER",
    "OUTER",
    "REPLACE",
    "RIGHT",
    "ROUND",
    "SELECT",
    "SET",
    "SUM",
    "THEN",
    "TRUE",
    "UNION",
    "UPDATE",
    "USING",
    "VALUES",
    "WHEN",
    "WHERE",
    "WITH",
}


class BusinessError(Exception):
    """A stable, client-visible business error."""

    def __init__(
        self,
        code: str,
        message: str,
        details: dict[str, Any] | None = None,
        http_status: int = 400,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details or {}
        self.http_status = http_status

    def as_dict(self) -> dict[str, Any]:
        payload = {
            "ok": False,
            "code": self.code,
            "error_code": self.code,
            "message": self.message,
            "details": self.details,
            "error": {
                "code": self.code,
                "message": self.message,
                "details": self.details,
            },
        }
        for key, value in self.details.items():
            payload.setdefault(key, value)
        return payload


@dataclass
class TaskLineage:
    task_name: str
    task_order: int
    description: str
    sql: str
    output_table: str
    last_run: dict[str, Any]
    reads: dict[str, set[str]] = field(default_factory=dict)
    writes: dict[str, set[str]] = field(default_factory=dict)
    read_columns: set[tuple[str, str]] = field(default_factory=set)
    write_columns: set[str] = field(default_factory=set)
    column_map: dict[tuple[str, str], set[tuple[str, str]]] = field(
        default_factory=dict
    )
    dependencies: set[str] = field(default_factory=set)
    depth: int = 0

    @property
    def output_columns(self) -> set[str]:
        return self.writes.get(self.output_table, set()) or self.write_columns

    def public(self) -> dict[str, Any]:
        return {
            "task_name": self.task_name,
            "task_order": self.task_order,
            "description": self.description,
            "output_table": self.output_table,
            "depth": self.depth,
            "last_run": self.last_run,
        }


@dataclass
class WarehouseMetadata:
    snapshot_id: str
    schema: OrderedDict[str, dict[str, Any]]
    tasks: list[TaskLineage]
    reports: list[dict[str, Any]]
    metrics: list[dict[str, Any]]
    table_dependencies: dict[str, set[str]] = field(default_factory=dict)
    table_depths: dict[str, int] = field(default_factory=dict)
    task_graph: dict[str, set[str]] = field(default_factory=dict)

    @property
    def task_by_name(self) -> dict[str, TaskLineage]:
        return {task.task_name: task for task in self.tasks}

    @property
    def table_names(self) -> set[str]:
        return set(self.schema)

    def find_table(self, name: str) -> str | None:
        lowered = str(name).strip().lower()
        for table_name in self.schema:
            if table_name.lower() == lowered:
                return table_name
        return None

    def find_column(self, table_name: str, name: str) -> str | None:
        table = self.schema.get(table_name)
        if not table:
            return None
        lowered = str(name).strip().lower()
        for column_name in table["columns"]:
            if column_name.lower() == lowered:
                return column_name
        return None


def _unquote_identifier(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "`", "["}:
        if value[0] == "[":
            return value[1:-1]
        return value[1:-1].replace(value[0] * 2, value[0])
    return value


def strip_sql_comments(sql: str) -> str:
    result: list[str] = []
    index = 0
    length = len(sql)
    quote: str | None = None
    while index < length:
        char = sql[index]
        if quote:
            result.append(char)
            if char == quote:
                if index + 1 < length and sql[index + 1] == quote:
                    result.append(sql[index + 1])
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in {"'", '"', "`"}:
            quote = char
            result.append(char)
            index += 1
            continue
        if char == "[":
            quote = "]"
            result.append(char)
            index += 1
            continue
        if sql.startswith("--", index):
            newline = sql.find("\n", index + 2)
            if newline < 0:
                break
            result.append("\n")
            index = newline + 1
            continue
        if sql.startswith("/*", index):
            end = sql.find("*/", index + 2)
            if end < 0:
                break
            result.append(" ")
            index = end + 2
            continue
        result.append(char)
        index += 1
    return "".join(result)


def split_sql_statements(sql: str) -> list[str]:
    statements: list[str] = []
    buffer = ""
    for line in sql.splitlines(keepends=True):
        buffer += line
        if sqlite3.complete_statement(buffer):
            statement = buffer.strip()
            if statement:
                statements.append(statement)
            buffer = ""
    if buffer.strip():
        statements.append(buffer.strip())
    return statements


def _find_top_level_keyword(text: str, keyword: str, start: int = 0) -> int:
    keyword_upper = keyword.upper()
    depth = 0
    quote: str | None = None
    index = start
    length = len(text)
    while index < length:
        char = text[index]
        if quote:
            if char == quote:
                if index + 1 < length and text[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in {"'", '"', "`"}:
            quote = char
            index += 1
            continue
        if char == "[":
            quote = "]"
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
            while end < length and (text[end].isalnum() or text[end] == "_"):
                end += 1
            token = text[index:end]
            if token.upper() == keyword_upper:
                before_ok = index == 0 or not (
                    text[index - 1].isalnum() or text[index - 1] == "_"
                )
                after_ok = end == length or not (
                    text[end].isalnum() or text[end] == "_"
                )
                if before_ok and after_ok:
                    return index
            index = end
            continue
        index += 1
    return -1


def _split_top_level(text: str, delimiter: str = ",") -> list[str]:
    parts: list[str] = []
    start = 0
    depth = 0
    quote: str | None = None
    index = 0
    length = len(text)
    while index < length:
        char = text[index]
        if quote:
            if char == quote:
                if index + 1 < length and text[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in {"'", '"', "`"}:
            quote = char
            index += 1
            continue
        if char == "[":
            quote = "]"
            index += 1
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth = max(0, depth - 1)
        elif char == delimiter and depth == 0:
            parts.append(text[start:index].strip())
            start = index + 1
        index += 1
    parts.append(text[start:].strip())
    return [part for part in parts if part]


def _matching_paren(text: str, start: int) -> int:
    depth = 0
    quote: str | None = None
    index = start
    while index < len(text):
        char = text[index]
        if quote:
            if char == quote:
                if index + 1 < len(text) and text[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            index += 1
            continue
        if char in {"'", '"', "`"}:
            quote = char
        elif char == "[":
            quote = "]"
        elif char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return index
        index += 1
    return -1


def _mask_string_literals(expression: str) -> str:
    output: list[str] = []
    index = 0
    quote: str | None = None
    while index < len(expression):
        char = expression[index]
        if quote:
            if char == quote:
                if index + 1 < len(expression) and expression[index + 1] == quote:
                    index += 2
                    continue
                quote = None
            output.append(" ")
            index += 1
            continue
        if char in {"'", '"', "`"}:
            quote = char
            output.append(" ")
            index += 1
            continue
        if char == "[":
            quote = "]"
            output.append(" ")
            index += 1
            continue
        output.append(char)
        index += 1
    return "".join(output)


def _table_aliases(statement: str, schema: dict[str, dict[str, Any]]) -> dict[str, str]:
    aliases: dict[str, str] = {}
    pattern = re.compile(
        r'\b(?:FROM|JOIN)\s+("?[A-Za-z_][A-Za-z0-9_]*"?)'
        r'(?:\s+(?:AS\s+)?("?[A-Za-z_][A-Za-z0-9_]*"?))?',
        re.IGNORECASE,
    )
    for match in pattern.finditer(statement):
        table = _unquote_identifier(match.group(1))
        canonical = next(
            (name for name in schema if name.lower() == table.lower()), table
        )
        aliases[canonical.lower()] = canonical
        alias = _unquote_identifier(match.group(2) or "")
        if alias and alias.upper() not in SQL_KEYWORDS:
            aliases[alias.lower()] = canonical
    return aliases


def _extract_expression_columns(
    expression: str,
    aliases: dict[str, str],
    available: dict[str, set[str]],
    default_table: str | None,
) -> set[tuple[str, str]]:
    expression = _mask_string_literals(expression)
    references: set[tuple[str, str]] = set()
    qualified_pattern = re.compile(
        r'("?[A-Za-z_][A-Za-z0-9_]*"?)\s*\.\s*'
        r'("?[A-Za-z_][A-Za-z0-9_]*"?)'
    )
    occupied: list[tuple[int, int]] = []
    for match in qualified_pattern.finditer(expression):
        qualifier = _unquote_identifier(match.group(1))
        column = _unquote_identifier(match.group(2))
        table = aliases.get(qualifier.lower())
        if table is None:
            table = next(
                (name for name in available if name.lower() == qualifier.lower()),
                None,
            )
        if table is not None and column.lower() in {
            name.lower() for name in available.get(table, set())
        }:
            canonical_column = next(
                name for name in available[table] if name.lower() == column.lower()
            )
            references.add((table, canonical_column))
        occupied.append((match.start(), match.end()))

    masked = list(expression)
    for start, end in occupied:
        for index in range(start, end):
            masked[index] = " "
    remaining = "".join(masked)

    identifier_pattern = re.compile(r"\b([A-Za-z_][A-Za-z0-9_]*)\b")
    for match in identifier_pattern.finditer(remaining):
        name = match.group(1)
        upper = name.upper()
        if upper in SQL_KEYWORDS:
            continue
        after = match.end()
        while after < len(remaining) and remaining[after].isspace():
            after += 1
        if after < len(remaining) and remaining[after] == "(":
            continue
        lowered = name.lower()
        candidates = [
            table
            for table, columns in available.items()
            if any(column.lower() == lowered for column in columns)
        ]
        if not candidates:
            continue
        if default_table and default_table in candidates:
            chosen = default_table
        elif len(candidates) == 1:
            chosen = candidates[0]
        else:
            for candidate in candidates:
                canonical_column = next(
                    column
                    for column in available[candidate]
                    if column.lower() == lowered
                )
                references.add((candidate, canonical_column))
            continue
        canonical_column = next(
            column
            for column in available[chosen]
            if column.lower() == lowered
        )
        references.add((chosen, canonical_column))
    return references


def _parse_insert_mapping(
    statement: str,
    aliases: dict[str, str],
    available: dict[str, set[str]],
) -> tuple[str | None, set[str], dict[str, set[tuple[str, str]]]]:
    clean = strip_sql_comments(statement).strip()
    match = re.match(
        r'(?is)^INSERT\s+(?:OR\s+[A-Za-z_]+\s+)?INTO\s+'
        r'("?[A-Za-z_][A-Za-z0-9_]*"?)\s*',
        clean,
    )
    if not match:
        match = re.match(
            r'(?is)^REPLACE\s+INTO\s+'
            r'("?[A-Za-z_][A-Za-z0-9_]*"?)\s*',
            clean,
        )
    if not match:
        return None, set(), {}
    table = _unquote_identifier(match.group(1))
    rest = clean[match.end() :]
    columns: list[str] = []
    if rest.startswith("("):
        end = _matching_paren(rest, 0)
        if end > 0:
            columns = [
                _unquote_identifier(item)
                for item in _split_top_level(rest[1:end])
            ]
            rest = rest[end + 1 :].lstrip()
    select_position = _find_top_level_keyword(rest, "SELECT")
    if select_position < 0:
        return table, set(columns), {}
    select_text = rest[select_position + len("SELECT") :]
    from_position = _find_top_level_keyword(select_text, "FROM")
    if from_position >= 0:
        projection = select_text[:from_position]
    else:
        projection = select_text
    if projection.lstrip().upper().startswith("DISTINCT "):
        projection = projection.lstrip()[len("DISTINCT ") :]
    expressions = _split_top_level(projection)
    mapping: dict[str, set[tuple[str, str]]] = {}
    if columns and len(columns) == len(expressions):
        for column, expression in zip(columns, expressions):
            expression = re.sub(
                r'(?is)\s+AS\s+"?[A-Za-z_][A-Za-z0-9_]*"?\s*$',
                "",
                expression,
            )
            mapping[column] = _extract_expression_columns(
                expression, aliases, available, table
            )
    return table, set(columns), mapping


def _parse_update_mapping(
    statement: str,
    aliases: dict[str, str],
    available: dict[str, set[str]],
) -> tuple[str | None, set[str], dict[str, set[tuple[str, str]]]]:
    clean = strip_sql_comments(statement).strip()
    match = re.match(
        r'(?is)^UPDATE\s+("?[A-Za-z_][A-Za-z0-9_]*"?)\s+SET\s+',
        clean,
    )
    if not match:
        return None, set(), {}
    table = _unquote_identifier(match.group(1))
    body = clean[match.end() :]
    where_position = _find_top_level_keyword(body, "WHERE")
    if where_position >= 0:
        assignments_text = body[:where_position]
        where_text = body[where_position + len("WHERE") :]
    else:
        assignments_text = body
        where_text = ""
    mapping: dict[str, set[tuple[str, str]]] = {}
    columns: set[str] = set()
    for assignment in _split_top_level(assignments_text):
        equals = _find_top_level_keyword(assignment, "=")
        if equals < 0:
            equals = assignment.find("=")
        if equals < 0:
            continue
        column = _unquote_identifier(assignment[:equals].strip())
        expression = assignment[equals + 1 :].strip()
        columns.add(column)
        mapping[column] = _extract_expression_columns(
            expression + " " + where_text, aliases, available, table
        )
    return table, columns, mapping


def _unknown_reference_error(
    task_name: str, sql: str, error: sqlite3.OperationalError
) -> BusinessError | None:
    message = str(error)
    lowered = message.lower()
    if "no such table:" in lowered:
        object_name = message.split(":", 1)[1].strip()
        return BusinessError(
            "E_UNKNOWN_REF",
            f"Task {task_name} references an unknown table.",
            {
                "task_name": task_name,
                "object_type": "table",
                "object_name": object_name,
                "missing_object": object_name,
            },
        )
    if "no such column:" in lowered or "no such field:" in lowered:
        object_name = message.split(":", 1)[1].strip()
        return BusinessError(
            "E_UNKNOWN_REF",
            f"Task {task_name} references an unknown column.",
            {
                "task_name": task_name,
                "object_type": "column",
                "object_name": object_name,
                "missing_object": object_name,
            },
        )
    return None


def parse_task_sql(
    metadata: WarehouseMetadata, task: TaskLineage
) -> TaskLineage:
    conn = sqlite3.connect(":memory:")
    try:
        for table_name, table in metadata.schema.items():
            definitions = ", ".join(
                f"{quote_identifier(name)} {data_type}"
                for name, data_type in table["columns"].items()
            )
            conn.execute(
                f"CREATE TABLE {quote_identifier(table_name)} ({definitions})"
            )

        statements = split_sql_statements(task.sql)
        if not statements:
            raise BusinessError(
                "E_INVALID_TASK_SQL",
                f"Task {task.task_name} has no SQL statement.",
                {"task_name": task.task_name, "object_type": "sql"},
            )

        seen_events: list[tuple[int, str | None, str | None]] = []

        def authorizer(
            action: int,
            arg1: str | None,
            arg2: str | None,
            database: str | None,
            trigger: str | None,
        ) -> int:
            if action in {
                sqlite3.SQLITE_READ,
                sqlite3.SQLITE_INSERT,
                sqlite3.SQLITE_UPDATE,
                sqlite3.SQLITE_DELETE,
            }:
                seen_events.append((action, arg1, arg2))
            return sqlite3.SQLITE_OK

        conn.set_authorizer(authorizer)
        reads: dict[str, set[str]] = defaultdict(set)
        writes: dict[str, set[str]] = defaultdict(set)
        written_tables: set[str] = set()
        insert_columns_by_table: dict[str, set[str]] = defaultdict(set)
        update_columns_by_table: dict[str, set[str]] = defaultdict(set)
        insert_mapping: dict[str, set[tuple[str, str]]] = {}
        update_mapping: dict[str, set[tuple[str, str]]] = {}

        for statement in statements:
            first_word = statement.lstrip().split(None, 1)[0].upper()
            if first_word in {"BEGIN", "COMMIT", "ROLLBACK", "PRAGMA"}:
                continue
            seen_events.clear()
            try:
                conn.execute("EXPLAIN " + statement)
            except sqlite3.OperationalError as error:
                unknown = _unknown_reference_error(
                    task.task_name, statement, error
                )
                if unknown is not None:
                    raise unknown from error
                raise BusinessError(
                    "E_INVALID_TASK_SQL",
                    f"Task {task.task_name} contains invalid SQL.",
                    {
                        "task_name": task.task_name,
                        "object_type": "sql",
                        "reason": str(error),
                    },
                ) from error
            for action, table_name, column_name in seen_events:
                if not table_name or table_name.startswith("sqlite_"):
                    continue
                canonical_table = next(
                    (
                        name
                        for name in metadata.schema
                        if name.lower() == table_name.lower()
                    ),
                    table_name,
                )
                if action == sqlite3.SQLITE_READ:
                    reads.setdefault(canonical_table, set())
                    if column_name:
                        canonical_column = next(
                            (
                                name
                                for name in metadata.schema.get(
                                    canonical_table, {}
                                ).get("columns", {})
                                if name.lower() == column_name.lower()
                            ),
                            column_name,
                        )
                        reads[canonical_table].add(canonical_column)
                elif action == sqlite3.SQLITE_UPDATE:
                    written_tables.add(canonical_table)
                    if column_name:
                        writes[canonical_table].add(column_name)
                        update_columns_by_table[canonical_table].add(column_name)
                elif action == sqlite3.SQLITE_INSERT:
                    written_tables.add(canonical_table)
                elif action == sqlite3.SQLITE_DELETE:
                    written_tables.add(canonical_table)

            clean = strip_sql_comments(statement)
            available = {
                table: set(data["columns"])
                for table, data in metadata.schema.items()
            }
            aliases = _table_aliases(clean, metadata.schema)
            insert_table, insert_cols, parsed_insert = _parse_insert_mapping(
                clean, aliases, available
            )
            if insert_table:
                insert_columns_by_table[insert_table].update(insert_cols)
                insert_mapping.update(parsed_insert)
            update_table, update_cols, parsed_update = _parse_update_mapping(
                clean, aliases, available
            )
            if update_table:
                update_columns_by_table[update_table].update(update_cols)
                update_mapping.update(parsed_update)

        output_lower = task.output_table.lower()
        canonical_output = next(
            (
                name
                for name in metadata.schema
                if name.lower() == output_lower
            ),
            None,
        )
        if canonical_output is None:
            raise BusinessError(
                "E_UNKNOWN_REF",
                f"Task {task.task_name} has an unknown output table.",
                {
                    "task_name": task.task_name,
                    "object_type": "table",
                    "object_name": task.output_table,
                    "missing_object": task.output_table,
                },
            )
        task.output_table = canonical_output
        if canonical_output not in written_tables:
            raise BusinessError(
                "E_INVALID_TASK_SQL",
                f"Task {task.task_name} does not write its declared output table.",
                {
                    "task_name": task.task_name,
                    "object_type": "output_table",
                    "object_name": canonical_output,
                },
            )

        explicit_insert = insert_columns_by_table.get(canonical_output, set())
        explicit_update = update_columns_by_table.get(canonical_output, set())
        writes.setdefault(canonical_output, set())
        writes[canonical_output].update(explicit_insert)
        writes[canonical_output].update(explicit_update)
        if not writes[canonical_output]:
            writes[canonical_output] = set(
                metadata.schema[canonical_output]["columns"]
            )

        task.reads = {
            table: set(columns) for table, columns in reads.items()
        }
        task.writes = {
            table: set(columns) for table, columns in writes.items() if columns
        }
        task.read_columns = {
            (table, column)
            for table, columns in task.reads.items()
            for column in columns
        }
        task.write_columns = set(task.writes.get(canonical_output, set()))

        all_reads = set(task.read_columns)
        column_map: dict[tuple[str, str], set[tuple[str, str]]] = {}
        for column in task.write_columns:
            sources = insert_mapping.get(column, set()) | update_mapping.get(
                column, set()
            )
            sources = {
                source
                for source in sources
                if source != (canonical_output, column)
                and source in all_reads
            }
            if not sources:
                sources = set(all_reads)
            column_map[(canonical_output, column)] = sources
        task.column_map = column_map
        return task
    finally:
        conn.close()


def _stable_topological(
    nodes: Iterable[str],
    edges: dict[str, set[str]],
    order_key,
) -> tuple[list[str], dict[str, int], list[list[str]]]:
    node_set = set(nodes)
    indegree = {node: 0 for node in node_set}
    for source, targets in edges.items():
        if source not in node_set:
            continue
        for target in targets:
            if target in node_set:
                indegree[target] += 1
    heap = [
        (order_key(node), node) for node, degree in indegree.items() if degree == 0
    ]
    heapq.heapify(heap)
    result: list[str] = []
    depth = {node: 0 for node in node_set}
    while heap:
        _, node = heapq.heappop(heap)
        result.append(node)
        for target in sorted(edges.get(node, set())):
            if target not in node_set:
                continue
            depth[target] = max(depth[target], depth[node] + 1)
            indegree[target] -= 1
            if indegree[target] == 0:
                heapq.heappush(heap, (order_key(target), target))
    cycles: list[list[str]] = []
    if len(result) != len(node_set):
        remaining = node_set - set(result)
        cycles = _find_cycles(remaining, edges)
        next_depth = max(depth.values(), default=0) + 1
        for node in sorted(remaining, key=order_key):
            depth[node] = next_depth
    return result, depth, cycles


def _find_cycles(nodes: set[str], edges: dict[str, set[str]]) -> list[list[str]]:
    state: dict[str, int] = {}
    stack: list[str] = []
    positions: dict[str, int] = {}
    found: dict[tuple[str, ...], list[str]] = {}

    def canonical(cycle: list[str]) -> tuple[str, ...]:
        if not cycle:
            return tuple()
        rotations = [
            tuple(cycle[index:] + cycle[:index])
            for index in range(len(cycle))
        ]
        return min(rotations)

    def visit(node: str) -> None:
        state[node] = 1
        positions[node] = len(stack)
        stack.append(node)
        for target in sorted(edges.get(node, set())):
            if target not in nodes:
                continue
            target_state = state.get(target, 0)
            if target_state == 0:
                visit(target)
            elif target_state == 1:
                cycle = stack[positions[target] :]
                found[canonical(cycle)] = cycle
        stack.pop()
        positions.pop(node, None)
        state[node] = 2

    for node in sorted(nodes):
        if state.get(node, 0) == 0:
            visit(node)
    return [list(key) for key in sorted(found)]


def load_warehouse_metadata(conn: sqlite3.Connection) -> WarehouseMetadata:
    try:
        keys = {
            row["key"]: row["value"]
            for row in conn.execute("SELECT key, value FROM meta_info")
        }
        snapshot_id = keys.get("snapshot_id", "baseline")
        schema_rows = list(
            conn.execute(
                "SELECT table_name, layer, description, row_count "
                "FROM schema_tables WHERE snapshot_id = ? "
                "ORDER BY table_name",
                (snapshot_id,),
            )
        )
        if not schema_rows:
            raise BusinessError(
                "E_METADATA_INVALID",
                "The metadata database has no table schema snapshot.",
                {"object_type": "schema_snapshot"},
            )
        schema: OrderedDict[str, dict[str, Any]] = OrderedDict()
        for row in schema_rows:
            schema[row["table_name"]] = {
                "layer": row["layer"],
                "description": row["description"],
                "row_count": int(row["row_count"]),
                "columns": OrderedDict(),
            }
        for row in conn.execute(
            "SELECT table_name, column_name, data_type, ordinal_position, nullable "
            "FROM schema_columns WHERE snapshot_id = ? "
            "ORDER BY table_name, ordinal_position",
            (snapshot_id,),
        ):
            if row["table_name"] not in schema:
                raise BusinessError(
                    "E_METADATA_INVALID",
                    "A schema column refers to an unknown snapshot table.",
                    {
                        "object_type": "table",
                        "object_name": row["table_name"],
                    },
                )
            schema[row["table_name"]]["columns"][row["column_name"]] = row[
                "data_type"
            ]

        tasks: list[TaskLineage] = []
        for row in conn.execute(
            "SELECT task_name, task_order, output_table, description, task_sql, "
            "last_run_id, last_run_status, last_run_started_at, "
            "last_run_finished_at, last_run_rows "
            "FROM tasks ORDER BY task_order"
        ):
            tasks.append(
                TaskLineage(
                    task_name=row["task_name"],
                    task_order=int(row["task_order"]),
                    description=row["description"],
                    sql=row["task_sql"],
                    output_table=row["output_table"],
                    last_run={
                        "run_id": row["last_run_id"],
                        "status": row["last_run_status"],
                        "started_at": row["last_run_started_at"],
                        "finished_at": row["last_run_finished_at"],
                        "output_rows": int(row["last_run_rows"]),
                    },
                )
            )
        if not tasks:
            raise BusinessError(
                "E_METADATA_INVALID",
                "The metadata database has no task definitions.",
                {"object_type": "task"},
            )

        reports = [
            dict(row)
            for row in conn.execute(
                "SELECT report_id, report_name, owner, task_name, description "
                "FROM report_task_map ORDER BY report_id, task_name"
            )
        ]
        metrics = [
            dict(row)
            for row in conn.execute(
                "SELECT metric_id, metric_name, source_table, query_sql, "
                "threshold_pct, description FROM metric_defs "
                "ORDER BY metric_id"
            )
        ]
        for metric in metrics:
            metric["threshold_pct"] = float(metric["threshold_pct"])
        metadata = WarehouseMetadata(
            snapshot_id=snapshot_id,
            schema=schema,
            tasks=tasks,
            reports=reports,
            metrics=metrics,
        )
    except sqlite3.DatabaseError as error:
        raise BusinessError(
            "E_METADATA_INVALID",
            "Unable to read the SQLite metadata schema.",
            {"object_type": "metadata_database"},
        ) from error

    known_tasks = {task.task_name for task in tasks}
    for report in reports:
        if report["task_name"] not in known_tasks:
            raise BusinessError(
                "E_METADATA_INVALID",
                "A report mapping refers to an unknown task.",
                {
                    "object_type": "task",
                    "object_name": report["task_name"],
                },
            )

    parsed_tasks = []
    for task in tasks:
        parsed_tasks.append(parse_task_sql(metadata, task))
    metadata.tasks = parsed_tasks

    table_edges: dict[str, set[str]] = {table: set() for table in schema}
    for task in metadata.tasks:
        for input_table in task.reads:
            if input_table != task.output_table:
                table_edges.setdefault(input_table, set()).add(task.output_table)
        table_edges.setdefault(task.output_table, set())
    metadata.table_dependencies = table_edges
    _, table_depths, _ = _stable_topological(
        schema.keys(), table_edges, lambda name: list(schema).index(name)
    )
    metadata.table_depths = table_depths

    producers: dict[str, list[TaskLineage]] = defaultdict(list)
    for task in metadata.tasks:
        producers[task.output_table].append(task)
    task_edges: dict[str, set[str]] = {
        task.task_name: set() for task in metadata.tasks
    }
    task_by_name = metadata.task_by_name
    for task in metadata.tasks:
        for input_table in task.reads:
            for producer in producers.get(input_table, []):
                if producer.task_name == task.task_name:
                    continue
                if (
                    input_table == task.output_table
                    and producer.task_order >= task.task_order
                ):
                    continue
                task_edges[producer.task_name].add(task.task_name)
                task.dependencies.add(producer.task_name)
    metadata.task_graph = task_edges
    _, task_depths, _ = _stable_topological(
        task_by_name.keys(),
        task_edges,
        lambda name: task_by_name[name].task_order,
    )
    for task in metadata.tasks:
        task.depth = task_depths.get(task.task_name, 0)

    metric_conn = sqlite3.connect(":memory:")
    try:
        for table_name, table in schema.items():
            definitions = ", ".join(
                f"{quote_identifier(name)} {data_type}"
                for name, data_type in table["columns"].items()
            )
            metric_conn.execute(
                f"CREATE TABLE {quote_identifier(table_name)} ({definitions})"
            )
        for metric in metrics:
            try:
                metric_conn.execute("EXPLAIN " + metric["query_sql"])
            except sqlite3.OperationalError as error:
                unknown = _unknown_reference_error(
                    metric["metric_id"], metric["query_sql"], error
                )
                if unknown is not None:
                    raise unknown from error
                raise BusinessError(
                    "E_METADATA_INVALID",
                    "A metric query is invalid.",
                    {
                        "object_type": "metric",
                        "object_name": metric["metric_id"],
                    },
                ) from error
    finally:
        metric_conn.close()
    return metadata


def _canonical_table_closure(
    metadata: WarehouseMetadata, table_name: str
) -> set[str]:
    result = {table_name}
    queue = deque([table_name])
    while queue:
        current = queue.popleft()
        for upstream, targets in metadata.table_dependencies.items():
            if current in targets and upstream not in result:
                result.add(upstream)
                queue.append(upstream)
    return result


def _path_between_tables(
    metadata: WarehouseMetadata, source: str, target: str
) -> list[str]:
    if source == target:
        return [source]
    queue = deque([[source]])
    visited = {source}
    while queue:
        path = queue.popleft()
        for next_table in sorted(metadata.table_dependencies.get(path[-1], set())):
            if next_table in visited:
                continue
            if next_table == target:
                return path + [next_table]
            visited.add(next_table)
            queue.append(path + [next_table])
    return [source, target] if source != target else [source]


def _table_fingerprint(
    conn: sqlite3.Connection, table: str, columns: OrderedDict[str, str]
) -> dict[str, Any]:
    select_columns = ", ".join(quote_identifier(name) for name in columns)
    digest = hashlib.sha256()
    row_count = 0
    try:
        cursor = conn.execute(
            f"SELECT {select_columns} FROM {quote_identifier(table)} ORDER BY rowid"
        )
        for row in cursor:
            row_count += 1
            digest.update(
                json.dumps(
                    list(row),
                    ensure_ascii=False,
                    separators=(",", ":"),
                    default=str,
                ).encode("utf-8")
            )
            digest.update(b"\n")
    except sqlite3.DatabaseError:
        return {"hash": None, "row_count": None, "readable": False}
    return {"hash": digest.hexdigest(), "row_count": row_count, "readable": True}


def _number(value: Any) -> float:
    if value is None:
        return 0.0
    number = float(value)
    if not math.isfinite(number):
        return 0.0
    return number


def _metric_value(conn: sqlite3.Connection, query_sql: str) -> float:
    row = conn.execute(query_sql).fetchone()
    if row is None:
        return 0.0
    return _number(row[0])


class LineageEngine:
    """Column-level lineage and deterministic affected-change analysis."""

    def __init__(self, metadata: WarehouseMetadata, source_db_path: str) -> None:
        self.metadata = metadata
        self.source_db_path = source_db_path
        self.task_by_name = metadata.task_by_name
        self.snapshot_fingerprint = hashlib.sha256(
            json.dumps(
                {
                    "snapshot_id": metadata.snapshot_id,
                    "schema": {
                        table: list(data["columns"].items())
                        for table, data in metadata.schema.items()
                    },
                    "tasks": [
                        (task.task_name, task.task_order, task.sql)
                        for task in metadata.tasks
                    ],
                },
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
        ).hexdigest()

    def resolve_change(
        self, table_name: str, column_name: str
    ) -> tuple[str, str]:
        canonical_table = self.metadata.find_table(table_name)
        if canonical_table is None:
            raise BusinessError(
                "E_UNKNOWN_TABLE",
                "The requested table does not exist.",
                {"object_type": "table", "object_name": table_name},
            )
        canonical_column = self.metadata.find_column(
            canonical_table, column_name
        )
        if canonical_column is None:
            raise BusinessError(
                "E_UNKNOWN_COLUMN",
                "The requested column does not exist.",
                {
                    "object_type": "column",
                    "object_name": column_name,
                    "table_name": canonical_table,
                },
            )
        return canonical_table, canonical_column

    def compute_impact(
        self, change: dict[str, Any]
    ) -> dict[str, Any]:
        target = (change["table_name"], change["column_name"])
        direct_tasks: set[str] = set()
        affected_tasks: set[str] = set()
        tainted: set[tuple[str, str]] = {target}
        node_parent: dict[tuple[str, str], tuple[str, str] | None] = {
            target: None
        }
        task_triggers: dict[str, set[tuple[str, str]]] = defaultdict(set)

        for task in self.metadata.tasks:
            if target in task.read_columns:
                direct_tasks.add(task.task_name)
                affected_tasks.add(task.task_name)
                task_triggers[task.task_name].add(target)

        def path_to_origin(node: tuple[str, str]) -> list[str]:
            path: list[tuple[str, str]] = []
            current: tuple[str, str] | None = node
            seen: set[tuple[str, str]] = set()
            while current is not None and current not in seen:
                seen.add(current)
                path.append(current)
                current = node_parent.get(current)
            path.reverse()
            return [f"{table}.{column}" for table, column in path]

        max_rounds = max(4, len(self.metadata.tasks) * 3)
        for _ in range(max_rounds):
            changed = False
            for task in sorted(
                self.metadata.tasks, key=lambda item: item.task_order
            ):
                triggers = {
                    node for node in task.read_columns if node in tainted
                }
                if not triggers:
                    continue
                affected_tasks.add(task.task_name)
                task_triggers[task.task_name].update(triggers)
                output_table = task.output_table
                propagated_for_task = False
                for output_column in task.output_columns:
                    output_node = (output_table, output_column)
                    sources = task.column_map.get(output_node, set())
                    touched = sorted(
                        source for source in sources if source in tainted
                    )
                    if not touched:
                        continue
                    propagated_for_task = True
                    source = touched[0]
                    if output_node not in tainted:
                        tainted.add(output_node)
                        node_parent[output_node] = source
                        changed = True
                if not propagated_for_task:
                    source = sorted(triggers)[0]
                    for output_column in task.output_columns:
                        output_node = (output_table, output_column)
                        if output_node not in tainted:
                            tainted.add(output_node)
                            node_parent[output_node] = source
                            changed = True
            if not changed:
                break

        affected_rows: list[dict[str, Any]] = []
        for task in sorted(
            self.metadata.tasks,
            key=lambda item: (item.depth, item.task_order),
        ):
            if task.task_name not in affected_tasks:
                continue
            is_direct = task.task_name in direct_tasks
            triggers = sorted(task_triggers.get(task.task_name, set()))
            paths = [path_to_origin(trigger) for trigger in triggers]
            affected_rows.append(
                {
                    "task_name": task.task_name,
                    "task_order": task.task_order,
                    "output_table": task.output_table,
                    "depth": task.depth,
                    "impact_type": (
                        "direct" if is_direct else "transitive"
                    ),
                    "reason": (
                        f"reads changed column {target[0]}.{target[1]}"
                        if is_direct
                        else "receives column lineage from an affected table"
                    ),
                    "trigger_columns": [
                        f"{table}.{column}" for table, column in triggers
                    ],
                    "lineage_paths": paths,
                }
            )

        direct_count = sum(
            1 for row in affected_rows if row["impact_type"] == "direct"
        )
        transitive_count = len(affected_rows) - direct_count
        affected_names = {row["task_name"] for row in affected_rows}
        affected_output_tables = {
            row["output_table"] for row in affected_rows
        }

        report_groups: dict[str, dict[str, Any]] = {}
        for mapping in self.metadata.reports:
            mapped_task = self.task_by_name.get(mapping["task_name"])
            mapped_table = mapped_task.output_table if mapped_task else None
            if (
                mapping["task_name"] not in affected_names
                and mapped_table not in affected_output_tables
            ):
                continue
            report = report_groups.setdefault(
                mapping["report_id"],
                {
                    "report_id": mapping["report_id"],
                    "report_name": mapping["report_name"],
                    "owner": mapping["owner"],
                    "description": mapping["description"],
                    "via_tasks": [],
                },
            )
            report["via_tasks"].append(mapping["task_name"])
        reports = sorted(
            report_groups.values(),
            key=lambda item: item["report_id"],
        )
        for report in reports:
            report["via_tasks"] = sorted(set(report["via_tasks"]))

        rollback_tasks = sorted(
            affected_rows,
            key=lambda row: (-row["depth"], -row["task_order"]),
        )
        rollback_order = [
            {
                "rollback_step": index,
                "task_name": row["task_name"],
                "task_order": row["task_order"],
                "output_table": row["output_table"],
                "dependency_depth": row["depth"],
                "impact_type": row["impact_type"],
            }
            for index, row in enumerate(rollback_tasks, 1)
        ]

        return {
            "algorithm_version": "column-lineage-v1",
            "snapshot_fingerprint": self.snapshot_fingerprint,
            "change": change,
            "summary": {
                "affected_task_count": len(affected_rows),
                "direct_count": direct_count,
                "transitive_count": transitive_count,
                "affected_report_count": len(reports),
            },
            "affected_tasks": affected_rows,
            "affected_reports": reports,
            "rollback_order": rollback_order,
        }

    def _clone_connection(self, source: sqlite3.Connection) -> sqlite3.Connection:
        clone = sqlite3.connect(":memory:")
        source.backup(clone)
        clone.row_factory = sqlite3.Row
        return clone

    def _run_tasks(
        self,
        conn: sqlite3.Connection,
        change: dict[str, Any] | None = None,
    ) -> list[dict[str, str]]:
        errors: list[dict[str, str]] = []
        target_table = change["table_name"] if change else None
        target_column = change["column_name"] if change else None
        for task in sorted(
            self.metadata.tasks,
            key=lambda item: (item.depth, item.task_order),
        ):
            if (
                change
                and task.output_table == target_table
                and target_column in task.write_columns
            ):
                continue
            try:
                conn.executescript(task.sql)
                conn.commit()
            except sqlite3.DatabaseError as error:
                errors.append(
                    {
                        "task_name": task.task_name,
                        "reason": str(error),
                    }
                )
        return errors

    def _apply_change(self, conn: sqlite3.Connection, change: dict[str, Any]) -> None:
        table = quote_identifier(change["table_name"])
        column = quote_identifier(change["column_name"])
        if change["change_type"] == "delete_column":
            conn.execute(f"UPDATE {table} SET {column} = NULL")
            return
        new_type = str(change.get("new_type") or "").upper()
        expression = column
        text_width = re.match(r"^(?:TEXT|VARCHAR|CHAR)\s*\(\s*(\d+)\s*\)", new_type)
        decimal_scale = re.match(
            r"^(?:REAL|NUMERIC|DECIMAL|DOUBLE)\s*\(\s*\d+\s*,\s*(\d+)\s*\)",
            new_type,
        )
        if text_width:
            expression = f"substr(CAST({column} AS TEXT), 1, {int(text_width.group(1))})"
        elif decimal_scale:
            expression = f"ROUND({column}, {int(decimal_scale.group(1))})"
        elif re.match(r"^(?:INTEGER|SMALLINT|BIGINT|INT)\b", new_type):
            expression = f"CAST(ROUND({column}, 0) AS INTEGER)"
        conn.execute(f"UPDATE {table} SET {column} = {expression}")

    def compute_drift(
        self, change: dict[str, Any], threshold_pct: float
    ) -> dict[str, Any]:
        source = sqlite3.connect(":memory:")
        backup_conn = sqlite3.connect(":memory:")
        try:
            self_source = sqlite3.connect(self.source_db_path)
            try:
                self_source.backup(source)
            finally:
                self_source.close()
            source.row_factory = sqlite3.Row
            source.backup(backup_conn)
            before = backup_conn
            before.row_factory = sqlite3.Row
            after = source
            after.row_factory = sqlite3.Row

            before_errors = self._run_tasks(before)
            self._apply_change(after, change)
            after_errors = self._run_tasks(after, change=change)
            before.commit()
            after.commit()

            before_fingerprints = {
                table: _table_fingerprint(
                    before, table, data["columns"]
                )
                for table, data in self.metadata.schema.items()
            }
            after_fingerprints = {
                table: _table_fingerprint(after, table, data["columns"])
                for table, data in self.metadata.schema.items()
            }

            metric_results: list[dict[str, Any]] = []
            for metric in self.metadata.metrics:
                before_value = _metric_value(before, metric["query_sql"])
                after_value = _metric_value(after, metric["query_sql"])
                absolute = abs(after_value - before_value)
                if before_value == 0:
                    relative = 0.0 if after_value == 0 else 100.0
                else:
                    relative = absolute / abs(before_value) * 100.0
                status = "E_DRIFT" if relative > threshold_pct else "OK"
                result: dict[str, Any] = {
                    "metric_id": metric["metric_id"],
                    "metric_name": metric["metric_name"],
                    "source_table": metric["source_table"],
                    "before": round(before_value, 6),
                    "after": round(after_value, 6),
                    "absolute_drift": round(absolute, 6),
                    "relative_deviation_pct": round(relative, 6),
                    "threshold_pct": threshold_pct,
                    "status": status,
                }
                if status == "E_DRIFT":
                    closure = _canonical_table_closure(
                        self.metadata, metric["source_table"]
                    )
                    changed_tables = sorted(
                        table
                        for table in closure
                        if before_fingerprints.get(table)
                        != after_fingerprints.get(table)
                    )
                    candidates = [
                        table
                        for table in changed_tables
                        if self.metadata.schema[table]["layer"] != "raw"
                    ] or changed_tables
                    first_table = (
                        min(
                            candidates,
                            key=lambda table: (
                                self.metadata.table_depths.get(table, 0),
                                table,
                            ),
                        )
                        if candidates
                        else change["table_name"]
                    )
                    result["first_drift_table"] = first_table
                    result["first_drift_layer"] = self.metadata.schema[
                        first_table
                    ]["layer"]
                    result["lineage_trace"] = _path_between_tables(
                        self.metadata,
                        first_table,
                        metric["source_table"],
                    )
                    result["drift_tables"] = changed_tables
                else:
                    result["first_drift_table"] = None
                    result["lineage_trace"] = []
                metric_results.append(result)

            drifted = [
                result
                for result in metric_results
                if result["status"] == "E_DRIFT"
            ]
            return {
                "algorithm_version": "deterministic-replay-v1",
                "snapshot_fingerprint": self.snapshot_fingerprint,
                "change": change,
                "threshold_pct": threshold_pct,
                "status": "E_DRIFT" if drifted else "OK",
                "has_drift": bool(drifted),
                "summary": {
                    "metrics_checked": len(metric_results),
                    "drifted_count": len(drifted),
                },
                "drifted_metrics": [
                    result["metric_id"] for result in drifted
                ],
                "metric_results": metric_results,
                "replay": {
                    "before_errors": before_errors,
                    "after_errors": after_errors,
                },
            }
        finally:
            source.close()
            backup_conn.close()

    def find_rollback_cycles(
        self, task_names: set[str]
    ) -> list[list[str]]:
        return _find_cycles(task_names, self.metadata.task_graph)

    def recheck_metrics_for_tasks(
        self, task_names: set[str]
    ) -> list[dict[str, Any]]:
        output_tables = {
            self.task_by_name[name].output_table
            for name in task_names
            if name in self.task_by_name
        }
        result = []
        for metric in self.metadata.metrics:
            closure = _canonical_table_closure(
                self.metadata, metric["source_table"]
            )
            if output_tables & closure or metric["source_table"] in output_tables:
                result.append(
                    {
                        "metric_id": metric["metric_id"],
                        "metric_name": metric["metric_name"],
                        "source_table": metric["source_table"],
                    }
                )
        return result
