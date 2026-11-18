from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .config import GatewayConfig, load_config
from .db import SqlServerDatabase, connect, execute_script
from .engine import QueryEngine, QueryRequest
from .errors import E_REQUEST, GatewayError
from .replay import ReplayRunner, compare_reports, write_report
from .server import serve


def _json_print(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _error_print(exc: GatewayError) -> int:
    _json_print({"ok": False, "error": exc.to_dict()})
    return 2


def _placeholders(text: str, config: GatewayConfig) -> str:
    username, password = config.database.resolved_credentials()
    return (
        text.replace("{{DATABASE}}", config.database.database)
        .replace("{{GATEWAY_LOGIN}}", username or "")
        .replace("{{GATEWAY_PASSWORD}}", (password or "").replace("'", "''"))
    )


def _safe_sql_identifier(value: str, label: str) -> str:
    import re

    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]{0,127}", value):
        raise GatewayError(E_REQUEST, f"invalid {label}")
    return value


def command_init_db(config: GatewayConfig) -> dict[str, Any]:
    root = config.repo_root
    _safe_sql_identifier(config.database.database, "database name")
    admin = config.admin
    admin_connection = connect(admin)
    try:
        execute_script(admin_connection, _placeholders((root / "sql/00_database.sql").read_text(), config))
        if config.database.auth == "sql":
            username, password = config.database.resolved_credentials()
            if not username or password is None:
                raise GatewayError(E_REQUEST, "gateway SQL login credentials are missing")
            _safe_sql_identifier(username, "gateway login")
            escaped = password.replace("'", "''")
            login_sql = f"""
IF EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'{username}')
    ALTER LOGIN [{username}] WITH PASSWORD = N'{escaped}';
ELSE
    CREATE LOGIN [{username}] WITH PASSWORD = N'{escaped}', CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF;
"""
            execute_script(admin_connection, login_sql)
    finally:
        admin_connection.close()

    target = replace(admin, database=config.database.database)
    database_connection = connect(target)
    try:
        for relative in ("sql/10_schema.sql", "sql/20_seed.sql", "sql/30_indexes_summary.sql"):
            script = _placeholders((root / relative).read_text(encoding="utf-8"), config)
            execute_script(database_connection, script)
        if config.database.auth == "sql":
            script = _placeholders(
                (root / "sql/40_security.sql").read_text(encoding="utf-8"),
                config,
            )
            execute_script(database_connection, script)
        cursor = database_connection.cursor()
        cursor.execute("SELECT COUNT_BIG(*) FROM dbo.OrderHeader")
        headers = int(cursor.fetchone()[0])
        cursor.execute("SELECT COUNT_BIG(*) FROM dbo.OrderLine")
        lines = int(cursor.fetchone()[0])
        cursor.execute("SELECT COUNT_BIG(*) FROM dbo.OrderSummaryDaily")
        summaries = int(cursor.fetchone()[0])
    finally:
        database_connection.close()
    return {
        "ok": True,
        "database": config.database.database,
        "order_headers": headers,
        "order_lines": lines,
        "summary_rows": summaries,
        "app_login": config.database.username if config.database.auth == "sql" else "windows",
    }


def command_index(config: GatewayConfig, index_name: str, state: str | None) -> dict[str, Any]:
    index_spec = config.index_by_name().get(index_name.lower())
    if index_spec is None:
        raise GatewayError(E_REQUEST, "index is not declared in the gateway configuration")
    admin = replace(config.admin, database=config.database.database)
    connection = connect(admin)
    database = SqlServerDatabase(admin, config.limits.query_timeout_s)
    try:
        if state is None:
            current = database.index_state(connection, index_spec.table, index_spec.name)
        else:
            if state not in {"disabled", "enabled"}:
                raise GatewayError(E_REQUEST, "index state must be disabled or enabled")
            current = database.set_index_disabled(
                connection,
                index_spec.table,
                index_spec.name,
                disabled=state == "disabled",
            )
    finally:
        connection.close()
    return {
        "ok": True,
        "index": index_spec.name,
        "table": index_spec.table,
        "state": current,
    }


def command_query(config: GatewayConfig, args: argparse.Namespace) -> dict[str, Any]:
    sql = args.sql
    if args.sql_file:
        sql = Path(args.sql_file).read_text(encoding="utf-8")
    if sql is None:
        raise GatewayError(E_REQUEST, "provide --sql or --sql-file")
    engine = QueryEngine(config)
    return engine.query(
        QueryRequest(
            sql=sql,
            dialect=args.dialect,
            query_id=args.query_id,
            fallback=args.fallback,
            tenant=args.tenant,
        )
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="orderlab-gateway")
    parser.add_argument("--config", default="config/gateway.json")
    subparsers = parser.add_subparsers(dest="command", required=True)

    subparsers.add_parser("init-db")
    subparsers.add_parser("serve")

    query = subparsers.add_parser("query")
    query.add_argument("--dialect", required=True)
    query.add_argument("--sql")
    query.add_argument("--sql-file")
    query.add_argument("--query-id")
    query.add_argument("--fallback")
    query.add_argument("--tenant")

    replay = subparsers.add_parser("replay")
    replay.add_argument("--output", default="artifacts/replay.json")
    replay.add_argument("--concurrency", type=int)
    replay.add_argument("--repeats", type=int, default=1)

    index = subparsers.add_parser("index")
    index.add_argument("--name", required=True)
    index.add_argument("--state", choices=["enabled", "disabled"])

    compare = subparsers.add_parser("compare")
    compare.add_argument("--baseline", default="artifacts/replay_baseline.json")
    compare.add_argument("--disabled", default="artifacts/replay_index_disabled.json")
    compare.add_argument("--restored", default="artifacts/replay_restored.json")
    compare.add_argument("--output", default="artifacts/replay_comparison.json")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config)
        if args.command == "init-db":
            result = command_init_db(config)
        elif args.command == "serve":
            serve(config)
            return 0
        elif args.command == "query":
            result = command_query(config, args)
        elif args.command == "replay":
            engine = QueryEngine(config)
            result = ReplayRunner(config, engine).run(
                concurrency=args.concurrency,
                repeats=args.repeats,
            )
            write_report(result, args.output)
        elif args.command == "index":
            result = command_index(config, args.name, args.state)
        elif args.command == "compare":
            baseline = json.loads(Path(args.baseline).read_text(encoding="utf-8"))
            disabled = json.loads(Path(args.disabled).read_text(encoding="utf-8"))
            restored = json.loads(Path(args.restored).read_text(encoding="utf-8"))
            result = compare_reports(baseline, disabled, restored)
            write_report(result, args.output)
        else:
            raise GatewayError(E_REQUEST, "unknown command")
    except GatewayError as exc:
        return _error_print(exc)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        return _error_print(GatewayError(E_REQUEST, str(exc)))
    _json_print(result)
    if args.command == "query" and not result.get("ok", False):
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
