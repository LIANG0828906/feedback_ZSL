from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


def _env(name: str | None, default: str | None = None) -> str | None:
    if not name:
        return default
    value = os.environ.get(name)
    return value if value is not None else default


@dataclass(slots=True)
class DatabaseConfig:
    driver: str
    server: str
    database: str
    auth: str = "sql"
    username: str | None = None
    password: str | None = None
    username_env: str | None = None
    password_env: str | None = None
    trusted_connection: bool = False
    encrypt: str = "yes"
    trust_server_certificate: bool = True
    login_timeout_s: int = 3

    def resolved_credentials(self) -> tuple[str | None, str | None]:
        username = self.username
        password = self.password
        if self.username_env:
            username = _env(self.username_env, username)
        if self.password_env:
            password = _env(self.password_env, password)
        return username, password

    def connection_string(self, include_database: bool = True) -> str:
        parts = [
            f"DRIVER={{{self.driver}}}",
            f"SERVER={self.server}",
            f"Encrypt={self.encrypt}",
            f"TrustServerCertificate={'yes' if self.trust_server_certificate else 'no'}",
            f"Connection Timeout={self.login_timeout_s}",
        ]
        if include_database:
            parts.append(f"DATABASE={self.database}")
        if self.auth == "windows" or self.trusted_connection:
            parts.append("Trusted_Connection=yes")
        else:
            username, password = self.resolved_credentials()
            if not username or password is None:
                raise ValueError("SQL authentication requires username and password")
            parts.append(f"UID={username}")
            parts.append(f"PWD={password}")
        return ";".join(parts) + ";"


@dataclass(slots=True)
class IndexSpec:
    name: str
    table: str
    key_columns: list[str]
    include_columns: list[str] = field(default_factory=list)

    @property
    def covered_columns(self) -> set[str]:
        return {column.lower() for column in self.key_columns + self.include_columns}

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "table": self.table,
            "key_columns": list(self.key_columns),
            "include_columns": list(self.include_columns),
        }


@dataclass(slots=True)
class TableSpec:
    name: str
    columns: list[str]
    nullable_columns: list[str] = field(default_factory=list)


@dataclass(slots=True)
class LimitConfig:
    max_connections: int = 4
    per_tenant_connections: int = 2
    acquire_timeout_ms: int = 250
    query_timeout_s: int = 8
    summary_max_rows: int = 6


@dataclass(slots=True)
class FallbackConfig:
    default: str = "summary"
    allowed: list[str] = field(default_factory=lambda: ["summary", "reject"])


@dataclass(slots=True)
class ReplayConfig:
    samples_path: str = "config/samples.json"
    concurrency: int = 2
    max_concurrency: int = 2


@dataclass(slots=True)
class ServerConfig:
    host: str = "127.0.0.1"
    port: int = 8780


@dataclass(slots=True)
class GatewayConfig:
    server: ServerConfig
    database: DatabaseConfig
    admin: DatabaseConfig
    limits: LimitConfig
    fallback: FallbackConfig
    replay: ReplayConfig
    tables: dict[str, TableSpec]
    indexes: list[IndexSpec]
    nullable_columns: set[str]
    string_columns: set[str]
    config_path: Path
    repo_root: Path

    @property
    def samples_path(self) -> Path:
        path = Path(self.replay.samples_path)
        return path if path.is_absolute() else self.repo_root / path

    def index_by_name(self) -> dict[str, IndexSpec]:
        return {index.name.lower(): index for index in self.indexes}

    def indexes_for_table(self, table: str) -> list[IndexSpec]:
        return [index for index in self.indexes if index.table.lower() == table.lower()]


def _db_config(data: dict[str, Any], default_db: str) -> DatabaseConfig:
    return DatabaseConfig(
        driver=data.get("driver", "ODBC Driver 18 for SQL Server"),
        server=data.get("server", "localhost"),
        database=data.get("database", default_db),
        auth=data.get("auth", "sql"),
        username=data.get("username"),
        password=data.get("password"),
        username_env=data.get("username_env"),
        password_env=data.get("password_env"),
        trusted_connection=bool(data.get("trusted_connection", False)),
        encrypt=data.get("encrypt", "yes"),
        trust_server_certificate=bool(data.get("trust_server_certificate", True)),
        login_timeout_s=int(data.get("login_timeout_s", 3)),
    )


def load_config(path: str | Path) -> GatewayConfig:
    config_path = Path(path).resolve()
    data = json.loads(config_path.read_text(encoding="utf-8"))
    database = _db_config(data["database"], "OrderGateway280")
    admin_data = data.get("admin", data["database"])
    admin = _db_config(admin_data, database.database)
    admin.database = admin_data.get("admin_database", "master")

    tables: dict[str, TableSpec] = {}
    for table_data in data.get("tables", []):
        spec = TableSpec(
            name=table_data["name"],
            columns=list(table_data["columns"]),
            nullable_columns=list(table_data.get("nullable_columns", [])),
        )
        tables[spec.name.lower()] = spec

    indexes = [
        IndexSpec(
            name=item["name"],
            table=item["table"],
            key_columns=list(item["key_columns"]),
            include_columns=list(item.get("include_columns", [])),
        )
        for item in data.get("indexes", [])
    ]
    limits = LimitConfig(**data.get("limits", {}))
    fallback = FallbackConfig(**data.get("fallback", {}))
    replay = ReplayConfig(**data.get("replay", {}))
    server_data = dict(data.get("server", {}))
    if os.environ.get("ORDERLAB_HOST"):
        server_data["host"] = os.environ["ORDERLAB_HOST"]
    if os.environ.get("ORDERLAB_PORT"):
        server_data["port"] = int(os.environ["ORDERLAB_PORT"])
    server = ServerConfig(**server_data)
    if limits.max_connections < 1:
        raise ValueError("limits.max_connections must be positive")
    if limits.per_tenant_connections < 1:
        raise ValueError("limits.per_tenant_connections must be positive")
    if limits.per_tenant_connections > limits.max_connections:
        raise ValueError("per-tenant connection limit cannot exceed the global limit")

    nullable = {column.lower() for table in tables.values() for column in table.nullable_columns}
    nullable.update(column.lower() for column in data.get("nullable_columns", []))
    strings = {column.lower() for column in data.get("string_columns", [])}

    return GatewayConfig(
        server=server,
        database=database,
        admin=admin,
        limits=limits,
        fallback=fallback,
        replay=replay,
        tables=tables,
        indexes=indexes,
        nullable_columns=nullable,
        string_columns=strings,
        config_path=config_path,
        repo_root=config_path.parent.parent,
    )
