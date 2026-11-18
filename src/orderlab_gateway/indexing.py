from __future__ import annotations

import re
from dataclasses import dataclass, field

from .config import GatewayConfig, IndexSpec
from .errors import E_TABLE, GatewayError
from .sqltext import (
    Token,
    decode_string,
    identifier,
    mask_comments,
    replace_spans,
    tokenize,
)


STOP_ALIAS_WORDS = {
    "where",
    "on",
    "group",
    "order",
    "having",
    "cross",
    "outer",
    "inner",
    "left",
    "right",
    "full",
    "join",
    "union",
    "except",
    "intersect",
    "option",
    "offset",
    "for",
    "tablesample",
    "pivot",
    "unpivot",
    "with",
}


@dataclass(slots=True)
class Predicate:
    table: str
    column: str
    value: object
    fragment: str


@dataclass(slots=True)
class TableNeed:
    alias: str
    table: str
    required_columns: set[str] = field(default_factory=set)
    equality_columns: set[str] = field(default_factory=set)
    tenant: Predicate | None = None
    status: Predicate | None = None
    candidate: IndexSpec | None = None
    unresolved_reason: str | None = None
    offending_fragments: list[str] = field(default_factory=list)


@dataclass(slots=True)
class Analysis:
    normalized_sql: str
    tables: list[TableNeed]
    expected_indexes: list[IndexSpec]
    tenant_id: int | None
    status: str | None
    unresolved: list[TableNeed]

    @property
    def can_execute_detail(self) -> bool:
        return not self.unresolved and bool(self.tables)

    @property
    def condition_fragment(self) -> str | None:
        for table in self.unresolved:
            if table.offending_fragments:
                return table.offending_fragments[0]
        fragments: list[str] = []
        for table in self.tables:
            if table.tenant and table.tenant.fragment not in fragments:
                fragments.append(table.tenant.fragment)
            if table.status and table.status.fragment not in fragments:
                fragments.append(table.status.fragment)
        return " AND ".join(fragments) if fragments else None


def _token_fragment(sql: str, token: Token, length: int = 100) -> str:
    return sql[token.start : min(len(sql), token.start + length)].strip()


def _alias_map(sql: str, tokens: list[Token], config: GatewayConfig) -> tuple[dict[str, str], list[TableNeed]]:
    aliases: dict[str, str] = {}
    needs: dict[str, TableNeed] = {}
    table_names = set(config.tables)
    for index, token in enumerate(tokens):
        if token.kind != "word" or token.lower not in {"from", "join"}:
            continue
        cursor = index + 1
        if cursor >= len(tokens):
            continue
        if (
            cursor + 2 < len(tokens)
            and tokens[cursor].kind in {"word", "bracket"}
            and tokens[cursor + 1].text == "."
            and tokens[cursor + 2].kind in {"word", "bracket"}
        ):
            table_token = tokens[cursor + 2]
            cursor += 3
        else:
            table_token = tokens[cursor]
            cursor += 1
        table = identifier(table_token)
        if table.lower() not in table_names:
            if table.startswith("("):
                continue
            raise GatewayError(
                E_TABLE,
                f"table {table} is outside the gateway query boundary",
                fragment=_token_fragment(sql, table_token),
                offset=table_token.start,
            )
        table_lower = table.lower()
        alias = table
        if cursor < len(tokens) and tokens[cursor].kind == "word" and tokens[cursor].lower == "as":
            cursor += 1
        if cursor < len(tokens):
            candidate = tokens[cursor]
            if candidate.kind in {"word", "bracket", "quoted"}:
                candidate_name = identifier(candidate)
                if candidate_name.lower() not in STOP_ALIAS_WORDS:
                    alias = candidate_name
        alias_key = alias.lower()
        aliases[alias_key] = table_lower
        needs.setdefault(
            alias_key,
            TableNeed(alias=alias_key, table=table_lower),
        )
    return aliases, list(needs.values())


def _known_columns(config: GatewayConfig) -> set[str]:
    columns: set[str] = set()
    for table in config.tables.values():
        columns.update(column.lower() for column in table.columns)
    return columns


def _table_columns(config: GatewayConfig, table: str) -> set[str]:
    spec = config.tables.get(table.lower())
    return {column.lower() for column in spec.columns} if spec else set()


def _display_column(config: GatewayConfig, table: str, column: str) -> str:
    spec = config.tables.get(table.lower())
    if spec:
        for candidate in spec.columns:
            if candidate.lower() == column.lower():
                return candidate
    return column


