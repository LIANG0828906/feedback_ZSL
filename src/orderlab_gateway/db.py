from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any, Iterable

from .config import DatabaseConfig
from .errors import E_CONN, E_PLAN_MISSING, E_SQL, E_TIMEOUT, GatewayError
from .plan import PlanInfo, parse_plan


@dataclass(slots=True)
class RawResult:
    columns: list[str]
    rows: list[tuple[Any, ...]]
    plan_xml: str
    plan: PlanInfo


def _pyodbc():
    try:
        import pyodbc
    except ImportError as exc:
        raise GatewayError(
            E_CONN,
            "pyodbc is not installed in the gateway runtime",
            details={"reason": "driver_missing"},
        ) from exc
    pyodbc.pooling = False
    return pyodbc


def connect(config: DatabaseConfig, *, include_database: bool = True):
    pyodbc = _pyodbc()
    try:
        connection = pyodbc.connect(
            config.connection_string(include_database=include_database),
            timeout=config.login_timeout_s,
            autocommit=True,
        )
    except Exception as exc:
        message = re.sub(r"\s+", " ", str(exc)).strip()
        raise GatewayError(
            E_CONN,
            "database is unavailable",
            details={"reason": "connect_failed", "driver_message": message[:500]},
        ) from exc
    return connection


def _db_error(exc: Exception) -> GatewayError:
    message = re.sub(r"\s+", " ", str(exc)).strip()
    sqlstate = ""
    args = getattr(exc, "args", ())
    if args:
        sqlstate = str(args[0])
    code = E_TIMEOUT if sqlstate in {"HYT00", "HYT01", "S1T00"} else E_SQL
    return GatewayError(
        code,
        "SQL Server rejected or could not complete the query",
        details={
            "reason": "database_error",
            "sqlstate": sqlstate,
            "driver_message": message[:500],
        },
    )


class SqlServerDatabase:
    def __init__(self, config: DatabaseConfig, query_timeout_s: int) -> None:
        self.config = config
        self.query_timeout_s = query_timeout_s

    def connect(self):
        return connect(self.config)

    def preflight_plan(self, connection, sql: str) -> PlanInfo:
        connection.timeout = self.query_timeout_s
        cursor = connection.cursor()
        try:
            try:
                cursor.execute("SET SHOWPLAN_XML ON")
                cursor.execute(sql)
                rows = cursor.fetchall()
            finally:
                cursor.execute("SET SHOWPLAN_XML OFF")
        except Exception as exc:
            raise _db_error(exc) from exc
        plan_xml = _plan_xml_from_result(cursor, rows)
        return parse_plan(plan_xml)

    def execute_with_plan(
        self,
        connection,
        sql: str,
        parameters: Iterable[Any] = (),
    ) -> RawResult:
        connection.timeout = self.query_timeout_s
        cursor = connection.cursor()
        rows: list[tuple[Any, ...]] = []
        columns: list[str] = []
        plan_xml: str | None = None
        try:
            try:
                cursor.execute("SET STATISTICS XML ON")
                cursor.execute(sql, *parameters)
                columns = [str(item[0]) for item in cursor.description or []]
                rows = list(cursor.fetchall())
                while cursor.nextset():
                    if cursor.description and "showplan" in str(cursor.description[0][0]).lower():
                        plan_rows = cursor.fetchall()
                        if plan_rows:
                            plan_xml = str(plan_rows[-1][0])
            finally:
                cursor.execute("SET STATISTICS XML OFF")
        except Exception as exc:
            raise _db_error(exc) from exc
        if not plan_xml:
            raise GatewayError(
                E_PLAN_MISSING,
                "SQL Server returned no XML execution plan for the query",
            )
        return RawResult(
            columns=columns,
            rows=rows,
            plan_xml=plan_xml,
            plan=parse_plan(plan_xml),
        )

    def index_state(self, connection, table: str, index_name: str) -> str:
        cursor = connection.cursor()
        try:
            cursor.execute(
                """
                SELECT i.is_disabled
                FROM sys.indexes AS i
                WHERE i.object_id = OBJECT_ID(?)
                  AND i.name = ?
                """,
                f"dbo.{table}",
                index_name,
            )
            row = cursor.fetchone()
        except Exception as exc:
            raise _db_error(exc) from exc
        if row is None:
            return "missing"
        return "disabled" if bool(row[0]) else "enabled"

    def set_index_disabled(
        self,
        connection,
        table: str,
        index_name: str,
        disabled: bool,
    ) -> str:
        current = self.index_state(connection, table, index_name)
        connection.timeout = self.query_timeout_s * 4
        cursor = connection.cursor()
        safe_table = _safe_identifier(table)
        safe_index = _safe_identifier(index_name)
        action = "DISABLE" if disabled else "REBUILD"
        if disabled and current == "disabled":
            return current
        if not disabled and current == "enabled":
            return current
        if current == "missing":
            raise GatewayError(
                E_PLAN_MISSING,
                f"index {index_name} does not exist",
                fragment=index_name,
            )
        try:
            cursor.execute(f"ALTER INDEX {safe_index} ON dbo.{safe_table} {action}")
        except Exception as exc:
            raise _db_error(exc) from exc
        return "disabled" if disabled else "enabled"

    def summary_rows(
        self,
        connection,
        tenant_id: int,
        status: str,
        max_rows: int,
    ):
        sql = """
SELECT TOP (?)
       s.TenantId,
       s.Status,
       s.SummaryDate,
       s.OrderCount,
       s.TotalAmount,
       s.Currency
FROM dbo.OrderSummaryDaily AS s
WHERE s.TenantId = ?
  AND s.Status = ?
ORDER BY s.SummaryDate DESC
""".strip()
        return self.execute_with_plan(
            connection,
            sql,
            (max_rows, tenant_id, status),
        )


def _plan_xml_from_result(cursor, rows) -> str:
    if rows:
        value = rows[-1][0]
        if value is not None:
            return str(value)
    if cursor.description and "showplan" in str(cursor.description[0][0]).lower():
        fetched = cursor.fetchall()
        if fetched:
            return str(fetched[-1][0])
    raise GatewayError(
        E_PLAN_MISSING,
        "SQL Server returned no preflight XML plan",
    )


def _safe_identifier(value: str) -> str:
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]{0,127}", value):
        raise GatewayError(E_SQL, "unsafe SQL identifier in administrative action")
    return f"[{value}]"


def execute_script(connection, script: str) -> None:
    batches = re.split(r"^\s*GO\s*$", script, flags=re.IGNORECASE | re.MULTILINE)
    connection.timeout = 600
    cursor = connection.cursor()
    for batch in batches:
        if not batch.strip():
            continue
        try:
            cursor.execute(batch)
            while cursor.nextset():
                pass
        except Exception as exc:
            raise _db_error(exc) from exc
