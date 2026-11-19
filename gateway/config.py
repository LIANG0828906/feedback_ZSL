"""Gateway configuration loaded from a JSON file with env-var overrides."""

from __future__ import annotations

import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path


DEFAULT_MODEL_DIR = "artifacts/models"
DEFAULT_MAPPING_FILE = "protocol_mapping.json"


@dataclass(frozen=True)
class VersionConfig:
    """Per-version queue, batching and circuit-breaker settings."""

    max_queue_depth: int = 128
    max_concurrency: int = 1
    min_batch_size: int = 1
    max_batch_size: int = 16
    min_wait_ms: float = 2.0
    max_wait_ms: float = 20.0
    enqueue_timeout_ms: float = 500.0
    ready_wait_ms: float = 2000.0
    circuit_p99_ms: float = 120.0
    circuit_error_rate: float = 0.5
    circuit_min_samples: int = 8
    window_seconds: float = 10.0


@dataclass(frozen=True)
class GatewayConfig:
    host: str = "127.0.0.1"
    port: int = 8000
    metrics_port: int = 9090
    model_dir: str = DEFAULT_MODEL_DIR
    mapping_file: str = DEFAULT_MAPPING_FILE
    stable_version: str = "v3"
    canary_version: str = "v4"
    default_canary_weight: float = 0.3
    tenant_default_weights: dict = field(default_factory=dict)
    versions: dict = field(default_factory=dict)

    def version_config(self, version: str) -> VersionConfig:
        raw = self.versions.get(version, {})
        allowed = {k: v for k, v in raw.items() if k in VersionConfig.__dataclass_fields__}
        return VersionConfig(**allowed)

    def to_json(self) -> str:
        return json.dumps(asdict(self), indent=2, sort_keys=True)


def _env_int(name: str) -> int | None:
    value = os.environ.get(name)
    return int(value) if value not in (None, "") else None


def _env_float(name: str) -> float | None:
    value = os.environ.get(name)
    return float(value) if value not in (None, "") else None


def load_config(path: str | os.PathLike | None = None) -> GatewayConfig:
    """Load config from JSON; unknown keys are ignored, env vars take precedence."""

    data: dict = {}
    if path and Path(path).exists():
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)

    allowed = {k: v for k, v in data.items() if k in GatewayConfig.__dataclass_fields__}
    cfg = GatewayConfig(**allowed)

    overrides = {
        "host": os.environ.get("GATEWAY_HOST"),
        "port": _env_int("GATEWAY_PORT"),
        "metrics_port": _env_int("METRICS_PORT"),
        "model_dir": os.environ.get("MODEL_DIR"),
        "default_canary_weight": _env_float("DEFAULT_CANARY_WEIGHT"),
    }
    overrides = {k: v for k, v in overrides.items() if v is not None}
    if overrides:
        cfg = GatewayConfig(**{**asdict(cfg), **overrides})
    return cfg
