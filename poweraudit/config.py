"""Configuration loading with path resolution relative to the config file."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class ConfigError(ValueError):
    pass


def _number(raw: dict[str, Any], key: str, default: float, minimum: float = 0.0) -> float:
    value = raw.get(key, default)
    if not isinstance(value, (int, float)) or isinstance(value, bool) or value < minimum:
        raise ConfigError(f"{key} must be a number >= {minimum}")
    return float(value)


@dataclass(frozen=True)
class AppConfig:
    source_path: Path
    base_dir: Path
    inventory_path: Path
    scenario_path: Path
    database_path: Path
    http_host: str
    http_port: int
    request_timeout_seconds: float
    device_timeout_seconds: float
    total_timeout_seconds: float
    retries: int
    max_workers: int
    max_varbinds_per_request: int
    emergency_battery_threshold_percent: float
    emergency_runtime_threshold_minutes: float
    failover_reserve_percent: float
    min_transfer_load_percent: float
    simulator_backend: str
    raw: dict[str, Any]

    @classmethod
    def load(cls, path: str | os.PathLike[str]) -> "AppConfig":
        source = Path(path).expanduser().resolve()
        try:
            raw = json.loads(source.read_text(encoding="utf-8"))
        except FileNotFoundError as exc:
            raise ConfigError(f"config file not found: {path}") from exc
        except json.JSONDecodeError as exc:
            raise ConfigError(f"invalid config JSON: {exc}") from exc
        if not isinstance(raw, dict):
            raise ConfigError("config root must be an object")
        base = source.parent

        def resolve(value: Any, default: str) -> Path:
            text = value if isinstance(value, str) and value else default
            candidate = Path(text).expanduser()
            return candidate if candidate.is_absolute() else (base / candidate).resolve()

        http = raw.get("http", {})
        database = raw.get("database", {})
        snmp = raw.get("snmp", {})
        emergency = raw.get("emergency", {})
        simulator = raw.get("simulator", {})
        if not isinstance(http, dict) or not isinstance(database, dict) or not isinstance(snmp, dict) or not isinstance(emergency, dict):
            raise ConfigError("http, database, snmp, and emergency must be objects")
        if not isinstance(simulator, dict):
            raise ConfigError("simulator must be an object")

        port = http.get("port", 8080)
        if not isinstance(port, int) or isinstance(port, bool) or not (0 <= port <= 65535):
            raise ConfigError("http.port must be an integer from 0 to 65535")
        max_workers = int(_number(snmp, "max_workers", 8, 1))
        max_varbinds = int(_number(snmp, "max_varbinds_per_request", 24, 1))
        retries = int(_number(snmp, "retries", 0, 0))
        return cls(
            source_path=source,
            base_dir=base,
            inventory_path=resolve(raw.get("inventory_path"), "data/asset_inventory.json"),
            scenario_path=resolve(raw.get("scenario_path"), "data/simulator_scenario.json"),
            database_path=resolve(database.get("path"), "runtime/power_audit.db"),
            http_host=str(http.get("host", "127.0.0.1")),
            http_port=port,
            request_timeout_seconds=_number(snmp, "request_timeout_seconds", 0.25, 0.01),
            device_timeout_seconds=_number(snmp, "device_timeout_seconds", 1.0, 0.02),
            total_timeout_seconds=_number(snmp, "total_timeout_seconds", 8.0, 0.05),
            retries=retries,
            max_workers=max_workers,
            max_varbinds_per_request=max_varbinds,
            emergency_battery_threshold_percent=_number(
                emergency, "battery_threshold_percent", 25.0, 0.0
            ),
            emergency_runtime_threshold_minutes=_number(
                emergency, "runtime_threshold_minutes", 20.0, 0.0
            ),
            failover_reserve_percent=_number(emergency, "failover_reserve_percent", 10.0, 0.0),
            min_transfer_load_percent=_number(emergency, "min_transfer_load_percent", 1.0, 0.0),
            simulator_backend=str(simulator.get("backend", "auto")).lower(),
            raw=raw,
        )

    def public_dict(self) -> dict[str, Any]:
        """Configuration values safe for status responses (no absolute paths)."""
        return {
            "http": {"host": self.http_host, "port": self.http_port},
            "snmp": {
                "request_timeout_seconds": self.request_timeout_seconds,
                "device_timeout_seconds": self.device_timeout_seconds,
                "total_timeout_seconds": self.total_timeout_seconds,
                "retries": self.retries,
                "max_workers": self.max_workers,
                "max_varbinds_per_request": self.max_varbinds_per_request,
            },
            "emergency": {
                "battery_threshold_percent": self.emergency_battery_threshold_percent,
                "runtime_threshold_minutes": self.emergency_runtime_threshold_minutes,
                "failover_reserve_percent": self.failover_reserve_percent,
                "min_transfer_load_percent": self.min_transfer_load_percent,
            },
            "simulator": {"backend": self.simulator_backend},
        }
