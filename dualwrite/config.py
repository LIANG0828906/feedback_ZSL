from __future__ import annotations

import json
import os
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any

from .errors import ConfigurationError


@dataclass(frozen=True)
class SiteConfig:
    name: str
    dsn: str
    priority: int


@dataclass(frozen=True)
class InterruptionConfig:
    start_after_seconds: float | None = None
    duration_seconds: float = 0.0
    repeat_every_seconds: float = 0.0


@dataclass(frozen=True)
class InjectionConfig:
    enabled: bool = False
    delay_min_ms: int = 0
    delay_max_ms: int = 0
    reorder_window_ms: int = 0
    duplicate_rate: float = 0.0
    seed: int = 20261119
    interruption: InterruptionConfig = field(default_factory=InterruptionConfig)


@dataclass(frozen=True)
class LimitsConfig:
    max_connections: int = 8
    api_acquire_timeout_ms: int = 50
    sync_acquire_timeout_ms: int = 2000
    statement_timeout_ms: int = 5000


@dataclass(frozen=True)
class SyncConfig:
    poll_interval_ms: int = 20
    fetch_size: int = 100
    convergence_timeout_seconds: int = 120


@dataclass(frozen=True)
class AuditConfig:
    sqlite_path: str = "var/dualwrite.sqlite3"
    recent_change_limit: int = 50


@dataclass(frozen=True)
class ValidationConfig:
    snapshot_chunk_rows: int = 2000
    pending_preview_limit: int = 50


@dataclass(frozen=True)
class Config:
    listen_host: str
    listen_port: int
    sites: dict[str, SiteConfig]
    sync: SyncConfig
    limits: LimitsConfig
    audit: AuditConfig
    injection: InjectionConfig
    validation: ValidationConfig

    @property
    def ordered_sites(self) -> list[SiteConfig]:
        return sorted(self.sites.values(), key=lambda item: item.name)

    @property
    def priorities(self) -> dict[str, int]:
        return {name: site.priority for name, site in self.sites.items()}


def _as_int(value: Any, default: int) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _as_float(value: Any, default: float) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def load_config(path: str | os.PathLike[str] | None = None) -> Config:
    raw: dict[str, Any] = {}
    if path:
        config_path = Path(path)
        if not config_path.exists():
            raise ConfigurationError(f"configuration file not found: {config_path}")
        try:
            raw = json.loads(config_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ConfigurationError(f"invalid configuration: {exc}") from exc

    listen = raw.get("listen", {})
    site_raw = raw.get("sites", {})
    if not isinstance(site_raw, dict) or set(site_raw) != {"east", "south"}:
        site_raw = {
            "east": {
                "dsn": "host=127.0.0.1 port=55432 dbname=postgres user=postgres connect_timeout=2",
                "priority": 20,
            },
            "south": {
                "dsn": "host=127.0.0.1 port=55433 dbname=postgres user=postgres connect_timeout=2",
                "priority": 10,
            },
        }

    sites: dict[str, SiteConfig] = {}
    for site_name in ("east", "south"):
        item = site_raw.get(site_name)
        if not isinstance(item, dict) or not item.get("dsn"):
            raise ConfigurationError(f"sites.{site_name}.dsn is required")
        env_key = f"DUALWRITE_{site_name.upper()}_DSN"
        dsn = os.environ.get(env_key, str(item["dsn"]))
        sites[site_name] = SiteConfig(
            name=site_name,
            dsn=dsn,
            priority=_as_int(item.get("priority"), 10),
        )

    sync_raw = raw.get("sync", {})
    limits_raw = raw.get("limits", {})
    audit_raw = raw.get("audit", {})
    injection_raw = raw.get("injection", {})
    interruption_raw = injection_raw.get("interruption", {})
    validation_raw = raw.get("validation", {})

    sqlite_path = os.environ.get(
        "DUALWRITE_SQLITE_PATH",
        str(audit_raw.get("sqlite_path", "var/dualwrite.sqlite3")),
    )
    max_connections = max(
        2, _as_int(os.environ.get("DUALWRITE_MAX_CONNECTIONS"), _as_int(limits_raw.get("max_connections"), 8))
    )

    config = Config(
        listen_host=str(listen.get("host", "127.0.0.1")),
        listen_port=_as_int(listen.get("port"), 8090),
        sites=sites,
        sync=SyncConfig(
            poll_interval_ms=max(5, _as_int(sync_raw.get("poll_interval_ms"), 20)),
            fetch_size=max(1, _as_int(sync_raw.get("fetch_size"), 100)),
            convergence_timeout_seconds=max(
                1, _as_int(sync_raw.get("convergence_timeout_seconds"), 120)
            ),
        ),
        limits=LimitsConfig(
            max_connections=max_connections,
            api_acquire_timeout_ms=max(
                1, _as_int(limits_raw.get("api_acquire_timeout_ms"), 50)
            ),
            sync_acquire_timeout_ms=max(
                50, _as_int(limits_raw.get("sync_acquire_timeout_ms"), 2000)
            ),
            statement_timeout_ms=max(
                100, _as_int(limits_raw.get("statement_timeout_ms"), 5000)
            ),
        ),
        audit=AuditConfig(
            sqlite_path=sqlite_path,
            recent_change_limit=max(1, _as_int(audit_raw.get("recent_change_limit"), 50)),
        ),
        injection=InjectionConfig(
            enabled=bool(injection_raw.get("enabled", False)),
            delay_min_ms=max(0, _as_int(injection_raw.get("delay_min_ms"), 0)),
            delay_max_ms=max(0, _as_int(injection_raw.get("delay_max_ms"), 0)),
            reorder_window_ms=max(0, _as_int(injection_raw.get("reorder_window_ms"), 0)),
            duplicate_rate=min(
                1.0, max(0.0, _as_float(injection_raw.get("duplicate_rate"), 0.0))
            ),
            seed=_as_int(injection_raw.get("seed"), 20261119),
            interruption=InterruptionConfig(
                start_after_seconds=(
                    None
                    if interruption_raw.get("start_after_seconds") is None
                    else max(0.0, _as_float(interruption_raw.get("start_after_seconds"), 0.0))
                ),
                duration_seconds=max(
                    0.0, _as_float(interruption_raw.get("duration_seconds"), 0.0)
                ),
                repeat_every_seconds=max(
                    0.0, _as_float(interruption_raw.get("repeat_every_seconds"), 0.0)
                ),
            ),
        ),
        validation=ValidationConfig(
            snapshot_chunk_rows=max(
                100, _as_int(validation_raw.get("snapshot_chunk_rows"), 2000)
            ),
            pending_preview_limit=max(
                1, _as_int(validation_raw.get("pending_preview_limit"), 50)
            ),
        ),
    )

    if config.injection.delay_max_ms < config.injection.delay_min_ms:
        config = replace(
            config,
            injection=replace(
                config.injection, delay_max_ms=config.injection.delay_min_ms
            ),
        )
    if config.sites["east"].priority == config.sites["south"].priority:
        raise ConfigurationError("site priorities must be distinct")
    return config

