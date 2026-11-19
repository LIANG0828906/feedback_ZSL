from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import TopologyError


_ENV_PATTERN = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")
_MODES = {"log_shipping", "transactional"}


def _expand_env(value: Any) -> Any:
    if isinstance(value, str):
        def replace(match: re.Match[str]) -> str:
            name = match.group(1)
            if name not in os.environ:
                raise TopologyError(
                    "config_env_missing",
                    f"Required environment variable {name} is not set",
                )
            return os.environ[name]

        return _ENV_PATTERN.sub(replace, value)
    if isinstance(value, list):
        return [_expand_env(item) for item in value]
    if isinstance(value, dict):
        return {key: _expand_env(item) for key, item in value.items()}
    return value


def _as_int(
    raw: dict[str, Any],
    key: str,
    default: int | None = None,
    *,
    minimum: int | None = None,
) -> int:
    value = raw.get(key, default)
    if isinstance(value, bool) or not isinstance(value, int):
        raise TopologyError("config_invalid", f"{key} must be an integer")
    if minimum is not None and value < minimum:
        raise TopologyError(
            "config_invalid", f"{key} must be greater than or equal to {minimum}"
        )
    return value


def _as_str(
    raw: dict[str, Any],
    key: str,
    default: str | None = None,
    *,
    allow_empty: bool = False,
) -> str:
    value = raw.get(key, default)
    if not isinstance(value, str) or (not allow_empty and not value.strip()):
        raise TopologyError("config_invalid", f"{key} must be a non-empty string")
    return value


def _as_bool(raw: dict[str, Any], key: str, default: bool) -> bool:
    value = raw.get(key, default)
    if not isinstance(value, bool):
        raise TopologyError("config_invalid", f"{key} must be a boolean")
    return value


@dataclass(frozen=True)
class EndpointConfig:
    name: str
    connection_string: str
    database: str
    pool_size: int = 8
    pool_acquire_timeout_ms: int = 50
    login_timeout_seconds: int = 5
    query_timeout_seconds: int = 15

    @classmethod
    def parse(cls, name: str, raw: Any) -> "EndpointConfig":
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", f"{name} must be an object")
        return cls(
            name=name,
            connection_string=_as_str(raw, "connection_string"),
            database=_as_str(raw, "database"),
            pool_size=_as_int(raw, "pool_size", 8, minimum=1),
            pool_acquire_timeout_ms=_as_int(
                raw, "pool_acquire_timeout_ms", 50, minimum=0
            ),
            login_timeout_seconds=_as_int(
                raw, "login_timeout_seconds", 5, minimum=1
            ),
            query_timeout_seconds=_as_int(
                raw, "query_timeout_seconds", 15, minimum=1
            ),
        )


@dataclass(frozen=True)
class ReplicationConfig:
    mode: str
    poll_interval_ms: int = 500
    watermark_wait_timeout_ms: int = 2000
    lag_threshold_ms: int = 3000
    lag_window_seconds: int = 30
    sync_stall_ms: int = 10000
    publication: str = "OrderPublication"
    distribution_connection_string: str | None = None
    distribution_database: str = "distribution"
    log_shipping_monitor_database: str = "msdb"

    @classmethod
    def parse(cls, raw: Any) -> "ReplicationConfig":
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", "replication must be an object")
        mode = _as_str(raw, "mode")
        if mode not in _MODES:
            raise TopologyError(
                "replication_mode_invalid",
                "replication.mode must be log_shipping or transactional",
            )
        distribution_cs = raw.get("distribution_connection_string")
        if distribution_cs is not None and not isinstance(distribution_cs, str):
            raise TopologyError(
                "config_invalid",
                "replication.distribution_connection_string must be a string",
            )
        return cls(
            mode=mode,
            poll_interval_ms=_as_int(
                raw, "poll_interval_ms", 500, minimum=100
            ),
            watermark_wait_timeout_ms=_as_int(
                raw, "watermark_wait_timeout_ms", 2000, minimum=100
            ),
            lag_threshold_ms=_as_int(
                raw, "lag_threshold_ms", 3000, minimum=100
            ),
            lag_window_seconds=_as_int(
                raw, "lag_window_seconds", 30, minimum=1
            ),
            sync_stall_ms=_as_int(raw, "sync_stall_ms", 10000, minimum=100),
            publication=_as_str(raw, "publication", "OrderPublication"),
            distribution_connection_string=distribution_cs or None,
            distribution_database=_as_str(
                raw, "distribution_database", "distribution"
            ),
            log_shipping_monitor_database=_as_str(
                raw, "log_shipping_monitor_database", "msdb"
            ),
        )


@dataclass(frozen=True)
class SwitchConfig:
    timeout_ms: int = 15000
    reverse_command: tuple[str, ...] = ()
    rollback_command: tuple[str, ...] = ()
    command_timeout_seconds: int = 180
    allow_logical_switch: bool = False

    @classmethod
    def parse(cls, raw: Any) -> "SwitchConfig":
        if raw is None:
            raw = {}
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", "switch must be an object")

        def command(key: str) -> tuple[str, ...]:
            value = raw.get(key, [])
            if value in (None, []):
                return ()
            if not isinstance(value, list) or not all(
                isinstance(item, str) and item for item in value
            ):
                raise TopologyError(
                    "config_invalid", f"switch.{key} must be an array of strings"
                )
            return tuple(value)

        return cls(
            timeout_ms=_as_int(raw, "timeout_ms", 15000, minimum=100),
            reverse_command=command("reverse_command"),
            rollback_command=command("rollback_command"),
            command_timeout_seconds=_as_int(
                raw, "command_timeout_seconds", 180, minimum=1
            ),
            allow_logical_switch=_as_bool(raw, "allow_logical_switch", False),
        )


