from __future__ import annotations

import copy
import os
from pathlib import Path
from typing import Any

from .util import deep_merge, read_json


DEFAULTS: dict[str, Any] = {
    "service": {
        "host": "127.0.0.1",
        "port": 18190,
        "database": "var/connectlab.sqlite3",
        "data_dir": "var",
        "seed": 318,
        "log_level": "INFO",
    },
    "proxy": {
        "mode": "embedded",
        "listen_host": "127.0.0.1",
        "listen_port": 18080,
        "pool_size": 8,
        "connect_timeout_ms": 400,
        "upstream_read_timeout_ms": 1200,
        "downstream_idle_timeout_ms": 8000,
        "stale_disconnect_grace_ms": 20,
        "envoy_bin": "envoy",
        "envoy_config": "config/envoy.yaml",
        "envoy_admin_port": 18082,
    },
    "upstream": {
        "listen_host": "127.0.0.1",
        "listen_port": 18081,
        "read_timeout_ms": 3000,
        "write_delay_ms": 0,
        "early_disconnect_after_response_ms": 15,
        "early_disconnect_before_response_ms": 30,
        "proxy_read_timeout_extra_ms": 250,
        "extra_ports": [],
    },
    "injection": {
        "file": "config/injection.json",
        "default_id": "baseline",
    },
    "batch": {
        "default_count": 24,
        "default_concurrency": 4,
        "max_count": 512,
        "max_concurrency": 16,
        "batch_timeout_ms": 20000,
        "per_hop_timeout_ms": 1500,
        "request_timeout_ms": 5000,
        "keepalive_requests_per_connection": 8,
    },
    "samples": {
        "dir": "data",
        "short_bytes": 256,
        "keepalive_bytes": 64,
        "upload_bytes": 4194304,
        "sequence_file": "data/request_sequence.json",
    },
    "targets": {
        "local-a": {
            "proxy_host": "127.0.0.1",
            "proxy_port": 18080,
            "upstream_host": "127.0.0.1",
            "upstream_port": 18081,
        }
    },
    "default_target": "local-a",
}


def project_root() -> Path:
    configured = os.environ.get("CONNECTLAB_ROOT")
    if configured:
        return Path(configured).resolve()
    return Path(__file__).resolve().parent.parent


_ROOT = project_root()


def set_project_root(path: str | Path) -> None:
    global _ROOT
    _ROOT = Path(path).resolve()


def root_path() -> Path:
    return _ROOT


def resolve_path(value: str) -> Path:
    path = Path(value)
    if not path.is_absolute():
        path = _ROOT / path
    return path


def load_config(path: str | Path | None = None) -> tuple[dict[str, Any], Path]:
    if path is None:
        path = os.environ.get("CONNECTLAB_CONFIG", str(_ROOT / "config" / "connectlab.json"))
    config_path = resolve_path(str(path))
    raw = read_json(config_path, {})
    if raw is None:
        raw = {}
    config = deep_merge(DEFAULTS, raw)
    config["_config_path"] = str(config_path)
    config["_root"] = str(_ROOT)
    return config, config_path


def normalize_config(config: dict[str, Any]) -> dict[str, Any]:
    result = copy.deepcopy(config)
    result.pop("_config_path", None)
    result.pop("_root", None)
    return result


def database_path(config: dict[str, Any]) -> Path:
    return resolve_path(str(config["service"]["database"]))


def injection_path(config: dict[str, Any]) -> Path:
    return resolve_path(str(config["injection"]["file"]))


def sample_path(config: dict[str, Any], name: str) -> Path:
    return resolve_path(str(Path(config["samples"]["dir"]) / name))


def target_profile(config: dict[str, Any], name: str | None) -> tuple[str, dict[str, Any]]:
    profile_name = name or config.get("default_target") or "local-a"
    targets = config.get("targets", {})
    if profile_name not in targets:
        raise ValueError(f"unknown target profile: {profile_name}")
    target = targets[profile_name]
    for key in ("proxy_host", "proxy_port", "upstream_host", "upstream_port"):
        if key not in target:
            raise ValueError(f"target profile {profile_name} is missing {key}")
    return profile_name, target
