from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import (
    CFG_BATCH_ORDER,
    CFG_DUPLICATE_BATCH,
    CFG_DUPLICATE_INSTANCE,
    CFG_EMPTY_BATCH,
    CFG_FILE_UNREADABLE,
    CFG_JSON_INVALID,
    CFG_PORT_INVALID,
    CFG_SAMPLE_INVALID,
    CFG_SCHEMA_INVALID,
    CFG_TARGET_RANGE,
    CFG_UNKNOWN_INSTANCE,
    CFG_WINDOW_INVALID,
    ConfigError,
)


@dataclass(frozen=True)
class InstanceConfig:
    instance_id: str
    group: str
    port: int
    initial_version: str


@dataclass(frozen=True)
class BatchConfig:
    batch_id: str
    order: int
    instances: tuple[str, ...]
    rollback_version: str


@dataclass(frozen=True)
class SLODefinition:
    availability_target_percent: float
    latency_percentile: float
    latency_target_ms: float
    latency_allowed_bad_percent: float
    minimum_samples: int
    max_error_budget_consumed_percent: float


@dataclass(frozen=True)
class NetworkConfig:
    host: str
    port_range: tuple[int, int]
    backend_port_range: tuple[int, int]
    orchestrator_port: int
    precheck_timeout_seconds: float
    drain_timeout_seconds: float
    request_timeout_seconds: float


@dataclass(frozen=True)
class StorageConfig:
    database_path: Path
    report_path: Path


@dataclass(frozen=True)
class SamplingConfig:
    interval_seconds: float
    quota_per_instance: int


@dataclass(frozen=True)
class PlanConfig:
    release_id: str
    target_version: str
    rollback_version: str
    observations_seconds: float
    batches: tuple[BatchConfig, ...]
    instances: tuple[InstanceConfig, ...]
    slo: SLODefinition
    network: NetworkConfig
    storage: StorageConfig
    sampling: SamplingConfig
    plan_path: Path
    slo_path: Path
    logical_sha256: str
    raw_plan: dict[str, Any] = field(repr=False)
    raw_slo: dict[str, Any] = field(repr=False)

    def instance_map(self) -> dict[str, InstanceConfig]:
        return {item.instance_id: item for item in self.instances}


def _read_json(path: Path) -> dict[str, Any]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError(
            CFG_FILE_UNREADABLE,
            f"cannot read configuration file: {path}",
            {"path": str(path), "reason": str(exc)},
        ) from exc
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ConfigError(
            CFG_JSON_INVALID,
            f"invalid JSON in configuration file: {path}",
            {"path": str(path), "line": exc.lineno, "column": exc.colno},
        ) from exc
    if not isinstance(value, dict):
        raise ConfigError(
            CFG_JSON_INVALID,
            f"configuration root must be an object: {path}",
            {"path": str(path)},
        )
    return value


def _require_object(parent: dict[str, Any], key: str, where: str) -> dict[str, Any]:
    value = parent.get(key)
    if not isinstance(value, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            f"{where}.{key} must be an object",
            {"field": f"{where}.{key}"},
        )
    return value


def _require_string(parent: dict[str, Any], key: str, where: str) -> str:
    value = parent.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            f"{where}.{key} must be a non-empty string",
            {"field": f"{where}.{key}"},
        )
    return value.strip()


def _require_number(parent: dict[str, Any], key: str, where: str) -> float:
    value = parent.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            f"{where}.{key} must be a number",
            {"field": f"{where}.{key}"},
        )
    if not math.isfinite(float(value)):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            f"{where}.{key} must be finite",
            {"field": f"{where}.{key}"},
        )
    return float(value)


def _require_int(parent: dict[str, Any], key: str, where: str) -> int:
    value = parent.get(key)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            f"{where}.{key} must be an integer",
            {"field": f"{where}.{key}"},
        )
    return value


def _check_percent(value: float, field_name: str) -> None:
    if value < 0.0 or value > 100.0:
        raise ConfigError(
            CFG_TARGET_RANGE,
            f"{field_name} must be between 0 and 100 inclusive",
            {"field": field_name, "value": value},
        )


def _check_positive(value: float, field_name: str, code: str) -> None:
    if value <= 0.0:
        raise ConfigError(
            code,
            f"{field_name} must be greater than zero",
            {"field": field_name, "value": value},
        )


