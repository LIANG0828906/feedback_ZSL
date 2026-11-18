from __future__ import annotations

from dataclasses import dataclass

from .errors import E_READONLY, GatewayError
from .sqltext import Token, tokenize


FORBIDDEN_WORDS = {
    "insert",
    "update",
    "delete",
    "merge",
    "drop",
    "exec",
    "execute",
    "alter",
    "create",
    "truncate",
    "grant",
    "revoke",
    "deny",
    "backup",
    "restore",
    "dbcc",
    "bulk",
    "kill",
    "shutdown",
    "reconfigure",
    "waitfor",
    "use",
    "set",
    "into",
    "openrowset",
    "opendatasource",
    "openquery",
}


@dataclass(slots=True)
class GuardResult:
    sql: str
    tokens: list[Token]


def _fragment(sql: str, token: Token) -> str:
    end = min(len(sql), token.start + 100)
    return sql[token.start:end].strip()


def validate_read_only(sql: str) -> GuardResult:
    if not sql or not sql.strip():
        raise GatewayError(E_READONLY, "empty query text is not executable")

    tokens = tokenize(sql)
    significant = [
        token for token in tokens if token.kind not in {"comment"} and token.text.strip()
    ]
    if not significant:
        raise GatewayError(E_READONLY, "query contains no executable statement")

    first = significant[0]
    if first.kind != "word" or first.lower not in {"select", "with"}:
        raise GatewayError(
            E_READONLY,
            "only SELECT statements and SELECT-bearing CTEs are allowed",
            fragment=_fragment(sql, first),
            offset=first.start,
        )

    has_select = False
    top_level_semicolons: list[Token] = []
    for token in significant:
        if token.kind == "word":
            word = token.lower
            if word == "select":
                has_select = True
            if word in FORBIDDEN_WORDS:
                raise GatewayError(
                    E_READONLY,
                    f"read-only gateway rejected keyword {token.text.upper()}",
                    fragment=_fragment(sql, token),
                    offset=token.start,
                    details={"keyword": token.text.upper()},
                )
            if token.text.startswith("#"):
                raise GatewayError(
                    E_READONLY,
                    "temporary tables are not allowed",
                    fragment=_fragment(sql, token),
                    offset=token.start,
                    details={"keyword": token.text},
                )
        elif token.kind == "punct" and token.text == ";" and token.depth == 0:
            top_level_semicolons.append(token)

    if not has_select:
        raise GatewayError(
            E_READONLY,
            "query does not contain a SELECT statement",
            fragment=_fragment(sql, first),
            offset=first.start,
        )

    non_empty = [token for token in significant if token.text != ";"]
    for semicolon in top_level_semicolons:
        after = [token for token in non_empty if token.start > semicolon.start]
        if after:
            raise GatewayError(
                E_READONLY,
                "multiple SQL statements are not allowed",
                fragment=_fragment(sql, semicolon),
                offset=semicolon.start,
            )

    return GuardResult(sql=sql, tokens=tokens)
