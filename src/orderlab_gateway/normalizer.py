from __future__ import annotations

import re
from dataclasses import dataclass, field

from .config import GatewayConfig
from .sqltext import (
    Token,
    mask_comments_and_strings,
    replace_spans,
    tokenize,
)


IDENT = r"(?:\[[^\]]+\]|[A-Za-z_][A-Za-z0-9_]*)(?:\s*\.\s*(?:\[[^\]]+\]|[A-Za-z_][A-Za-z0-9_]*))?"


@dataclass(slots=True)
class NormalizedQuery:
    sql: str
    dialect: str
    notes: list[str] = field(default_factory=list)


def normalize_query(sql: str, dialect: str, config: GatewayConfig) -> NormalizedQuery:
    notes: list[str] = []
    normalized = _strip_query_hints(sql, notes)
    normalized = _strip_table_hints(normalized, config, notes)
    normalized = _normalize_date_truncation(normalized, notes)
    normalized = _normalize_string_numeric(normalized, config, notes)
    normalized = _normalize_null_ordering(normalized, config, notes)
    return NormalizedQuery(sql=normalized, dialect=dialect, notes=notes)


def _strip_query_hints(sql: str, notes: list[str]) -> str:
    masked = mask_comments_and_strings(sql)
    replacements: list[tuple[int, int, str]] = []
    for match in re.finditer(r"\bOPTION\s*\(", masked, flags=re.IGNORECASE):
        open_index = masked.find("(", match.start())
        depth = 0
        close_index = -1
        for index in range(open_index, len(masked)):
            if masked[index] == "(":
                depth += 1
            elif masked[index] == ")":
                depth -= 1
                if depth == 0:
                    close_index = index + 1
                    break
        if close_index > 0:
            while close_index < len(sql) and sql[close_index] in " \t\r\n;":
                close_index += 1
            replacements.append((match.start(), close_index, ""))
    if replacements:
        notes.append("query-level OPTION hints removed")
        return replace_spans(sql, replacements).rstrip()
    return sql


def _strip_table_hints(sql: str, config: GatewayConfig, notes: list[str]) -> str:
    table_names = "|".join(re.escape(table.name) for table in config.tables.values())
    if not table_names:
        return sql
    masked = mask_comments_and_strings(sql)
    identifier_pattern = r"(?:\[[^\]]+\]|[A-Za-z_][A-Za-z0-9_]*)"
    pattern = (
        rf"\b(?:FROM|JOIN)\s+(?:{identifier_pattern}\s*\.\s*)?"
        rf"(?:\[?(?:{table_names})\]?)"
        rf"(?:\s+(?:AS\s+)?{identifier_pattern})?"
        r"\s+(?P<hint>\bWITH\s*\()"
    )
    replacements: list[tuple[int, int, str]] = []
    for match in re.finditer(pattern, masked, flags=re.IGNORECASE):
        open_index = masked.find("(", match.start("hint"))
        depth = 0
        close_index = -1
        for index in range(open_index, len(masked)):
            if masked[index] == "(":
                depth += 1
            elif masked[index] == ")":
                depth -= 1
                if depth == 0:
                    close_index = index + 1
                    break
        if close_index > 0:
            replacements.append((match.start("hint"), close_index, ""))
    if replacements:
        notes.append("table hints removed")
        return re.sub(r"[ \t]{2,}", " ", replace_spans(sql, replacements))
    return sql


def _regex_replace(
    sql: str,
    pattern: str,
    builder,
    notes: list[str] | None = None,
    note: str | None = None,
) -> str:
    masked = mask_comments_and_strings(sql)
    replacements: list[tuple[int, int, str]] = []
    for match in re.finditer(pattern, masked, flags=re.IGNORECASE):
        start, end = match.span()
        if any(not (end <= old_start or start >= old_end) for old_start, old_end, _ in replacements):
            continue
        replacement = builder(match, sql)
        if replacement is None:
            continue
        if sql[start:end] != replacement:
            replacements.append((start, end, replacement))
            if notes is not None and note:
                notes.append(note)
    return replace_spans(sql, replacements)