def _validate_plan_structure(plan: dict[str, Any]) -> None:
    schema = plan.get("schema_version")
    if schema != 1:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "change plan schema_version must be 1",
            {"field": "schema_version", "value": schema},
        )
    _require_string(plan, "release_id", "plan")
    _require_string(plan, "target_version", "plan")
    _require_string(plan, "rollback_version", "plan")
    _require_number(plan, "observation_window_seconds", "plan")
    instances = plan.get("instances")
    if not isinstance(instances, list) or not instances:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.instances must be a non-empty array",
            {"field": "plan.instances"},
        )
    batches = plan.get("batches")
    if not isinstance(batches, list) or not batches:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.batches must be a non-empty array",
            {"field": "plan.batches"},
        )


def _validate_slo_structure(slo: dict[str, Any]) -> None:
    if slo.get("schema_version") != 1:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "SLO schema_version must be 1",
            {"field": "schema_version", "value": slo.get("schema_version")},
        )
    _require_object(slo, "availability", "slo")
    _require_object(slo, "latency", "slo")
    _require_int(slo, "minimum_samples", "slo")
    _require_object(slo, "error_budget", "slo")


def _sha256_json(*documents: dict[str, Any]) -> str:
    encoded = json.dumps(
        documents,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def load_configuration(plan_path: str | Path, slo_path: str | Path) -> PlanConfig:
    plan_file = Path(plan_path).resolve()
    slo_file = Path(slo_path).resolve()
    plan = _read_json(plan_file)
    slo = _read_json(slo_file)
    _validate_plan_structure(plan)
    _validate_slo_structure(slo)

    release_id = _require_string(plan, "release_id", "plan")
    target_version = _require_string(plan, "target_version", "plan")
    rollback_version = _require_string(plan, "rollback_version", "plan")
    if target_version == rollback_version:
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "target_version and rollback_version must differ",
            {"target_version": target_version},
        )

    observation = _require_number(plan, "observation_window_seconds", "plan")
    _check_positive(observation, "plan.observation_window_seconds", CFG_WINDOW_INVALID)

    sampling_raw = plan.get("sampling", {})
    if not isinstance(sampling_raw, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.sampling must be an object",
            {"field": "plan.sampling"},
        )
    interval = _require_number(
        {"interval_seconds": sampling_raw.get("interval_seconds", 1.0)},
        "interval_seconds",
        "plan.sampling",
    )
    _check_positive(interval, "plan.sampling.interval_seconds", CFG_SAMPLE_INVALID)
    if interval > observation:
        raise ConfigError(
            CFG_WINDOW_INVALID,
            "sampling interval cannot be longer than the observation window",
            {"interval_seconds": interval, "window_seconds": observation},
        )
    quota = _require_int(
        {"quota_per_instance": sampling_raw.get("quota_per_instance", 1)},
        "quota_per_instance",
        "plan.sampling",
    )
    _check_positive(
        float(quota),
        "plan.sampling.quota_per_instance",
        CFG_SAMPLE_INVALID,
    )

    network_raw = _require_object(plan, "network", "plan")
    host = _require_string(network_raw, "host", "plan.network")
    port_range_raw = network_raw.get("port_range")
    if (
        not isinstance(port_range_raw, list)
        or len(port_range_raw) != 2
        or any(isinstance(item, bool) or not isinstance(item, int) for item in port_range_raw)
    ):
        raise ConfigError(
            CFG_PORT_INVALID,
            "plan.network.port_range must contain two integers",
            {"field": "plan.network.port_range"},
        )
    port_start, port_end = int(port_range_raw[0]), int(port_range_raw[1])
    if not (1 <= port_start <= port_end <= 65535):
        raise ConfigError(
            CFG_PORT_INVALID,
            "plan.network.port_range must be within 1..65535 and ordered",
            {"port_range": [port_start, port_end]},
        )
    orchestrator_port = _require_int(network_raw, "orchestrator_port", "plan.network")
    if not (1 <= orchestrator_port <= 65535):
        raise ConfigError(
            CFG_PORT_INVALID,
            "plan.network.orchestrator_port is not a valid TCP port",
            {"port": orchestrator_port},
        )
    backend_range_raw = network_raw.get("backend_port_range", [41000, 41999])
    if (
        not isinstance(backend_range_raw, list)
        or len(backend_range_raw) != 2
        or any(
            isinstance(item, bool) or not isinstance(item, int)
            for item in backend_range_raw
        )
    ):
        raise ConfigError(
            CFG_PORT_INVALID,
            "plan.network.backend_port_range must contain two integers",
            {"field": "plan.network.backend_port_range"},
        )
    backend_start, backend_end = (
        int(backend_range_raw[0]),
        int(backend_range_raw[1]),
    )
    if not (1 <= backend_start <= backend_end <= 65535):
        raise ConfigError(
            CFG_PORT_INVALID,
            "plan.network.backend_port_range must be within 1..65535 and ordered",
            {"backend_port_range": [backend_start, backend_end]},
        )
    precheck_timeout = float(network_raw.get("precheck_timeout_seconds", 2.0))
    drain_timeout = float(network_raw.get("drain_timeout_seconds", 5.0))
    request_timeout = float(network_raw.get("request_timeout_seconds", 10.0))
    _check_positive(precheck_timeout, "plan.network.precheck_timeout_seconds", CFG_SCHEMA_INVALID)
    _check_positive(drain_timeout, "plan.network.drain_timeout_seconds", CFG_SCHEMA_INVALID)
    _check_positive(request_timeout, "plan.network.request_timeout_seconds", CFG_SCHEMA_INVALID)

    instances: list[InstanceConfig] = []
    instance_ids: set[str] = set()
    ports: set[int] = set()
    for index, raw in enumerate(plan["instances"]):
        where = f"plan.instances[{index}]"
        if not isinstance(raw, dict):
            raise ConfigError(
                CFG_SCHEMA_INVALID,
                f"{where} must be an object",
                {"field": where},
            )
        instance_id = _require_string(raw, "id", where)
        group = _require_string(raw, "group", where)
        port = _require_int(raw, "port", where)
        initial_version = _require_string(raw, "initial_version", where)
        if not (1 <= port <= 65535):
            raise ConfigError(
                CFG_PORT_INVALID,
                f"{where}.port is not a valid TCP port",
                {"instance_id": instance_id, "port": port},
            )
        if instance_id in instance_ids:
            raise ConfigError(
                CFG_DUPLICATE_INSTANCE,
                f"duplicate instance declaration: {instance_id}",
                {"instance_id": instance_id},
            )
        if port in ports:
            raise ConfigError(
                CFG_PORT_INVALID,
                f"duplicate instance port: {port}",
                {"port": port, "instance_id": instance_id},
            )
        instance_ids.add(instance_id)
        ports.add(port)
        instances.append(
            InstanceConfig(
                instance_id=instance_id,
                group=group,
                port=port,
                initial_version=initial_version,
            )
        )

    batches: list[BatchConfig] = []
    batch_ids: set[str] = set()
    batch_orders: set[int] = set()
    assigned_instances: dict[str, str] = {}
    for index, raw in enumerate(plan["batches"]):
        where = f"plan.batches[{index}]"
        if not isinstance(raw, dict):
            raise ConfigError(
                CFG_SCHEMA_INVALID,
                f"{where} must be an object",
                {"field": where},
            )
        batch_id = _require_string(raw, "id", where)
        order = _require_int(raw, "order", where)
        members = raw.get("instances")
        if not isinstance(members, list) or not members:
            raise ConfigError(
                CFG_EMPTY_BATCH,
                f"batch {batch_id} has no instances",
                {"batch_id": batch_id},
            )
        batch_rollback = str(raw.get("rollback_version", rollback_version)).strip()
        if not batch_rollback:
            raise ConfigError(
                CFG_SCHEMA_INVALID,
                f"batch {batch_id} rollback_version is empty",
                {"batch_id": batch_id},
            )
        if batch_id in batch_ids:
            raise ConfigError(
                CFG_DUPLICATE_BATCH,
                f"duplicate batch id: {batch_id}",
                {"batch_id": batch_id},
            )
        if order in batch_orders or order <= 0:
            raise ConfigError(
                CFG_BATCH_ORDER,
                f"batch order must be a unique positive integer: {batch_id}",
                {"batch_id": batch_id, "order": order},
            )
        normalized: list[str] = []
        for member in members:
            if not isinstance(member, str) or not member.strip():
                raise ConfigError(
                    CFG_SCHEMA_INVALID,
                    f"batch {batch_id} contains a non-string instance id",
                    {"batch_id": batch_id},
                )
            instance_id = member.strip()
            if instance_id not in instance_ids:
                raise ConfigError(
                    CFG_UNKNOWN_INSTANCE,
                    f"batch {batch_id} references undeclared instance: {instance_id}",
                    {"batch_id": batch_id, "instance_id": instance_id},
                )
            if instance_id in assigned_instances:
                raise ConfigError(
                    CFG_DUPLICATE_INSTANCE,
                    f"instance {instance_id} appears in more than one batch",
                    {
                        "instance_id": instance_id,
                        "batches": [assigned_instances[instance_id], batch_id],
                    },
                )
            assigned_instances[instance_id] = batch_id
            normalized.append(instance_id)
        batch_ids.add(batch_id)
        batch_orders.add(order)
        batches.append(
            BatchConfig(
                batch_id=batch_id,
                order=order,
                instances=tuple(normalized),
                rollback_version=batch_rollback,
            )
        )

    batches.sort(key=lambda item: item.order)
    orders = [item.order for item in batches]
    if orders != list(range(1, len(batches) + 1)):
        raise ConfigError(
            CFG_BATCH_ORDER,
            "batch orders must be contiguous and start at 1",
            {"orders": orders},
        )

    availability_raw = slo["availability"]
    latency_raw = slo["latency"]
    budget_raw = slo["error_budget"]
    availability_target = _require_number(
        availability_raw,
        "target_percent",
        "slo.availability",
    )
    latency_percentile = _require_number(latency_raw, "percentile", "slo.latency")
    latency_target_ms = _require_number(latency_raw, "target_ms", "slo.latency")
    allowed_bad = _require_number(
        {"allowed_bad_percent": latency_raw.get("allowed_bad_percent", 100.0 - latency_percentile)},
        "allowed_bad_percent",
        "slo.latency",
    )
    minimum_samples = _require_int(slo, "minimum_samples", "slo")
    max_budget = _require_number(
        budget_raw,
        "max_consumed_percent",
        "slo.error_budget",
    )
    _check_percent(availability_target, "slo.availability.target_percent")
    _check_percent(latency_percentile, "slo.latency.percentile")
    _check_percent(allowed_bad, "slo.latency.allowed_bad_percent")
    _check_percent(max_budget, "slo.error_budget.max_consumed_percent")
    _check_positive(latency_target_ms, "slo.latency.target_ms", CFG_TARGET_RANGE)
    _check_positive(float(minimum_samples), "slo.minimum_samples", CFG_SAMPLE_INVALID)

    storage_raw = plan.get("storage", {})
    if not isinstance(storage_raw, dict):
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.storage must be an object",
            {"field": "plan.storage"},
        )
    db_value = storage_raw.get("database_path", "state/rollout.sqlite3")
    report_value = storage_raw.get("report_path", "state/report.json")
    if not isinstance(db_value, str) or not db_value.strip():
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.storage.database_path must be a non-empty string",
            {"field": "plan.storage.database_path"},
        )
    if not isinstance(report_value, str) or not report_value.strip():
        raise ConfigError(
            CFG_SCHEMA_INVALID,
            "plan.storage.report_path must be a non-empty string",
            {"field": "plan.storage.report_path"},
        )
    base_dir = plan_file.parent
    database_path = Path(db_value)
    report_path = Path(report_value)
    if not database_path.is_absolute():
        database_path = (base_dir / database_path).resolve()
    if not report_path.is_absolute():
        report_path = (base_dir / report_path).resolve()

    logical_sha = _sha256_json(plan, slo)
    return PlanConfig(
        release_id=release_id,
        target_version=target_version,
        rollback_version=rollback_version,
        observations_seconds=observation,
        batches=tuple(batches),
        instances=tuple(instances),
        slo=SLODefinition(
            availability_target_percent=availability_target,
            latency_percentile=latency_percentile,
            latency_target_ms=latency_target_ms,
            latency_allowed_bad_percent=allowed_bad,
            minimum_samples=minimum_samples,
            max_error_budget_consumed_percent=max_budget,
        ),
        network=NetworkConfig(
            host=host,
            port_range=(port_start, port_end),
            backend_port_range=(backend_start, backend_end),
            orchestrator_port=orchestrator_port,
            precheck_timeout_seconds=precheck_timeout,
            drain_timeout_seconds=drain_timeout,
            request_timeout_seconds=request_timeout,
        ),
        storage=StorageConfig(
            database_path=database_path,
            report_path=report_path,
        ),
        sampling=SamplingConfig(
            interval_seconds=interval,
            quota_per_instance=quota,
        ),
        plan_path=plan_file,
        slo_path=slo_file,
        logical_sha256=logical_sha,
        raw_plan=plan,
        raw_slo=slo,
    )
