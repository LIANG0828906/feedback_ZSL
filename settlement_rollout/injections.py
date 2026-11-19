from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .backend import normalize_faults
from .config import PlanConfig
from .errors import (
    CFG_FILE_UNREADABLE,
    CFG_JSON_INVALID,
    CFG_SCHEMA_INVALID,
    CFG_UNKNOWN_INSTANCE,
    ConfigError,
)


@dataclass(frozen=True)
class InjectionConfig:
    expected_outcome: str
    rate_per_second: float
    workers: int
    profiles: dict[str, dict[str, Any]]
    sha256: str
    raw: dict[str, Any]


VALID_OUTCOMES = {
    "RELEASED",
    "ROLLED_BACK",
    "PARTIAL_ROLLBACK",
    "ROLLBACK_FAILED",
    "ABORTED",
}


def _read(path: Path) -> dict[str, Any]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError(
            CFG_FILE_UNREADABLE,
            f"cannot read injection file: {path}",
            {"path": str(path), "reason": str(exc)},
        ) from exc
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ConfigError(
            CFG_JSON_INVALID,
            f"invalid JSON in injection file: {path}",
            {"path": str(path), "line": exc.lineno, "column": exc.colno},
        ) from exc
    if not isinstance(value, dict):
        raise ConfigError(
            CFG_JSON_INVALID,
            "injection file root must be an object",
            {"path": str(path)},
        )
    return value


def load_injections(
    path: str | Path,
    plan: PlanConfig,
) -> InjectionConfig:
    file_path = Path(path).resolve()
    raw = _read(file_path)
    if raw.get("schema_version") != 1:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection schema_version must be 1",
            {"field": "schema_version"},
        )
    expected = str(raw.get("expected_outcome", "")).upper()
    if expected not in VALID_OUTCOMES:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection expected_outcome is invalid",
            {"expected_outcome": expected, "allowed": sorted(VALID_OUTCOMES)},
        )
    load_raw = raw.get("load", {})
    if not isinstance(load_raw, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection load must be an object",
            {"field": "load"},
        )
    rate = float(load_raw.get("rate_per_second", 12.0))
    workers = int(load_raw.get("workers", 12))
    if rate <= 0 or workers <= 0:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection load rate and workers must be positive",
            {"rate_per_second": rate, "workers": workers},
        )
    defaults = raw.get("defaults", {})
    if not isinstance(defaults, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection defaults must be an object",
            {"field": "defaults"},
        )
    raw_instances = raw.get("instances", {})
    if not isinstance(raw_instances, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection instances must be an object",
            {"field": "instances"},
        )
    declared = set(plan.instance_map())
    unknown = sorted(set(raw_instances) - declared)
    if unknown:
        raise ConfigError(
            CFG_UNKNOWN_INSTANCE,
            f"injection references undeclared instances: {', '.join(unknown)}",
            {"instances": unknown},
        )
    default_versions = defaults.get("versions", {})
    if not isinstance(default_versions, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "injection defaults.versions must be an object",
            {"field": "defaults.versions"},
        )
    profiles: dict[str, dict[str, Any]] = {}
    for instance_id in sorted(declared):
        own = raw_instances.get(instance_id, {})
        if not isinstance(own, dict):
            raise ConfigError(
                CFG_SCHEMA_INVALID,
                f"injection profile for {instance_id} must be an object",
                {"instance_id": instance_id},
            )
        own_versions = own.get("versions", {})
        if not isinstance(own_versions, dict):
            raise ConfigError(
                CFG_SCHEMA_INVALID,
                f"injection versions for {instance_id} must be an object",
                {"instance_id": instance_id},
            )
        merged_versions = {
            **default_versions,
            **own_versions,
        }
        normalized_versions: dict[str, dict[str, Any]] = {}
        for version, profile in merged_versions.items():
            if not isinstance(version, str) or not version:
                raise ConfigError(
                    CFG_SCHEMA_INVALID,
                    "injection version keys must be non-empty strings",
                    {"instance_id": instance_id},
                )
            if not isinstance(profile, dict):
                raise ConfigError(
                    CFG_SCHEMA_INVALID,
                    f"injection profile for {instance_id}/{version} must be an object",
                    {"instance_id": instance_id, "version": version},
                )
            try:
                normalized_versions[version] = normalize_faults(profile)
            except (TypeError, ValueError) as exc:
                raise ConfigError(
                    CFG_SCHEMA_INVALID,
                    f"invalid fault profile for {instance_id}/{version}: {exc}",
                    {"instance_id": instance_id, "version": version},
                ) from exc
        profiles[instance_id] = {
            "versions": normalized_versions,
            "rollback_restart_refused": bool(
                own.get(
                    "rollback_restart_refused",
                    defaults.get("rollback_restart_refused", False),
                )
            ),
        }
    encoded = json.dumps(
        raw,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return InjectionConfig(
        expected_outcome=expected,
        rate_per_second=rate,
        workers=workers,
        profiles=profiles,
        sha256=hashlib.sha256(encoded).hexdigest(),
        raw=raw,
    )