def _collect_required_columns(
    sql: str,
    tokens: list[Token],
    aliases: dict[str, str],
    config: GatewayConfig,
    needs: list[TableNeed],
) -> None:
    by_alias = {need.alias: need for need in needs}
    known = _known_columns(config)

    for index, token in enumerate(tokens):
        if (
            token.kind in {"word", "bracket"}
            and index + 2 < len(tokens)
            and tokens[index + 1].text == "."
            and tokens[index + 2].kind in {"word", "bracket"}
        ):
            alias = identifier(token).lower()
            if alias in by_alias:
                by_alias[alias].required_columns.add(identifier(tokens[index + 2]).lower())

    for index, token in enumerate(tokens):
        if token.kind != "word":
            continue
        word = token.lower
        if word not in known:
            continue
        if index > 0 and tokens[index - 1].text == ".":
            continue
        if index + 1 < len(tokens) and tokens[index + 1].text == "(":
            continue
        targets = [
            need for need in needs if word in _table_columns(config, need.table)
        ]
        if len(targets) == 1:
            targets[0].required_columns.add(word)
        elif len(aliases) == 1:
            targets[0].required_columns.add(word)
        else:
            for need in targets:
                need.required_columns.add(word)

    if any(token.kind == "punct" and token.text == "*" for token in tokens):
        for need in needs:
            need.required_columns.update(_table_columns(config, need.table))

    for index, token in enumerate(tokens):
        if token.kind not in {"word", "bracket"}:
            continue
        column = identifier(token).lower()
        if column not in known:
            continue
        cursor = index
        alias: str | None = None
        if index >= 2 and tokens[index - 1].text == ".":
            alias = identifier(tokens[index - 2]).lower()
        if cursor + 2 >= len(tokens):
            continue
        if tokens[cursor + 1].kind != "punct" or tokens[cursor + 1].text != "=":
            continue
        if tokens[cursor + 2].kind not in {"number", "string"}:
            continue
        targets: list[TableNeed]
        if alias and alias in by_alias:
            targets = [by_alias[alias]]
        else:
            targets = [
                need for need in needs if column in _table_columns(config, need.table)
            ]
        for target in targets:
            target.equality_columns.add(column)

    for match in re.finditer(
        r"\bTRY_CONVERT\s*\(\s*bigint\s*,\s*([A-Za-z_][A-Za-z0-9_]*(?:\s*\.\s*[A-Za-z_][A-Za-z0-9_]*)?)\s*\)\s*=",
        mask_comments(sql),
        flags=re.IGNORECASE,
    ):
        expression = match.group(1)
        parts = [part.strip() for part in expression.split(".")]
        column = parts[-1].lower()
        alias = parts[0].lower() if len(parts) > 1 else None
        if alias and alias in by_alias:
            targets = [by_alias[alias]]
        else:
            targets = [
                need for need in needs if column in _table_columns(config, need.table)
            ]
        for target in targets:
            target.equality_columns.add(column)


def _predicate_for(
    tokens: list[Token],
    index: int,
    aliases: dict[str, str],
    sql: str,
) -> Predicate | None:
    token = tokens[index]
    column = identifier(token).lower()
    alias: str | None = None
    start = token.start
    cursor = index
    if index >= 2 and tokens[index - 1].text == ".":
        alias_token = tokens[index - 2]
        alias = identifier(alias_token).lower()
        start = alias_token.start
        cursor = index
    if cursor + 1 >= len(tokens):
        return None
    operator_token = tokens[cursor + 1]
    if operator_token.kind != "punct" or operator_token.text not in {"=", "IN"}:
        return None
    value_index = cursor + 2
    if operator_token.text.lower() == "in":
        if value_index + 1 >= len(tokens) or tokens[value_index].text != "(":
            return None
        value_index += 1
    if value_index >= len(tokens):
        return None
    value_token = tokens[value_index]
    if value_token.kind == "number":
        value: object = int(value_token.text) if value_token.text.isdigit() else value_token.text
    elif value_token.kind == "string":
        value = decode_string(value_token)
    else:
        return None
    end = value_token.end
    if operator_token.text.lower() == "in":
        close_index = value_index + 1
        if close_index >= len(tokens) or tokens[close_index].text != ")":
            return None
        if tokens[value_index + 1].text != ")":
            return None
        end = tokens[close_index].end
    if alias is not None and alias not in aliases:
        alias = None
    if alias is None:
        targets = [
            need.alias for need in []
        ]
        return Predicate(
            table="",
            column=column,
            value=value,
            fragment=sql[start:end].strip(),
        )
    return Predicate(
        table=aliases[alias],
        column=column,
        value=value,
        fragment=sql[start:end].strip(),
    )


def _attach_predicates(
    sql: str,
    tokens: list[Token],
    aliases: dict[str, str],
    needs: list[TableNeed],
) -> None:
    by_alias = {need.alias: need for need in needs}
    for index, token in enumerate(tokens):
        if token.kind != "word" or token.lower not in {"tenantid", "status"}:
            continue
        predicate = _predicate_for(tokens, index, aliases, sql)
        if predicate is None:
            continue
        targets: list[TableNeed] = []
        if predicate.table:
            for alias, table in aliases.items():
                if table == predicate.table and predicate.column.lower() in _columns_for_fragment(needs, alias):
                    targets.append(by_alias[alias])
        else:
            targets = [
                need
                for need in needs
                if predicate.column.lower() in _columns_for_fragment(needs, need.alias)
            ]
            if len(targets) > 1:
                targets = []
        for target in targets:
            predicate_for_target = Predicate(
                table=target.table,
                column=predicate.column,
                value=predicate.value,
                fragment=predicate.fragment,
            )
            if predicate.column.lower() == "tenantid" and target.tenant is None:
                target.tenant = predicate_for_target
            elif predicate.column.lower() == "status" and target.status is None:
                target.status = predicate_for_target