def _normalize_date_truncation(sql: str, notes: list[str]) -> str:
    legacy_second = (
        rf"\bDATEADD\s*\(\s*(?:second|ss|s)\s*,\s*"
        rf"DATEDIFF\s*\(\s*(?:second|ss|s)\s*,\s*0\s*,\s*({IDENT})\s*\)\s*,\s*0\s*\)"
    )
    sql = _regex_replace(
        sql,
        legacy_second,
        _second_replacement,
        notes,
        "legacy second truncation normalized",
    )
    legacy_day = (
        rf"\bDATEADD\s*\(\s*(?:day|dd|d)\s*,\s*"
        rf"DATEDIFF\s*\(\s*(?:day|dd|d)\s*,\s*0\s*,\s*({IDENT})\s*\)\s*,\s*0\s*\)"
    )
    sql = _regex_replace(
        sql,
        legacy_day,
        lambda match, _sql: f"CAST({match.group(1)} AS date)",
        notes,
        "legacy day truncation normalized",
    )
    convert_date = rf"\bCONVERT\s*\(\s*date\s*,\s*({IDENT})\s*(?:,\s*\d+\s*)?\)"
    sql = _regex_replace(
        sql,
        convert_date,
        lambda match, _sql: f"CAST({match.group(1)} AS date)",
        notes,
        "CONVERT(date, ...) normalized to CAST",
    )
    second_pattern = (
        rf"\bDATETRUNC\s*\(\s*(?:second|ss|s)\s*,\s*({IDENT})\s*\)"
    )
    sql = _regex_replace(
        sql,
        second_pattern,
        _second_replacement,
        notes,
        "date truncation to second normalized",
    )
    day_pattern = rf"\bDATETRUNC\s*\(\s*(?:day|dd|d)\s*,\s*({IDENT})\s*\)"
    sql = _regex_replace(
        sql,
        day_pattern,
        lambda match, _sql: f"CAST({match.group(1)} AS date)",
        notes,
        "date truncation to day normalized",
    )
    return sql


def _second_replacement(match: re.Match[str], sql: str) -> str | None:
    return f"DATETRUNC(second, {match.group(1)})"


def _normalize_string_numeric(sql: str, config: GatewayConfig, notes: list[str]) -> str:
    convert_pattern = (
        rf"\b(?:TRY_CONVERT|CONVERT)\s*\(\s*(?:bigint|int)\s*,\s*({IDENT})\s*\)"
        r"\s*(=|<>|!=|<=|>=|<|>)\s*([+-]?\d+)"
    )
    sql = _regex_replace(
        sql,
        convert_pattern,
        lambda match, _sql: (
            f"TRY_CONVERT(bigint, {match.group(1)}) {match.group(2)} {match.group(3)}"
        ),
        notes,
        "string-to-number comparison normalized with TRY_CONVERT",
    )
    cast_pattern = (
        rf"\bCAST\s*\(\s*({IDENT})\s+AS\s+(?:bigint|int)\s*\)"
        r"\s*(=|<>|!=|<=|>=|<|>)\s*([+-]?\d+)"
    )
    sql = _regex_replace(
        sql,
        cast_pattern,
        lambda match, _sql: (
            f"TRY_CONVERT(bigint, {match.group(1)}) {match.group(2)} {match.group(3)}"
        ),
        notes,
        "explicit string cast normalized with TRY_CONVERT",
    )

    string_columns = {column.lower() for column in config.string_columns}
    bare_pattern = (
        rf"({IDENT})\s*(=|<>|!=|<=|>=|<|>)\s*([+-]?\d+(?:\.\d+)?)"
        r"(?![\w.])"
    )

    def bare_builder(match: re.Match[str], _sql: str) -> str | None:
        expression = match.group(1)
        leaf = expression.split(".")[-1].strip().strip("[]").lower()
        if leaf not in string_columns:
            return None
        return (
            f"TRY_CONVERT(bigint, {expression}) {match.group(2)} {match.group(3)}"
        )

    sql = _regex_replace(
        sql,
        bare_pattern,
        bare_builder,
        notes,
        "implicit string/numeric comparison made explicit",
    )
    return sql