@dataclass(frozen=True)
class ReplicationJob:
    endpoint: str
    job_name: str

    @classmethod
    def parse(cls, raw: Any) -> "ReplicationJob":
        if not isinstance(raw, dict):
            raise TopologyError(
                "config_invalid", "faults.replication_jobs entries must be objects"
            )
        endpoint = _as_str(raw, "endpoint")
        if endpoint not in {"primary", "replica"}:
            raise TopologyError(
                "config_invalid",
                "faults.replication_jobs endpoint must be primary or replica",
            )
        return cls(endpoint=endpoint, job_name=_as_str(raw, "job_name"))


@dataclass(frozen=True)
class FaultConfig:
    replication_jobs: tuple[ReplicationJob, ...] = ()
    allow_simulated_replication_stall: bool = False
    disconnect_command: tuple[str, ...] = ()
    reconnect_command: tuple[str, ...] = ()

    @classmethod
    def parse(cls, raw: Any) -> "FaultConfig":
        if raw is None:
            raw = {}
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", "faults must be an object")
        jobs_raw = raw.get("replication_jobs", [])
        if not isinstance(jobs_raw, list):
            raise TopologyError(
                "config_invalid", "faults.replication_jobs must be an array"
            )

        def command(key: str) -> tuple[str, ...]:
            value = raw.get(key, [])
            if value in (None, []):
                return ()
            if not isinstance(value, list) or not all(
                isinstance(item, str) and item for item in value
            ):
                raise TopologyError(
                    "config_invalid", f"faults.{key} must be an array of strings"
                )
            return tuple(value)

        disconnect_command = command("disconnect_command")
        reconnect_command = command("reconnect_command")
        if bool(disconnect_command) != bool(reconnect_command):
            raise TopologyError(
                "config_invalid",
                "faults.disconnect_command and reconnect_command must be configured together",
            )

        return cls(
            replication_jobs=tuple(ReplicationJob.parse(item) for item in jobs_raw),
            allow_simulated_replication_stall=_as_bool(
                raw, "allow_simulated_replication_stall", False
            ),
            disconnect_command=disconnect_command,
            reconnect_command=reconnect_command,
        )


@dataclass(frozen=True)
class ServerConfig:
    host: str = "127.0.0.1"
    port: int = 8080
    request_threads: int = 16
    max_body_bytes: int = 1024 * 1024
    admin_token: str | None = None
    compare_on_replica_read: bool = True
    max_page_size: int = 200
    default_page_size: int = 50

    @classmethod
    def parse(cls, raw: Any) -> "ServerConfig":
        if raw is None:
            raw = {}
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", "server must be an object")
        admin_token = raw.get("admin_token")
        if admin_token is not None and not isinstance(admin_token, str):
            raise TopologyError(
                "config_invalid", "server.admin_token must be a string"
            )
        default_page_size = _as_int(
            raw, "default_page_size", 50, minimum=1
        )
        max_page_size = _as_int(raw, "max_page_size", 200, minimum=1)
        if default_page_size > max_page_size:
            raise TopologyError(
                "config_invalid",
                "server.default_page_size must not exceed max_page_size",
            )
        return cls(
            host=_as_str(raw, "host", "127.0.0.1"),
            port=_as_int(raw, "port", 8080, minimum=1),
            request_threads=_as_int(raw, "request_threads", 16, minimum=1),
            max_body_bytes=_as_int(
                raw, "max_body_bytes", 1024 * 1024, minimum=1024
            ),
            admin_token=admin_token or None,
            compare_on_replica_read=_as_bool(
                raw, "compare_on_replica_read", True
            ),
            max_page_size=max_page_size,
            default_page_size=default_page_size,
        )


@dataclass(frozen=True)
class ServiceConfig:
    primary: EndpointConfig
    replica: EndpointConfig
    replication: ReplicationConfig
    switch: SwitchConfig = field(default_factory=SwitchConfig)
    faults: FaultConfig = field(default_factory=FaultConfig)
    server: ServerConfig = field(default_factory=ServerConfig)

    @classmethod
    def parse(cls, raw: Any) -> "ServiceConfig":
        if not isinstance(raw, dict):
            raise TopologyError("config_invalid", "configuration root must be an object")
        primary = EndpointConfig.parse("primary", raw.get("primary"))
        replica = EndpointConfig.parse("replica", raw.get("replica"))
        replication = ReplicationConfig.parse(raw.get("replication"))
        switch = SwitchConfig.parse(raw.get("switch"))
        faults = FaultConfig.parse(raw.get("faults"))
        server = ServerConfig.parse(raw.get("server"))

        if (
            primary.database.casefold() == replica.database.casefold()
            and primary.connection_string == replica.connection_string
        ):
            raise TopologyError(
                "same_endpoint",
                "Primary and replica must not use the same endpoint and database",
            )
        return cls(
            primary=primary,
            replica=replica,
            replication=replication,
            switch=switch,
            faults=faults,
            server=server,
        )


def load_config(path: str | Path) -> ServiceConfig:
    try:
        raw_text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise TopologyError(
            "config_unreadable", f"Configuration file could not be read: {exc.strerror}"
        ) from None
    try:
        parsed = json.loads(raw_text)
    except json.JSONDecodeError as exc:
        raise TopologyError(
            "config_invalid_json",
            f"Configuration JSON is invalid at line {exc.lineno}, column {exc.colno}",
        ) from None
    return ServiceConfig.parse(_expand_env(parsed))

