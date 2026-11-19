"""A deliberately small SQL parser for the two report dialects.

The parser accepts the SQL subset used by the sample jobs and keeps enough
structure for dialect normalization, pushdown, join reordering, and streaming
deduplication. Unsupported constructs fail with a stable business error rather
than being silently mis-executed.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import ERROR_SQL, ExecutorError


@dataclass(frozen=True)
class Token:
    kind: str
    value: str
    pos: int
    quote: str | None = None

    @property
    def upper(self) -> str:
        return self.value.upper()


@dataclass(frozen=True)
class Expr:
    pass


@dataclass(frozen=True)
class ColumnRef(Expr):
    table: str | None
    name: str
    quoted: bool = False


@dataclass(frozen=True)
class Literal(Expr):
    value: Any
    kind: str


@dataclass(frozen=True)
class Star(Expr):
    table: str | None = None


@dataclass(frozen=True)
class BinaryOp(Expr):
    op: str
    left: Expr
    right: Expr


@dataclass(frozen=True)
class UnaryOp(Expr):
    op: str
    operand: Expr


@dataclass(frozen=True)
class FuncCall(Expr):
    name: str
    args: tuple[Expr, ...]
    distinct: bool = False


@dataclass(frozen=True)
class Cast(Expr):
    expr: Expr
    target: str


@dataclass(frozen=True)
class IsNull(Expr):
    expr: Expr
    negated: bool = False


@dataclass(frozen=True)
class Between(Expr):
    expr: Expr
    low: Expr
    high: Expr
    negated: bool = False


@dataclass(frozen=True)
class SelectItem:
    expr: Expr
    alias: str | None

    @property
    def output_name(self) -> str:
        if self.alias:
            return self.alias
        if isinstance(self.expr, ColumnRef):
            return self.expr.name
        if isinstance(self.expr, FuncCall):
            return self.expr.name.lower()
        return "expr"


@dataclass(frozen=True)
class TableRef:
    name: str
    alias: str
    quoted: bool = False


@dataclass(frozen=True)
class JoinEdge:
    kind: str
    right_alias: str
    condition: Expr


@dataclass(frozen=True)
class OrderItem:
    expr: Expr
    direction: str = "ASC"
    nulls: str | None = None


@dataclass(frozen=True)
class Query:
    select: tuple[SelectItem, ...]
    distinct: bool
    distinct_on: tuple[Expr, ...]
    tables: tuple[TableRef, ...]
    joins: tuple[JoinEdge, ...]
    where: Expr | None
    group_by: tuple[Expr, ...]
    having: Expr | None
    order_by: tuple[OrderItem, ...]
    limit: int | None
    sql: str = field(default="", compare=False)

    def table_map(self) -> dict[str, TableRef]:
        return {t.alias: t for t in self.tables}


AGGREGATE_NAMES = {"COUNT", "SUM", "AVG", "MIN", "MAX"}

_CLAUSE_WORDS = {
    "FROM",
    "WHERE",
    "GROUP",
    "HAVING",
    "ORDER",
    "LIMIT",
    "JOIN",
    "INNER",
    "LEFT",
    "RIGHT",
    "FULL",
    "OUTER",
    "ON",
    "AS",
    "ASC",
    "DESC",
    "NULLS",
    "FIRST",
    "LAST",
    "UNION",
}

_OP_PRECEDENCE = {
    "OR": 10,
    "AND": 20,
    "=": 30,
    "!=": 30,
    "<>": 30,
    "<": 30,
    "<=": 30,
    ">": 30,
    ">=": 30,
    "LIKE": 30,
    "IN": 30,
    "IS": 30,
    "BETWEEN": 30,
    "||": 40,
    "+": 40,
    "-": 40,
    "*": 50,
    "/": 50,
    "%": 50,
    "DIV": 50,
    "MOD": 50,
}


def _sql_error(message: str, sql: str, pos: int) -> ExecutorError:
    lo = max(0, pos - 30)
    hi = min(len(sql), pos + 30)
    snippet = sql[lo:hi].replace("\n", " ")
    return ExecutorError(
        ERROR_SQL,
        f"{message} near character {pos}: ...{snippet}...",
    )


def tokenize(sql: str) -> list[Token]:
    tokens: list[Token] = []
    i = 0
    n = len(sql)
    while i < n:
        ch = sql[i]
        if ch.isspace():
            i += 1
            continue
        if sql.startswith("--", i):
            nl = sql.find("\n", i + 2)
            i = n if nl < 0 else nl + 1
            continue
        if sql.startswith("/*", i):
            end = sql.find("*/", i + 2)
            if end < 0:
                raise _sql_error("unterminated block comment", sql, i)
            i = end + 2
            continue
        if ch in {"'", '"', "`"}:
            quote = ch
            j = i + 1
            buf: list[str] = []
            while j < n:
                if sql[j] == quote:
                    if j + 1 < n and sql[j + 1] == quote:
                        buf.append(quote)
                        j += 2
                        continue
                    break
                if sql[j] == "\\" and j + 1 < n and quote == "'":
                    escapes = {
                        "n": "\n",
                        "r": "\r",
                        "t": "\t",
                        "\\": "\\",
                        "'": "'",
                        '"': '"',
                    }
                    buf.append(escapes.get(sql[j + 1], sql[j + 1]))
                    j += 2
                    continue
                buf.append(sql[j])
                j += 1
            if j >= n:
                raise _sql_error("unterminated quoted value", sql, i)
            kind = "STRING" if quote == "'" else "IDENT"
            tokens.append(Token(kind, "".join(buf), i, quote))
            i = j + 1
            continue
        if ch.isdigit() or (ch == "." and i + 1 < n and sql[i + 1].isdigit()):
            m = re.match(r"(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?", sql[i:])
            assert m is not None
            tokens.append(Token("NUMBER", m.group(0), i))
            i += len(m.group(0))
            continue
        if ch.isalpha() or ch == "_":
            m = re.match(r"[A-Za-z_][A-Za-z0-9_$]*", sql[i:])
            assert m is not None
            tokens.append(Token("WORD", m.group(0), i))
            i += len(m.group(0))
            continue
        two = sql[i : i + 2]
        if two in {"<=", ">=", "<>", "!=", "||", "::"}:
            tokens.append(Token("OP", two, i))
            i += 2
            continue
        if ch in "(),.;*+-/%=<>:":
            tokens.append(Token("OP" if ch not in "(),.;" else ch, ch, i))
            i += 1
            continue
        raise _sql_error(f"unexpected character {ch!r}", sql, i)
    tokens.append(Token("EOF", "", n))
    return tokens


class Parser:
    def __init__(self, sql: str):
        self.sql = sql
        self.tokens = tokenize(sql)
        self.index = 0

    @property
    def current(self) -> Token:
        return self.tokens[self.index]

    def peek(self, offset: int = 1) -> Token:
        idx = min(len(self.tokens) - 1, self.index + offset)
        return self.tokens[idx]

    def advance(self) -> Token:
        tok = self.current
        if tok.kind != "EOF":
            self.index += 1
        return tok

    def accept(self, value: str) -> Token | None:
        tok = self.current
        if tok.upper == value.upper() or tok.value == value:
            self.index += 1
            return tok
        return None

    def expect(self, value: str) -> Token:
        tok = self.accept(value)
        if tok is None:
            raise _sql_error(f"expected {value!r}", self.sql, self.current.pos)
        return tok

    def parse(self) -> Query:
        self.expect("SELECT")
        distinct = False
        distinct_on: list[Expr] = []
        if self.accept("DISTINCT"):
            distinct = True
            if self.accept("ON"):
                self.expect("(")
                distinct_on = self.parse_expr_list(")")
                self.expect(")")
        elif self.accept("ALL"):
            pass

        select_items = self.parse_select_list()
        self.expect("FROM")
        tables, joins = self.parse_from()

        where = None
        if self.accept("WHERE"):
            where = self.parse_expr()

        group_by: list[Expr] = []
        if self.accept("GROUP"):
            self.expect("BY")
            group_by = self.parse_expr_list_until_clause()

        having = None
        if self.accept("HAVING"):
            having = self.parse_expr()

        order_by: list[OrderItem] = []
        if self.accept("ORDER"):
            self.expect("BY")
            order_by = self.parse_order_list()

        limit = None
        if self.accept("LIMIT"):
            tok = self.advance()
            if tok.kind != "NUMBER":
                raise _sql_error("LIMIT requires an integer", self.sql, tok.pos)
            limit = int(tok.value)

        self.accept(";")
        if self.current.kind != "EOF":
            raise _sql_error("unexpected trailing SQL", self.sql, self.current.pos)
        return Query(
            select=tuple(select_items),
            distinct=distinct,
            distinct_on=tuple(distinct_on),
            tables=tuple(tables),
            joins=tuple(joins),
            where=where,
            group_by=tuple(group_by),
            having=having,
            order_by=tuple(order_by),
            limit=limit,
            sql=self.sql,
        )

    def parse_select_list(self) -> list[SelectItem]:
        items: list[SelectItem] = []
        while True:
            expr = self.parse_expr()
            alias = None
            if self.accept("AS"):
                alias_tok = self.advance()
                if alias_tok.kind not in {"WORD", "IDENT"}:
                    raise _sql_error(
                        "expected select alias", self.sql, alias_tok.pos
                    )
                alias = alias_tok.value
            elif (
                self.current.kind in {"WORD", "IDENT"}
                and self.current.upper not in _CLAUSE_WORDS
            ):
                alias = self.advance().value
            items.append(SelectItem(expr, alias))
            if not self.accept(","):
                break
        return items

    def parse_from(self) -> tuple[list[TableRef], list[JoinEdge]]:
        tables: list[TableRef] = [self.parse_table_ref()]
        joins: list[JoinEdge] = []
        while True:
            if self.accept(","):
                table = self.parse_table_ref()
                tables.append(table)
                joins.append(
                    JoinEdge(
                        kind="CROSS",
                        right_alias=table.alias,
                        condition=Literal(True, "bool"),
                    )
                )
                continue

            kind: str | None = None
            if self.accept("JOIN"):
                kind = "INNER"
            elif self.accept("INNER"):
                self.expect("JOIN")
                kind = "INNER"
            elif self.accept("LEFT"):
                self.accept("OUTER")
                self.expect("JOIN")
                kind = "LEFT"
            elif self.accept("RIGHT"):
                self.accept("OUTER")
                self.expect("JOIN")
                kind = "RIGHT"
            elif self.accept("FULL"):
                self.accept("OUTER")
                self.expect("JOIN")
                kind = "FULL"
            if kind is None:
                break
            table = self.parse_table_ref()
            tables.append(table)
            if kind != "CROSS":
                self.expect("ON")
                condition = self.parse_expr()
            else:
                condition = Literal(True, "bool")
            joins.append(JoinEdge(kind, table.alias, condition))
        return tables, joins

    def parse_table_ref(self) -> TableRef:
        tok = self.advance()
        if tok.kind not in {"WORD", "IDENT"}:
            raise _sql_error("expected table name", self.sql, tok.pos)
        name = tok.value
        while self.accept("."):
            nxt = self.advance()
            if nxt.kind not in {"WORD", "IDENT"}:
                raise _sql_error("expected qualified table name", self.sql, nxt.pos)
            name = f"{name}.{nxt.value}"
        alias = name.split(".")[-1]
        if self.accept("AS"):
            alias_tok = self.advance()
            if alias_tok.kind not in {"WORD", "IDENT"}:
                raise _sql_error("expected table alias", self.sql, alias_tok.pos)
            alias = alias_tok.value
        elif (
            self.current.kind in {"WORD", "IDENT"}
            and self.current.upper not in _CLAUSE_WORDS
        ):
            alias = self.advance().value
        return TableRef(name=name, alias=alias, quoted=tok.quote is not None)

    def parse_order_list(self) -> list[OrderItem]:
        items: list[OrderItem] = []
        while True:
            expr = self.parse_expr()
            direction = "ASC"
            if self.accept("ASC"):
                direction = "ASC"
            elif self.accept("DESC"):
                direction = "DESC"
            nulls = None
            if self.accept("NULLS"):
                if self.accept("FIRST"):
                    nulls = "FIRST"
                elif self.accept("LAST"):
                    nulls = "LAST"
                else:
                    raise _sql_error(
                        "expected FIRST or LAST after NULLS",
                        self.sql,
                        self.current.pos,
                    )
            items.append(OrderItem(expr, direction, nulls))
            if not self.accept(","):
                break
        return items

    def parse_expr_list(self, terminator: str = ")") -> list[Expr]:
        items: list[Expr] = []
        if self.current.value == terminator:
            return items
        while True:
            items.append(self.parse_expr())
            if not self.accept(","):
                break
        return items

    def parse_expr_list_until_clause(self) -> list[Expr]:
        items: list[Expr] = []
        while True:
            items.append(self.parse_expr())
            if not self.accept(","):
                break
            if self.current.kind == "EOF":
                break
        return items

    def parse_expr(self, min_bp: int = 0) -> Expr:
        tok = self.advance()
        if tok.kind == "NUMBER":
            value: Any
            if any(c in tok.value for c in ".eE"):
                value = float(tok.value)
                kind = "float"
            else:
                value = int(tok.value)
                kind = "int"
            left: Expr = Literal(value, kind)
        elif tok.kind == "STRING":
            left = Literal(tok.value, "string")
        elif tok.value == "(":
            left = self.parse_expr()
            self.expect(")")
        elif tok.value in {"-", "+"}:
            left = UnaryOp(tok.value, self.parse_expr(60))
        elif tok.upper == "NOT":
            left = UnaryOp("NOT", self.parse_expr(20))
        elif tok.upper in {"NULL", "TRUE", "FALSE"}:
            left = Literal(
                None if tok.upper == "NULL" else tok.upper == "TRUE",
                "null" if tok.upper == "NULL" else "bool",
            )
        elif tok.kind in {"WORD", "IDENT"}:
            if tok.upper == "CAST" and self.current.value == "(":
                self.expect("(")
                inner = self.parse_expr()
                self.expect("AS")
                type_parts: list[str] = []
                depth = 0
                while True:
                    if self.current.value == ")":
                        if depth == 0:
                            break
                        depth -= 1
                        type_parts.append(self.advance().value)
                        continue
                    if self.current.value == "(":
                        depth += 1
                        type_parts.append(self.advance().value)
                        continue
                    part = self.advance()
                    if part.kind == "EOF":
                        raise _sql_error("unterminated CAST", self.sql, part.pos)
                    type_parts.append(part.value)
                self.expect(")")
                left = Cast(inner, "".join(type_parts).upper())
            elif self.current.value == "(":
                self.expect("(")
                distinct = False
                if self.accept("DISTINCT"):
                    distinct = True
                args: list[Expr] = []
                if self.current.value != ")":
                    if self.accept("*"):
                        args.append(Star())
                    else:
                        args = self.parse_expr_list(")")
                self.expect(")")
                left = FuncCall(tok.value, tuple(args), distinct)
            else:
                table: str | None = None
                name = tok.value
                quoted = tok.quote is not None
                if self.accept("."):
                    if self.accept("*"):
                        left = Star(name)
                        return self.parse_postfix(left, min_bp)
                    name_tok = self.advance()
                    if name_tok.kind not in {"WORD", "IDENT"}:
                        raise _sql_error(
                            "expected column name", self.sql, name_tok.pos
                        )
                    table = tok.value
                    name = name_tok.value
                    quoted = name_tok.quote is not None
                left = ColumnRef(table, name, quoted)
        elif tok.value == "*":
            left = Star(None)
        else:
            raise _sql_error("expected expression", self.sql, tok.pos)
        return self.parse_postfix(left, min_bp)

    def parse_postfix(self, left: Expr, min_bp: int) -> Expr:
        while True:
            tok = self.current
            op = tok.upper if tok.kind == "WORD" else tok.value
            if op == "IS":
                if 30 < min_bp:
                    break
                self.advance()
                negated = bool(self.accept("NOT"))
                self.expect("NULL")
                left = IsNull(left, negated)
                continue
            if op == "BETWEEN":
                if 30 < min_bp:
                    break
                self.advance()
                negated = False
                low = self.parse_expr(31)
                self.expect("AND")
                high = self.parse_expr(31)
                left = Between(left, low, high, negated)
                continue
            if op == "NOT" and self.peek().upper == "BETWEEN":
                if 30 < min_bp:
                    break
                self.advance()
                self.advance()
                low = self.parse_expr(31)
                self.expect("AND")
                high = self.parse_expr(31)
                left = Between(left, low, high, True)
                continue
            bp = _OP_PRECEDENCE.get(op)
            if bp is None or bp < min_bp:
                break
            if op in {"IS", "IN", "BETWEEN"}:
                break
            self.advance()
            right = self.parse_expr(bp + 1)
            left = BinaryOp(op, left, right)
        return left


def parse_sql(sql: str) -> Query:
    if not sql.strip():
        raise ExecutorError(ERROR_SQL, "empty SQL")
    return Parser(sql).parse()


def walk_expr(expr: Expr | None) -> Iterable[Expr]:
    if expr is None:
        return
    yield expr
    if isinstance(expr, BinaryOp):
        yield from walk_expr(expr.left)
        yield from walk_expr(expr.right)
    elif isinstance(expr, UnaryOp):
        yield from walk_expr(expr.operand)
    elif isinstance(expr, FuncCall):
        for arg in expr.args:
            yield from walk_expr(arg)
    elif isinstance(expr, Cast):
        yield from walk_expr(expr.expr)
    elif isinstance(expr, IsNull):
        yield from walk_expr(expr.expr)
    elif isinstance(expr, Between):
        yield from walk_expr(expr.expr)
        yield from walk_expr(expr.low)
        yield from walk_expr(expr.high)


def expr_aliases(expr: Expr | None) -> set[str]:
    aliases: set[str] = set()
    for node in walk_expr(expr):
        if isinstance(node, ColumnRef) and node.table:
            aliases.add(node.table)
        elif isinstance(node, Star) and node.table:
            aliases.add(node.table)
    return aliases


def expr_columns(expr: Expr | None) -> list[ColumnRef]:
    return [node for node in walk_expr(expr) if isinstance(node, ColumnRef)]


def contains_aggregate(expr: Expr | None) -> bool:
    return any(
        isinstance(node, FuncCall) and node.name.upper() in AGGREGATE_NAMES
        for node in walk_expr(expr)
    )


def clone_expr(expr: Expr) -> Expr:
    # All expression nodes are immutable, so returning the node is safe.
    return expr


def and_expr(parts: Iterable[Expr]) -> Expr | None:
    items = list(parts)
    if not items:
        return None
    result = items[0]
    for item in items[1:]:
        result = BinaryOp("AND", result, item)
    return result


def split_conjuncts(expr: Expr | None) -> list[Expr]:
    if expr is None:
        return []
    if isinstance(expr, BinaryOp) and expr.op == "AND":
        return split_conjuncts(expr.left) + split_conjuncts(expr.right)
    return [expr]


def is_equality(expr: Expr) -> bool:
    return isinstance(expr, BinaryOp) and expr.op == "="


def equality_sides(expr: Expr) -> tuple[ColumnRef, ColumnRef] | None:
    if (
        isinstance(expr, BinaryOp)
        and expr.op == "="
        and isinstance(expr.left, ColumnRef)
        and isinstance(expr.right, ColumnRef)
    ):
        return expr.left, expr.right
    return None

