from __future__ import annotations

import copy
import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


DEFAULT_CONFIG: dict[str, Any] = {
    "server": {
        "host": "127.0.0.1",
        "port": 8090,
        "request_timeout_seconds": 15,
    },
    "storage": {
        "path": "data/coordinator.sqlite3",
    },
    "kafka": {
        "bootstrap_servers": "127.0.0.1:9092",
        "topic": "order-events",
        "partitions": 12,
        "client_id": "order-group-coordinator",
        "request_timeout_ms": 5000,
        "metadata_refresh_ms": 5000,
    },
    "backend": {
        "kind": "kafka",
        "simulator_dir": "data/simulated",
    },
    "group": {
        "group_id": "order-events-consumers",
        "default_session_timeout_ms": 10000,
        "default_heartbeat_interval_ms": 1000,
        "max_fetch_records": 250,
        "max_fetch_bytes": 1048576,
        "sweep_interval_ms": 50,
    },
    "rebalance": {
        "max_partitions_per_rebalance": 6,
        "cooldown_ms": 5000,
        "partition_threshold_delay_ms": 1500,
        "suppression_max_delay_ms": 8000,
        "revoke_ack_timeout_ms": 2500,
        "initial_assignment_immediate": True,
    },
    "metrics": {
        "lag_refresh_ms": 1000,
    },
    "worker": {
        "batch_size": 250,
        "heartbeat_interval_ms": 1000,
        "session_timeout_ms": 10000,
        "consume_delay_ms": 0,
        "poll_retry_delay_ms": 100,
        "commit_each_batch": True,
    },
}


def _deep_merge(base: dict[str, Any], update: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(base)
    for key, value in update.items():
        if isinstance(value, dict) and isinstance(result.get(key), dict):
            result[key] = _deep_merge(result[key], value)
        else:
            result[key] = value
    return result


def _env_value(name: str) -> Any:
    raw = os.environ.get(name)
    if raw is None:
        return None
    lowered = raw.lower()
    if lowered in {"true", "false"}:
        return lowered == "true"
    try:
        return int(raw)
    except ValueError:
        return raw


def _apply_environment(config: dict[str, Any]) -> None:
    mappings = {
        "ORDERGROUP_HOST": ("server", "host"),
        "ORDERGROUP_PORT": ("server", "port"),
        "ORDERGROUP_DB": ("storage", "path"),
        "ORDERGROUP_BACKEND": ("backend", "kind"),
        "ORDERGROUP_SIMULATOR_DIR": ("backend", "simulator_dir"),
        "ORDERGROUP_BOOTSTRAP": ("kafka", "bootstrap_servers"),
        "ORDERGROUP_TOPIC": ("kafka", "topic"),
        "ORDERGROUP_PARTITIONS": ("kafka", "partitions"),
        "ORDERGROUP_MAX_REBALANCE_PARTITIONS": (
            "rebalance",
            "max_partitions_per_rebalance",
        ),
        "ORDERGROUP_COOLDOWN_MS": ("rebalance", "cooldown_ms"),
        "ORDERGROUP_REVOKE_ACK_TIMEOUT_MS": (
            "rebalance",
            "revoke_ack_timeout_ms",
        ),
    }
    for env_name, (section, key) in mappings.items():
        value = _env_value(env_name)
        if value is not None:
            config[section][key] = value


@dataclass
class Config:
    data: dict[str, Any] = field(default_factory=lambda: copy.deepcopy(DEFAULT_CONFIG))
    source: str | None = None

    @classmethod
    def load(cls, path: str | os.PathLike[str] | None = None) -> "Config":
        data = copy.deepcopy(DEFAULT_CONFIG)
        source = None
        candidate = path or os.environ.get("ORDERGROUP_CONFIG")
        if candidate:
            config_path = Path(candidate)
            with config_path.open("r", encoding="utf-8") as handle:
                loaded = json.load(handle)
            if not isinstance(loaded, dict):
                raise ValueError("configuration root must be a JSON object")
            data = _deep_merge(data, loaded)
            source = str(config_path)
        _apply_environment(data)
        return cls(data=data, source=source)

    def section(self, name: str) -> dict[str, Any]:
        value = self.data.get(name, {})
        if not isinstance(value, dict):
            raise ValueError(f"configuration section {name!r} must be an object")
        return value

    def get(self, section: str, key: str, default: Any = None) -> Any:
        return self.section(section).get(key, default)

    def resolve_path(self, value: str | os.PathLike[str]) -> Path:
        path = Path(value)
        if path.is_absolute():
            return path
        if self.source:
            return Path(self.source).resolve().parent / path
        return Path.cwd() / path

    @property
    def host(self) -> str:
        return str(self.get("server", "host", "127.0.0.1"))

    @property
    def port(self) -> int:
        return int(self.get("server", "port", 8090))

    @property
    def db_path(self) -> Path:
        return self.resolve_path(str(self.get("storage", "path", "data/coordinator.sqlite3")))

    @property
    def topic(self) -> str:
        return str(self.get("kafka", "topic", "order-events"))

    @property
    def partitions(self) -> int:
        return int(self.get("kafka", "partitions", 12))

    @property
    def backend_kind(self) -> str:
        return str(self.get("backend", "kind", "kafka")).lower()

    @property
    def simulator_dir(self) -> Path:
        return self.resolve_path(str(self.get("backend", "simulator_dir", "data/simulated")))