def _columns_for_fragment(needs: list[TableNeed], alias: str) -> set[str]:
    for need in needs:
        if need.alias == alias:
            return need.required_columns
    return set()


def analyze_query(sql: str, config: GatewayConfig) -> Analysis:
    tokens = tokenize(mask_comments(sql))
    aliases, needs = _alias_map(sql, tokens, config)
    _collect_required_columns(sql, tokens, aliases, config, needs)
    if not any(token.kind == "word" and token.lower == "or" for token in tokens):
        _attach_predicates(sql, tokens, aliases, needs)

    chosen: list[IndexSpec] = []
    unresolved: list[TableNeed] = []
    for need in needs:
        table_columns = _table_columns(config, need.table)
        candidates: list[tuple[int, int, str, IndexSpec]] = []
        for index_spec in config.indexes_for_table(need.table):
            key_lower = [column.lower() for column in index_spec.key_columns]
            if len(key_lower) < 2 or key_lower[0] != "tenantid" or key_lower[1] != "status":
                continue
            if need.tenant is None or need.status is None:
                continue
            missing = need.required_columns - index_spec.covered_columns - {"lineid"}
            if missing:
                need.offending_fragments.extend(
                    sorted(
                        f"{need.alias}.{_display_column(config, need.table, column)}"
                        for column in missing
                    )
                )
                continue
            score = 0
            for key_column in key_lower[2:]:
                if key_column in need.equality_columns:
                    score += 10
                elif key_column in need.required_columns:
                    score += 2
            if need.tenant.value is not None:
                score += 1
            if need.status.value is not None:
                score += 1
            candidates.append((score, len(index_spec.include_columns), index_spec.name, index_spec))
        if candidates:
            candidates.sort(key=lambda item: (-item[0], item[1], item[2]))
            need.candidate = candidates[0][3]
            chosen.append(need.candidate)
        else:
            if need.tenant is None:
                need.offending_fragments.append(f"missing TenantId predicate for {need.alias}")
            if need.status is None:
                need.offending_fragments.append(f"missing Status predicate for {need.alias}")
            if not need.offending_fragments:
                need.offending_fragments.append(
                    f"columns for {need.alias} are not covered by a tenant/status index"
                )
            need.offending_fragments = list(dict.fromkeys(need.offending_fragments))
            need.unresolved_reason = "no covering tenant/status index"
            unresolved.append(need)

    tenant_id: int | None = None
    status: str | None = None
    for need in needs:
        if need.tenant and isinstance(need.tenant.value, int):
            tenant_id = need.tenant.value
            break
    for need in needs:
        if need.status and isinstance(need.status.value, str):
            status = need.status.value
            break

    return Analysis(
        normalized_sql=sql,
        tables=needs,
        expected_indexes=chosen,
        tenant_id=tenant_id,
        status=status,
        unresolved=unresolved,
    )


def apply_index_hints(sql: str, analysis: Analysis) -> str:
    by_alias = {
        table.alias: table.candidate
        for table in analysis.tables
        if table.candidate is not None
    }
    if not by_alias:
        return sql
    tokens = tokenize(mask_comments(sql))
    replacements: list[tuple[int, int, str]] = []
    for index, token in enumerate(tokens):
        if token.kind != "word" or token.lower not in {"from", "join"}:
            continue
        cursor = index + 1
        if (
            cursor + 2 < len(tokens)
            and tokens[cursor].kind in {"word", "bracket"}
            and tokens[cursor + 1].text == "."
            and tokens[cursor + 2].kind in {"word", "bracket"}
        ):
            table_token = tokens[cursor + 2]
            cursor += 3
        elif cursor < len(tokens):
            table_token = tokens[cursor]
            cursor += 1
        else:
            continue
        alias = identifier(table_token).lower()
        insert_at = table_token.end
        if cursor < len(tokens) and tokens[cursor].kind == "word" and tokens[cursor].lower == "as":
            cursor += 1
        if cursor < len(tokens):
            candidate = tokens[cursor]
            if candidate.kind in {"word", "bracket", "quoted"}:
                alias_name = identifier(candidate)
                if alias_name.lower() not in STOP_ALIAS_WORDS:
                    alias = alias_name.lower()
                    insert_at = candidate.end
        index_spec = by_alias.get(alias)
        if index_spec is None:
            continue
        if (
            cursor < len(tokens) - 1
            and tokens[cursor].kind == "word"
            and tokens[cursor].lower == "with"
            and tokens[cursor + 1].text == "("
        ):
            continue
        replacements.append(
            (
                insert_at,
                insert_at,
                f" WITH (INDEX([{index_spec.name}]))",
            )
        )
    return replace_spans(sql, replacements)
