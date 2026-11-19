"""Dialect normalization and expression compilation."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

from .catalog import Catalog, type_category
from .errors import ERROR_SQL, ExecutorError
from .sql import (
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
    SelectItem,
    Star,
    UnaryOp,
)


ColumnMapper = Callable[[str, str], str]


@dataclass(frozen=True)
class DialectSpec:
    name: str
    display_name: str
    identifier_quote: str
    null_order_asc: str
    null_order_desc: str
    integer_division: str
    modulo: str
    date_trunc_function: str

    def report(self) -> dict[str, str]:
        return {
            "date_trunc_function": self.date_trunc_function,
            "identifier_quote": self.identifier_quote,
            "integer_division": self.integer_division,
            "modulo": self.modulo,
            "null_order_asc": self.null_order_asc,
            "null_order_desc": self.null_order_desc,
        }


DIALECTS: dict[str, DialectSpec] = {
    "mysql": DialectSpec(
        name="mysql",
        display_name="MySQL 8.0",
        identifier_quote="backtick",
        null_order_asc="FIRST",
        null_order_desc="LAST",
        integer_division="truncate_toward_zero",
        modulo="sign_of_dividend",
        date_trunc_function="DATE",
    ),
    "postgres": DialectSpec(
        name="postgres",
        display_name="PostgreSQL 15",
        identifier_quote="double_quote",
        null_order_asc="LAST",
        null_order_desc="FIRST",
        integer_division="truncate_toward_zero",
        modulo="sign_of_dividend",
        date_trunc_function="date_trunc",
    ),
}


def normalize_dialect_name(name: str) -> str:
    key = name.strip().lower()
    if key in {"postgresql", "postgres", "pg"}:
        return "postgres"
    if key in {"mysql", "mariadb"}:
        return "mysql"
    raise ExecutorError(ERROR_SQL, f"unsupported SQL dialect {name!r}")


def get_dialect(name: str) -> DialectSpec:
    return DIALECTS[normalize_dialect_name(name)]


def quote_ident(name: str) -> str:
    return '"' + name.replace('"', '""') + '"'


def sql_string(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def literal_sql(value: object, kind: str) -> str:
    if kind == "null" or value is None:
        return "NULL"
    if kind == "bool":
        return "TRUE" if value else "FALSE"
    if kind == "string":
        return sql_string(str(value))
    if kind == "float":
        return repr(float(value))
    return str(int(value))


def _category_of(
    expr: Expr,
    dialect: DialectSpec,
    catalog: Catalog,
    query: Query,
    alias: str | None = None,
) -> str:
    if isinstance(expr, ColumnRef):
        _, info = catalog.resolve_column(query, expr)
        return info.category
    if isinstance(expr, Literal):
        return "string" if expr.kind == "string" else (
            "bool" if expr.kind == "bool" else (
                "integer" if expr.kind == "int" else "float"
            )
        )
    if isinstance(expr, Cast):
        return type_category(expr.target) if expr.target else "other"
    if isinstance(expr, UnaryOp):
        return _category_of(expr.operand, dialect, catalog, query, alias)
    if isinstance(expr, BinaryOp):
        if expr.op in {
            "=",
            "!=",
            "<>",
            "<",
            "<=",
            ">",
            ">=",
            "AND",
            "OR",
            "LIKE",
            "IS",
        }:
            return "bool"
        if expr.op in {"DIV", "MOD", "%"}:
            return "integer"
        if expr.op == "/":
            left = _category_of(expr.left, dialect, catalog, query, alias)
            right = _category_of(expr.right, dialect, catalog, query, alias)
            if dialect.name == "postgres" and left == "integer" and right == "integer":
                return "integer"
            return "float"
        left = _category_of(expr.left, dialect, catalog, query, alias)
        right = _category_of(expr.right, dialect, catalog, query, alias)
        if left == "float" or right == "float":
            return "float"
        if left == "integer" and right == "integer":
            return "integer"
        return "other"
    if isinstance(expr, FuncCall):
        name = expr.name.upper()
        if name == "COUNT":
            return "integer"
        if name == "AVG":
            return "float"
        if name in {"SUM", "MIN", "MAX"} and expr.args:
            return _category_of(expr.args[0], dialect, catalog, query, alias)
        if name in {"DATE", "DATE_TRUNC"}:
            return "date"
        if name in {"DATE_FORMAT", "TO_CHAR", "STRFTIME", "UPPER", "LOWER"}:
            return "string"
        return "other"
    return "other"


def expr_category(
    expr: Expr, dialect: DialectSpec, catalog: Catalog, query: Query
) -> str:
    return _category_of(expr, dialect, catalog, query)


def _int_div_sql(left: str, right: str) -> str:
    return (
        "CAST(trunc(CAST(("
        + left
        + ") AS DOUBLE) / CAST(("
        + right
        + ") AS DOUBLE)) AS BIGINT)"
    )


def _mod_sql(left: str, right: str) -> str:
    return (
        "CAST((("
        + left
        + ") - trunc(CAST(("
        + left
        + ") AS DOUBLE) / CAST(("
        + right
        + ") AS DOUBLE)) * ("
        + right
        + ")) AS BIGINT)"
    )


def compile_expr(
    expr: Expr,
    dialect: DialectSpec,
    catalog: Catalog,
    query: Query,
    column_mapper: ColumnMapper | None = None,
    preserve_output_aliases: set[str] | None = None,
) -> str:
    def map_column(table: str, name: str) -> str:
        if column_mapper is not None:
            return column_mapper(table, name)
        if table:
            return f"{quote_ident(table)}.{quote_ident(name)}"
        return quote_ident(name)

    if isinstance(expr, ColumnRef):
        if (
            expr.table is None
            and preserve_output_aliases
            and expr.name in preserve_output_aliases
        ):
            return quote_ident(expr.name)
        if expr.table:
            return map_column(expr.table, expr.name)
        return map_column("", expr.name)
    if isinstance(expr, Literal):
        return literal_sql(expr.value, expr.kind)
    if isinstance(expr, Star):
        if expr.table:
            return f"{quote_ident(expr.table)}.*"
        return "*"
    if isinstance(expr, UnaryOp):
        inner = compile_expr(
            expr.operand,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        if expr.op.upper() == "NOT":
            return f"(NOT {inner})"
        return f"({expr.op}{inner})"
    if isinstance(expr, BinaryOp):
        left = compile_expr(
            expr.left,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        right = compile_expr(
            expr.right,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        op = expr.op.upper()
        if op == "DIV":
            return _int_div_sql(left, right)
        if op == "MOD":
            return _mod_sql(left, right)
        if op == "%":
            return _mod_sql(left, right)
        if op == "/":
            left_cat = _category_of(expr.left, dialect, catalog, query)
            right_cat = _category_of(expr.right, dialect, catalog, query)
            if dialect.name == "postgres" and left_cat == "integer" and right_cat == "integer":
                return _int_div_sql(left, right)
            return f"(({left}) / ({right}))"
        if op == "AND":
            return f"(({left}) AND ({right}))"
        if op == "OR":
            return f"(({left}) OR ({right}))"
        return f"(({left}) {op} ({right}))"
    if isinstance(expr, IsNull):
        inner = compile_expr(
            expr.expr,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        return f"(({inner}) IS {'NOT ' if expr.negated else ''}NULL)"
    if isinstance(expr, Between):
        inner = compile_expr(
            expr.expr,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        low = compile_expr(
            expr.low,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        high = compile_expr(
            expr.high,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        return f"(({inner}) {'NOT ' if expr.negated else ''}BETWEEN ({low}) AND ({high}))"
    if isinstance(expr, Cast):
        inner = compile_expr(
            expr.expr,
            dialect,
            catalog,
            query,
            column_mapper,
            preserve_output_aliases,
        )
        return f"CAST(({inner}) AS {expr.target})"
    if isinstance(expr, FuncCall):
        name = expr.name.upper()
        args = [
            compile_expr(
                arg,
                dialect,
                catalog,
                query,
                column_mapper,
                preserve_output_aliases,
            )
            for arg in expr.args
        ]
        distinct = "DISTINCT " if expr.distinct else ""
        if name == "DATE":
            if not args:
                raise ExecutorError(ERROR_SQL, "DATE() needs one argument")
            return f"CAST(({args[0]}) AS DATE)"
        if name == "DATE_FORMAT":
            if len(args) != 2:
                raise ExecutorError(ERROR_SQL, "DATE_FORMAT() needs two arguments")
            if dialect.name == "mysql":
                return f"strftime(({args[0]}), ({args[1]}))"
            return f"to_char(({args[0]}), ({args[1]}))"
        if name == "STR_TO_DATE":
            if len(args) != 2:
                raise ExecutorError(ERROR_SQL, "STR_TO_DATE() needs two arguments")
            return f"strptime(({args[0]}), ({args[1]}))"
        if name == "DATE_TRUNC":
            if len(args) != 2:
                raise ExecutorError(ERROR_SQL, "date_trunc() needs two arguments")
            if dialect.name == "mysql":
                # MySQL lacks date_trunc; day is the common report granularity.
                return f"(CASE WHEN lower(CAST({args[0]} AS VARCHAR)) = 'day' THEN CAST(({args[1]}) AS DATE) ELSE date_trunc(({args[0]}), ({args[1]})) END)"
            return f"date_trunc(({args[0]}), ({args[1]}))"
        if name == "MOD":
            if len(args) != 2:
                raise ExecutorError(ERROR_SQL, "MOD() needs two arguments")
            return _mod_sql(args[0], args[1])
        if name == "COUNT" and len(args) == 1 and args[0] == "*":
            return "COUNT(*)"
        if name in {"CAST", "TRY_CAST"}:
            raise ExecutorError(ERROR_SQL, "use CAST(expr AS type), not a function")
        return f"{name}({distinct}{', '.join(args)})"
    raise ExecutorError(ERROR_SQL, f"cannot compile expression {expr!r}")


def compile_order_item(
    item: OrderItem,
    dialect: DialectSpec,
    catalog: Catalog,
    query: Query,
    output_aliases: set[str],
    column_mapper: ColumnMapper | None = None,
) -> str:
    expr_sql = compile_expr(
        item.expr,
        dialect,
        catalog,
        query,
        column_mapper,
        preserve_output_aliases=output_aliases,
    )
    nulls = item.nulls
    if nulls is None:
        nulls = (
            dialect.null_order_asc
            if item.direction.upper() == "ASC"
            else dialect.null_order_desc
        )
    return f"{expr_sql} {item.direction.upper()} NULLS {nulls}"


def select_output_names(query: Query) -> list[str]:
    return [item.output_name for item in query.select]


def find_select_item(query: Query, alias: str) -> SelectItem | None:
    for item in query.select:
        if item.output_name.lower() == alias.lower():
            return item
    return None

