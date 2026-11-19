from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


def _resolve_path(base: Path, value: str) -> Path:
    path = Path(value)
    return path if path.is_absolute() else (base / path).resolve()


@dataclass(frozen=True)
class ModelConfig:
    version: str
    path: Path
    protocol_version: str
    role: str = "canary"


@dataclass(frozen=True)
class ProtocolConfig:
    version: str
    feature_names: tuple[str, ...]


@dataclass(frozen=True)
class RoutingConfig:
    default_weights: dict[str, float]
    tenant_weights: dict[str, dict[str, float]]
    client_weights: dict[str, dict[str, dict[str, float]]]
    bucket_count: int
    hash_seed: str


@dataclass(frozen=True)
class MonitoringConfig:
    window_seconds: float
    min_samples: int
    p99_latency_ms: float
    error_rate: float
    evaluate_interval_ms: int
    request_timeout_ms: int
    auto_rollback_versions: tuple[str, ...]


@dataclass(frozen=True)
class LoadingConfig:
    ready_timeout_ms: int
    retain_previous_on_reload_failure: bool


@dataclass(frozen=True)
class AppConfig:
    base_dir: Path
    service: dict[str, Any]
    models: dict[str, ModelConfig]
    protocols: dict[str, ProtocolConfig]
    stable_version: str
    routing: RoutingConfig
    monitoring: MonitoringConfig
    loading: LoadingConfig
    fault_injection_enabled: bool
    startup_failures: frozenset[str] = field(default_factory=frozenset)


def _float_weights(value: Any, label: str) -> dict[str, float]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be an object")
    result: dict[str, float] = {}
    for version, weight in value.items():
        number = float(weight)
        if number < 0:
            raise ValueError(f"{label}.{version} must be non-negative")
        result[str(version)] = number
    return result


def load_config(path: str | Path) -> AppConfig:
    config_path = Path(path).resolve()
    raw = json.loads(config_path.read_text(encoding="utf-8"))
    base_dir = config_path.parent

    models: dict[str, ModelConfig] = {}
    for version, item in raw.get("models", {}).items():
        models[str(version)] = ModelConfig(
            version=str(version),
            path=_resolve_path(base_dir, item["path"]),
            protocol_version=str(item["protocol_version"]),
            role=str(item.get("role", "canary")),
        )
    if not models:
        raise ValueError("at least one model version is required")

    protocols: dict[str, ProtocolConfig] = {}
    for version, item in raw.get("protocols", {}).items():
        names = tuple(str(name) for name in item.get("feature_names", []))
        if not names:
            raise ValueError(f"protocol {version} has no feature_names")
        if len(set(names)) != len(names):
            raise ValueError(f"protocol {version} contains duplicate feature names")
        protocols[str(version)] = ProtocolConfig(version=str(version), feature_names=names)
    for model in models.values():
        if model.protocol_version not in protocols:
            raise ValueError(
                f"model {model.version} references unknown protocol {model.protocol_version}"
            )

    stable_version = str(raw.get("stable_version", next(iter(models))))
    if stable_version not in models:
        raise ValueError(f"stable_version {stable_version} is not a configured model")

    routing_raw = raw.get("routing", {})
    default_weights = _float_weights(
        routing_raw.get("default_weights", {stable_version: 1.0}), "routing.default_weights"
    )
    for version in default_weights:
        if version not in models:
            raise ValueError(f"routing.default_weights references unknown version {version}")

    tenant_weights: dict[str, dict[str, float]] = {}
    for tenant, weights in routing_raw.get("tenant_weights", {}).items():
        parsed = _float_weights(weights, f"routing.tenant_weights.{tenant}")
        for version in parsed:
            if version not in models:
                raise ValueError(
                    f"routing.tenant_weights.{tenant} references unknown version {version}"
                )
        tenant_weights[str(tenant)] = parsed

    client_weights: dict[str, dict[str, dict[str, float]]] = {}
    for tenant, clients in routing_raw.get("client_weights", {}).items():
        client_weights[str(tenant)] = {}
        for client, weights in clients.items():
            parsed = _float_weights(
                weights, f"routing.client_weights.{tenant}.{client}"
            )
            for version in parsed:
                if version not in models:
                    raise ValueError(
                        f"routing.client_weights.{tenant}.{client} references "
                        f"unknown version {version}"
                    )
            client_weights[str(tenant)][str(client)] = parsed

    monitoring_raw = raw.get("monitoring", {})
    monitoring = MonitoringConfig(
        window_seconds=float(monitoring_raw.get("window_seconds", 10.0)),
        min_samples=int(monitoring_raw.get("min_samples", 10)),
        p99_latency_ms=float(monitoring_raw.get("p99_latency_ms", 40.0)),
        error_rate=float(monitoring_raw.get("error_rate", 0.1)),
        evaluate_interval_ms=int(monitoring_raw.get("evaluate_interval_ms", 200)),
        request_timeout_ms=int(monitoring_raw.get("request_timeout_ms", 500)),
        auto_rollback_versions=tuple(
            str(v) for v in monitoring_raw.get("auto_rollback_versions", [])
        ),
    )
    for version in monitoring.auto_rollback_versions:
        if version not in models:
            raise ValueError(f"monitoring.auto_rollback_versions unknown version {version}")
        if version == stable_version:
            raise ValueError("stable version cannot be in auto_rollback_versions")

    loading_raw = raw.get("loading", {})
    loading = LoadingConfig(
        ready_timeout_ms=int(loading_raw.get("ready_timeout_ms", 3000)),
        retain_previous_on_reload_failure=bool(
            loading_raw.get("retain_previous_on_reload_failure", True)
        ),
    )

    env_failures = {
        item.strip()
        for item in os.getenv("RANKING_FAULT_FAIL_LOAD", "").split(",")
        if item.strip()
    }
    fault_raw = raw.get("fault_injection", {})
    configured_failures = {
        str(item) for item in fault_raw.get("startup_fail_versions", [])
    }
    unknown_failures = (env_failures | configured_failures) - set(models)
    if unknown_failures:
        raise ValueError(
            "unknown startup load failure versions: "
            + ", ".join(sorted(unknown_failures))
        )

    return AppConfig(
        base_dir=base_dir,
        service=dict(raw.get("service", {})),
        models=models,
        protocols=protocols,
        stable_version=stable_version,
        routing=RoutingConfig(
            default_weights=default_weights,
            tenant_weights=tenant_weights,
            client_weights=client_weights,
            bucket_count=int(routing_raw.get("bucket_count", 10000)),
            hash_seed=str(routing_raw.get("hash_seed", "ranking-gateway-v1")),
        ),
        monitoring=monitoring,
        loading=loading,
        fault_injection_enabled=bool(
            fault_raw.get("enabled", True)
        ),
        startup_failures=frozenset(env_failures | configured_failures),
    )