def _normalize_null_ordering(sql: str, config: GatewayConfig, notes: list[str]) -> str:
    masked = mask_comments_and_strings(sql)
    tokens = tokenize(masked)
    order_index = -1
    for index, token in enumerate(tokens[:-1]):
        if (
            token.kind == "word"
            and token.lower == "order"
            and token.depth == 0
            and tokens[index + 1].kind == "word"
            and tokens[index + 1].lower == "by"
        ):
            order_index = index + 1
    if order_index < 0:
        return sql

    region_start = tokens[order_index].end
    region_end = len(sql)
    stop_words = {"offset", "for", "option"}
    for token in tokens[order_index + 1 :]:
        if token.depth == 0 and (
            (token.kind == "word" and token.lower in stop_words)
            or (token.kind == "punct" and token.text == ";")
        ):
            region_end = token.start
            break

    items: list[tuple[int, int]] = []
    item_start = region_start
    for token in tokens[order_index + 1 :]:
        if token.start >= region_end:
            break
        if token.depth == 0 and token.kind == "punct" and token.text == ",":
            items.append((item_start, token.start))
            item_start = token.end
    items.append((item_start, region_end))

    replacements: list[tuple[int, int, str]] = []
    previous_null_expression: str | None = None
    for start, end in items:
        original = sql[start:end]
        expression_text = original.strip()
        if not expression_text:
            continue
        null_match = re.search(
            r"\bCASE\s+WHEN\b.*?\bIS\s+NULL\b.*?\bEND\b",
            expression_text,
            flags=re.IGNORECASE | re.DOTALL,
        )
        if null_match:
            inner = re.search(
                r"\bCASE\s+WHEN\s*\(?\s*(.*?)\s*\)?\s+IS\s+NULL\b",
                expression_text,
                flags=re.IGNORECASE | re.DOTALL,
            )
            previous_null_expression = _canonical_expr(inner.group(1)) if inner else None
            continue

        direction_match = re.search(
            r"\s+(ASC|DESC)\s*(?:NULLS\s+(FIRST|LAST)\s*)?$",
            expression_text,
            flags=re.IGNORECASE,
        )
        direction = "ASC"
        explicit_nulls: str | None = None
        if direction_match:
            direction = direction_match.group(1).upper()
            explicit_nulls = direction_match.group(2)
            expression_text = expression_text[: direction_match.start()].rstrip()
        else:
            nulls_only = re.search(
                r"\s+NULLS\s+(FIRST|LAST)\s*$", expression_text, flags=re.IGNORECASE
            )
            if nulls_only:
                explicit_nulls = nulls_only.group(1)
                expression_text = expression_text[: nulls_only.start()].rstrip()

        expression = expression_text.strip()
        canonical = _canonical_expr(expression)
        if previous_null_expression == canonical:
            continue
        leaf = _leaf_identifier(expression)
        if leaf and leaf.lower() in config.nullable_columns:
            nulls_first = explicit_nulls.upper() == "FIRST" if explicit_nulls else direction == "ASC"
            rank_null = 0 if nulls_first else 1
            rank_value = 1 if nulls_first else 0
            leading = original[: len(original) - len(original.lstrip())]
            trailing = original[len(original.rstrip()) :]
            replacement = (
                leading
                +
                f"CASE WHEN ({expression}) IS NULL THEN {rank_null} ELSE {rank_value} END, "
                f"{expression} {direction}"
                f"{trailing}"
            )
            replacements.append((start, end, replacement))
            notes.append(f"NULL ordering made explicit for {expression}")
            previous_null_expression = canonical
        else:
            previous_null_expression = None

    return replace_spans(sql, replacements) if replacements else sql


def _canonical_expr(expression: str) -> str:
    return re.sub(r"\s+", " ", expression.strip()).lower()


def _leaf_identifier(expression: str) -> str | None:
    matches = list(re.finditer(r"\[([^\]]+)\]|[A-Za-z_][A-Za-z0-9_]*", expression))
    if not matches:
        return None
    leaf = matches[-1].group(0)
    if leaf.startswith("[") and leaf.endswith("]"):
        leaf = leaf[1:-1].replace("]]", "]")
    return leaf


def protect_keywords_like(sql: str) -> str:
    """Compatibility helper retained for downstream evaluators."""
    return mask_comments_and_strings(sql)
