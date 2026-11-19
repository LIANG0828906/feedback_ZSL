from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlparse


PRIORITIES = ("high", "medium", "low")
PRIORITY_RANK = {name: index for index, name in enumerate(PRIORITIES)}
TENANT_ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")


class ConfigError(Exception):
    def __init__(self, code: str, message: str, tenant_id: str | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.tenant_id = tenant_id

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"business_code": self.code, "message": self.message}
        if self.tenant_id is not None:
            result["tenant_id"] = self.tenant_id
        return result


@dataclass(frozen=True)
class TenantConfig:
    tenant_id: str
    priority: str
    max_concurrency: int
    max_retries: int
    backoff_base_ms: int
    timeout_ms: int
    degrade_after_timeouts: int
    receiver_url: str

    @property
    def priority_rank(self) -> int:
        return PRIORITY_RANK[self.priority]


def _as_int(value: Any, code: str, message: str, tenant_id: str | None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ConfigError(code, message, tenant_id)
    return value


def parse_config(document: Any) -> tuple[TenantConfig, ...]:
    if not isinstance(document, dict):
        raise ConfigError("CFG_ROOT_INVALID", "configuration root must be an object")
    raw_tenants = document.get("tenants")
    if not isinstance(raw_tenants, list) or not raw_tenants:
        raise ConfigError("CFG_TENANTS_INVALID", "tenants must be a non-empty array")

    tenants: list[TenantConfig] = []
    seen: set[str] = set()
    for index, raw in enumerate(raw_tenants):
        if not isinstance(raw, dict):
            raise ConfigError(
                "CFG_TENANT_INVALID", f"tenants[{index}] must be an object"
            )

        tenant_id = raw.get("tenant_id")
        if not isinstance(tenant_id, str) or not TENANT_ID_RE.fullmatch(tenant_id):
            raise ConfigError(
                "CFG_TENANT_ID_INVALID",
                f"tenants[{index}].tenant_id is invalid",
            )
        if tenant_id in seen:
            raise ConfigError(
                "CFG_DUPLICATE_TENANT", f"duplicate tenant {tenant_id}", tenant_id
            )
        seen.add(tenant_id)

        priority = raw.get("priority")
        if priority not in PRIORITIES:
            raise ConfigError(
                "CFG_PRIORITY_INVALID",
                f"tenant {tenant_id} priority must be one of {', '.join(PRIORITIES)}",
                tenant_id,
            )

        max_concurrency = _as_int(
            raw.get("max_concurrency"),
            "CFG_CONCURRENCY_INVALID",
            f"tenant {tenant_id} max_concurrency must be a positive integer",
            tenant_id,
        )
        if max_concurrency <= 0:
            raise ConfigError(
                "CFG_CONCURRENCY_INVALID",
                f"tenant {tenant_id} max_concurrency must be greater than zero",
                tenant_id,
            )

        max_retries = _as_int(
            raw.get("max_retries"),
            "CFG_RETRIES_INVALID",
            f"tenant {tenant_id} max_retries must be a non-negative integer",
            tenant_id,
        )
        if max_retries < 0:
            raise ConfigError(
                "CFG_RETRIES_INVALID",
                f"tenant {tenant_id} max_retries must not be negative",
                tenant_id,
            )

        backoff_base_ms = _as_int(
            raw.get("backoff_base_ms"),
            "CFG_BACKOFF_INVALID",
            f"tenant {tenant_id} backoff_base_ms must be a non-negative integer",
            tenant_id,
        )
        if backoff_base_ms < 0:
            raise ConfigError(
                "CFG_BACKOFF_INVALID",
                f"tenant {tenant_id} backoff_base_ms must not be negative",
                tenant_id,
            )

        timeout_ms = _as_int(
            raw.get("timeout_ms", 2000),
            "CFG_TIMEOUT_INVALID",
            f"tenant {tenant_id} timeout_ms must be a positive integer",
            tenant_id,
        )
        if timeout_ms <= 0:
            raise ConfigError(
                "CFG_TIMEOUT_INVALID",
                f"tenant {tenant_id} timeout_ms must be greater than zero",
                tenant_id,
            )

        degrade_after_timeouts = _as_int(
            raw.get("degrade_after_timeouts", 3),
            "CFG_DEGRADE_THRESHOLD_INVALID",
            f"tenant {tenant_id} degrade_after_timeouts must be a non-negative integer",
            tenant_id,
        )
        if degrade_after_timeouts < 0:
            raise ConfigError(
                "CFG_DEGRADE_THRESHOLD_INVALID",
                f"tenant {tenant_id} degrade_after_timeouts must not be negative",
                tenant_id,
            )

        receiver_url = raw.get("receiver_url")
        if not isinstance(receiver_url, str) or not receiver_url.strip():
            raise ConfigError(
                "CFG_RECEIVER_URL_INVALID",
                f"tenant {tenant_id} receiver_url is required",
                tenant_id,
            )
        parsed_url = urlparse(receiver_url)
        if parsed_url.scheme not in ("http", "https") or not parsed_url.netloc:
            raise ConfigError(
                "CFG_RECEIVER_URL_INVALID",
                f"tenant {tenant_id} receiver_url must be an absolute HTTP URL",
                tenant_id,
            )

        tenants.append(
            TenantConfig(
                tenant_id=tenant_id,
                priority=priority,
                max_concurrency=max_concurrency,
                max_retries=max_retries,
                backoff_base_ms=backoff_base_ms,
                timeout_ms=timeout_ms,
                degrade_after_timeouts=degrade_after_timeouts,
                receiver_url=receiver_url.rstrip("/"),
            )
        )
    return tuple(tenants)


def load_config(path: str | None = None) -> tuple[TenantConfig, ...]:
    inline = os.environ.get("TENANT_CONFIG_JSON")
    config_path = path or os.environ.get(
        "TENANT_CONFIG_PATH", "/etc/webhook-lab/tenants.json"
    )
    try:
        if inline is not None:
            text = inline
        else:
            with open(config_path, "r", encoding="utf-8") as handle:
                text = handle.read()
    except FileNotFoundError as exc:
        raise ConfigError(
            "CFG_FILE_MISSING", f"tenant configuration not found at {config_path}"
        ) from exc
    except OSError as exc:
        raise ConfigError(
            "CFG_FILE_UNREADABLE", f"tenant configuration cannot be read: {exc}"
        ) from exc
    except UnicodeDecodeError as exc:
        raise ConfigError(
            "CFG_JSON_INVALID", "tenant configuration is not valid UTF-8 JSON"
        ) from exc

    try:
        document = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ConfigError(
            "CFG_JSON_INVALID",
            f"tenant configuration is not valid JSON at line {exc.lineno}",
        ) from exc
    return parse_config(document)

