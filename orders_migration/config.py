from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any


_ENV_PATTERN = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")


def _expand(value: str) -> str:
    def replace(match: re.Match[str]) -> str:
        name = match.group(1)
        if name not in os.environ:
            raise ValueError(f"environment variable {name} is not set")
        return os.environ[name]

    return _ENV_PATTERN.sub(replace, value)


def _number(value: Any, name: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{name} must be numeric")
    if minimum is not None and value < minimum:
        raise ValueError(f"{name} must be >= {minimum}")
    return float(value)


@dataclass(frozen=True)
class DatabaseConfig:
    driver: str
    legacy_connection_string: str
    shard_connection_string_template: str
    shard_count: int
    connect_timeout_seconds: float
    command_timeout_seconds: float

    def shard_connection_string(self, shard: int) -> str:
        if not 0 <= shard < self.shard_count:
            raise ValueError(f"shard index {shard} outside 0..{self.shard_count - 1}")
        template = _expand(self.shard_connection_string_template)
        if "{shard}" not in template:
            raise ValueError(
                "shard_connection_string_template must contain {shard}"
            )
        return template.replace("{shard}", str(shard))


@dataclass(frozen=True)
class PoolConfig:
    max_connections: int
    acquire_timeout_ms: int
    idle_ttl_seconds: float


@dataclass(frozen=True)
class MigrationConfig:
    batch_size: int
    read_mode_during_migration: str
    resume: bool


@dataclass(frozen=True)
class ReconciliationConfig:
    batch_size: int
    in_flight_retry_ms: int
    in_flight_retry_limit: int


@dataclass(frozen=True)
class RollbackConfig:
    wait_timeout_seconds: float
    batch_size: int


@dataclass(frozen=True)
class LoadConfig:
    target_writes_per_second: float
    target_reads_per_second: float
    write_p95_ms: float
    read_p95_ms: float


@dataclass(frozen=True)
class Config:
    path: Path
    host: str
    port: int
    database: DatabaseConfig
    pool: PoolConfig
    migration: MigrationConfig
    reconciliation: ReconciliationConfig
    rollback: RollbackConfig
    sqlite_path: Path
    load_limits: LoadConfig
    fault_injection_enabled: bool
    admin_token: str | None

    @property
    def shard_count(self) -> int:
        return self.database.shard_count

    @classmethod
    def load(cls, path: str | Path) -> "Config":
        config_path = Path(path).resolve()
        try:
            raw = json.loads(config_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ValueError(f"cannot read configuration {config_path}: {exc}") from exc
        if not isinstance(raw, dict):
            raise ValueError("configuration root must be an object")

        listen = raw.get("listen", {})
        db = raw.get("database", {})
        pool = raw.get("connection_pool", {})
        migration = raw.get("migration", {})
        recon = raw.get("reconciliation", {})
        rollback = raw.get("rollback", {})
        audit = raw.get("audit", {})
        load = raw.get("load", {})
        faults = raw.get("fault_injection", {})

        legacy_cs = _expand(str(db.get("legacy_connection_string", ""))).strip()
        shard_cs = _expand(str(db.get("shard_connection_string_template", ""))).strip()
        if not legacy_cs:
            raise ValueError("database.legacy_connection_string is required")
        if not shard_cs:
            raise ValueError("database.shard_connection_string_template is required")

        shard_count = int(db.get("shard_count", 4))
        if not 1 <= shard_count <= 64:
            raise ValueError("database.shard_count must be between 1 and 64")

        driver = str(db.get("driver", "auto")).lower()
        if driver not in {"auto", "pyodbc", "pymssql"}:
            raise ValueError("database.driver must be auto, pyodbc, or pymssql")

        read_mode = str(migration.get("read_mode_during_migration", "legacy")).lower()
        if read_mode not in {"legacy", "shard"}:
            raise ValueError(
                "migration.read_mode_during_migration must be legacy or shard"
            )

        max_connections = int(pool.get("max_connections", 12))
        if not 1 <= max_connections <= 64:
            raise ValueError("connection_pool.max_connections must be between 1 and 64")

        sqlite_value = str(audit.get("sqlite_path", "runtime/audit.db"))
        sqlite_path = Path(sqlite_value)
        if not sqlite_path.is_absolute():
            sqlite_path = (config_path.parent / sqlite_path).resolve()

        return cls(
            path=config_path,
            host=str(listen.get("host", "127.0.0.1")),
            port=int(listen.get("port", 8090)),
            database=DatabaseConfig(
                driver=driver,
                legacy_connection_string=legacy_cs,
                shard_connection_string_template=shard_cs,
                shard_count=shard_count,
                connect_timeout_seconds=_number(
                    db.get("connect_timeout_seconds", 5),
                    "database.connect_timeout_seconds",
                    0.1,
                ),
                command_timeout_seconds=_number(
                    db.get("command_timeout_seconds", 20),
                    "database.command_timeout_seconds",
                    0.1,
                ),
            ),
            pool=PoolConfig(
                max_connections=max_connections,
                acquire_timeout_ms=int(pool.get("acquire_timeout_ms", 250)),
                idle_ttl_seconds=_number(
                    pool.get("idle_ttl_seconds", 60),
                    "connection_pool.idle_ttl_seconds",
                    0,
                ),
            ),
            migration=MigrationConfig(
                batch_size=max(1, int(migration.get("batch_size", 500))),
                read_mode_during_migration=read_mode,
                resume=bool(migration.get("resume", True)),
            ),
            reconciliation=ReconciliationConfig(
                batch_size=max(1, int(recon.get("batch_size", 500))),
                in_flight_retry_ms=max(0, int(recon.get("in_flight_retry_ms", 120))),
                in_flight_retry_limit=max(0, int(recon.get("in_flight_retry_limit", 8))),
            ),
            rollback=RollbackConfig(
                wait_timeout_seconds=_number(
                    rollback.get("wait_timeout_seconds", 30),
                    "rollback.wait_timeout_seconds",
                    0,
                ),
                batch_size=max(1, int(rollback.get("batch_size", 500))),
            ),
            sqlite_path=sqlite_path,
            load_limits=LoadConfig(
                target_writes_per_second=_number(
                    load.get("target_writes_per_second", 20),
                    "load.target_writes_per_second",
                    0,
                ),
                target_reads_per_second=_number(
                    load.get("target_reads_per_second", 30),
                    "load.target_reads_per_second",
                    0,
                ),
                write_p95_ms=_number(
                    load.get("write_p95_ms", 250), "load.write_p95_ms", 0
                ),
                read_p95_ms=_number(
                    load.get("read_p95_ms", 120), "load.read_p95_ms", 0
                ),
            ),
            fault_injection_enabled=bool(faults.get("enabled", False)),
            admin_token=(
                _expand(str(faults["admin_token"]))
                if faults.get("admin_token")
                else None
            ),
        )
