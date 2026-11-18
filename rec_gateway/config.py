from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


PROJECT_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CONFIG_PATH = PROJECT_ROOT / "config" / "gateway.json"


@dataclass(frozen=True)
class TenantConfig:
    tenant_id: str
    canary_weight: float


@dataclass
class GatewayConfig:
    host: str = "127.0.0.1"
    port: int = 8080
    model_dir: Path = PROJECT_ROOT / "generated" / "models"
    database: Path = PROJECT_ROOT / "data" / "gateway.sqlite3"
    stable_version: str = "v3"
    canary_version: str = "v4"
    max_batch_size: int = 64
    max_concurrency: int = 2
    request_timeout_ms: float = 600.0
    fault_injection_enabled: bool = True
    tenants: dict[str, TenantConfig] = field(default_factory=dict)
    config_path: Path | None = None

    @classmethod
    def from_dict(
        cls, raw: dict[str, Any], base_dir: Path | None = None
    ) -> "GatewayConfig":
        base = (base_dir or PROJECT_ROOT).resolve()
        tenants_raw = raw.get("tenants", [])
        tenants: dict[str, TenantConfig] = {}
        for item in tenants_raw:
            tenant_id = str(item["tenant_id"])
            weight = float(item.get("canary_weight", 0.0))
            if not tenant_id or tenant_id in tenants:
                raise ValueError("tenant_id must be non-empty and unique")
            if weight < 0.0 or weight > 1.0:
                raise ValueError("canary_weight must be within [0, 1]")
            tenants[tenant_id] = TenantConfig(tenant_id, weight)
        if not tenants:
            raise ValueError("at least one tenant is required")

        config = cls(
            host=str(raw.get("host", "127.0.0.1")),
            port=int(raw.get("port", 8080)),
            model_dir=(base / raw.get("model_dir", "generated/models")).resolve(),
            database=(base / raw.get("database", "data/gateway.sqlite3")).resolve(),
            stable_version=str(raw.get("stable_version", "v3")),
            canary_version=str(raw.get("canary_version", "v4")),
            max_batch_size=int(raw.get("max_batch_size", 64)),
            max_concurrency=int(raw.get("max_concurrency", 2)),
            request_timeout_ms=float(raw.get("request_timeout_ms", 600.0)),
            fault_injection_enabled=bool(raw.get("fault_injection_enabled", True)),
            tenants=tenants,
        )
        if config.max_batch_size < 1 or config.max_concurrency < 1:
            raise ValueError("max_batch_size and max_concurrency must be positive")
        if config.stable_version == config.canary_version:
            raise ValueError("stable_version and canary_version must differ")
        if config.request_timeout_ms <= 0:
            raise ValueError("request_timeout_ms must be positive")
        return config

    @classmethod
    def load(cls, path: str | Path | None = None) -> "GatewayConfig":
        config_path = Path(path or os.environ.get("REC_GATEWAY_CONFIG", DEFAULT_CONFIG_PATH))
        config_path = config_path.resolve()
        with config_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
        config = cls.from_dict(raw, base_dir=config_path.parent)
        config.config_path = config_path
        if os.environ.get("REC_GATEWAY_HOST"):
            config.host = os.environ["REC_GATEWAY_HOST"]
        if os.environ.get("REC_GATEWAY_PORT"):
            config.port = int(os.environ["REC_GATEWAY_PORT"])
        if os.environ.get("REC_GATEWAY_MODEL_DIR"):
            config.model_dir = Path(os.environ["REC_GATEWAY_MODEL_DIR"]).resolve()
        if os.environ.get("REC_GATEWAY_DATABASE"):
            config.database = Path(os.environ["REC_GATEWAY_DATABASE"]).resolve()
        return config

    def public_summary(self) -> dict[str, Any]:
        return {
            "host": self.host,
            "port": self.port,
            "stable_version": self.stable_version,
            "canary_version": self.canary_version,
            "max_batch_size": self.max_batch_size,
            "max_concurrency": self.max_concurrency,
            "request_timeout_ms": self.request_timeout_ms,
            "fault_injection_enabled": self.fault_injection_enabled,
        }
