from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class ConfigError(Exception):
    def __init__(self, source: Path | str, errors: list[str]):
        self.source = str(source)
        self.errors = errors
        message = f"invalid configuration {self.source}:\n" + "\n".join(
            f"  - {error}" for error in errors
        )
        super().__init__(message)


@dataclass(frozen=True)
class ServiceConfig:
    host: str
    port: int
    admin_token: str
    business_threads: int


@dataclass(frozen=True)
class DatabaseConfig:
    path: Path
    busy_timeout_ms: int
    connection_limit: int
    operation_timeout_ms: int
    init: bool
    orders_per_tenant: int
    statuses: tuple[str, ...]


@dataclass(frozen=True)
class TenantConfig:
    tenant_id: str
    rate_per_second: float
    burst: int
    max_inflight: int
    enabled: bool = True


@dataclass(frozen=True)
class SLIConfig:
    name: str
    kind: str
    window_seconds: float
    target: float | None
    target_ms: float | None
    allowed_bad_ratio: float
    min_samples: int
    degrade_level: int
    recover_budget_ratio: float


@dataclass(frozen=True)
class DegradeLevelConfig:
    level: int
    name: str
    query_mode: str
    page_size_cap: int


@dataclass(frozen=True)
class CircuitConfig:
    failure_threshold: int
    timeout_ratio_threshold: float
    timeout_min_requests: int
    window_seconds: float
    open_seconds: float
    half_open_max_calls: int
    open_degrade_level: int
    code: str


@dataclass(frozen=True)
class CapacityConfig:
    sample_interval_ms: int
    worker_threads: int
    thread_occupancy_threshold: float
    queue_depth_threshold: int
    sqlite_busy_events_threshold: int
    enter_after_samples: int
    recover_thread_occupancy: float
    recover_queue_depth: int
    sqlite_busy_recover_events: int
    recover_after_samples: int
    page_size_cap: int
    degrade_level: int
    code: str


@dataclass(frozen=True)
class QueryConfig:
    default_page_size: int
    max_page_size: int


@dataclass(frozen=True)
class FallbackConfig:
    order: tuple[str, ...]
    cache_ttl_seconds: float
    cache_max_entries: int
    snapshot_max_orders_per_tenant: int


@dataclass(frozen=True)
class AuditConfig:
    path: Path
    run_id: str | None


@dataclass(frozen=True)
class Config:
    source_path: Path
    service: ServiceConfig
    database: DatabaseConfig
    tenants: tuple[TenantConfig, ...]
    slis: dict[str, SLIConfig]
    degrade_levels: dict[int, DegradeLevelConfig]
    circuit: CircuitConfig
    capacity: CapacityConfig
    query: QueryConfig
    fallback: FallbackConfig
    audit: AuditConfig
    sli_evaluation_interval_ms: int
    raw: dict[str, Any] = field(repr=False, default_factory=dict)

    def tenant(self, tenant_id: str) -> TenantConfig | None:
        return next((item for item in self.tenants if item.tenant_id == tenant_id), None)

    def resolve_audit_path(self) -> Path:
        return self.audit.path


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _object(value: Any, path: str, errors: list[str]) -> dict[str, Any]:
    if not isinstance(value, dict):
        errors.append(f"{path}: expected object")
        return {}
    return value


def _unknown(data: dict[str, Any], allowed: set[str], path: str, errors: list[str]) -> None:
    for key in sorted(set(data) - allowed):
        errors.append(f"{path}.{key}: unknown configuration key")


def _required(data: dict[str, Any], key: str, path: str, errors: list[str]) -> Any:
    if key not in data:
        errors.append(f"{path}.{key}: required")
        return None
    return data[key]


def _int_value(
    data: dict[str, Any],
    key: str,
    path: str,
    errors: list[str],
    *,
    minimum: int = 0,
    default: int | None = None,
) -> int:
    value = data.get(key, default)
    if value is None:
        errors.append(f"{path}.{key}: required integer")
        return minimum
    if not _is_int(value) or value < minimum:
        errors.append(f"{path}.{key}: expected integer >= {minimum}")
        return minimum
    return value


def _number_value(
    data: dict[str, Any],
    key: str,
    path: str,
    errors: list[str],
    *,
    minimum: float = 0.0,
    maximum: float | None = None,
    default: float | None = None,
) -> float:
    value = data.get(key, default)
    if value is None:
        errors.append(f"{path}.{key}: required number")
        return minimum
    if not _is_number(value) or value < minimum or (maximum is not None and value > maximum):
        suffix = f" and <= {maximum}" if maximum is not None else ""
        errors.append(f"{path}.{key}: expected number >= {minimum}{suffix}")
        return minimum
    return float(value)


def _bool_value(
    data: dict[str, Any],
    key: str,
    path: str,
    errors: list[str],
    *,
    default: bool,
) -> bool:
    value = data.get(key, default)
    if not isinstance(value, bool):
        errors.append(f"{path}.{key}: expected boolean")
        return default
    return value


def _string_value(
    data: dict[str, Any],
    key: str,
    path: str,
    errors: list[str],
    *,
    default: str | None = None,
    nonempty: bool = True,
) -> str:
    value = data.get(key, default)
    if not isinstance(value, str) or (nonempty and not value.strip()):
        errors.append(f"{path}.{key}: expected non-empty string")
        return default or ""
    return value.strip()


def _resolve_path(base: Path, value: str) -> Path:
    target = Path(value).expanduser()
    if not target.is_absolute():
        target = base / target
    return target.resolve()


def load_config(path: str | Path) -> Config:
    source = Path(path).expanduser().resolve()
    errors: list[str] = []
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise ConfigError(source, ["$: file does not exist"]) from None
    except json.JSONDecodeError as exc:
        raise ConfigError(source, [f"$: invalid JSON at line {exc.lineno} column {exc.colno}: {exc.msg}"]) from None
    except OSError as exc:
        raise ConfigError(source, [f"$: cannot read file: {exc}"]) from None

    if not isinstance(raw, dict):
        raise ConfigError(source, ["$: top-level value must be an object"])
    _unknown(
        raw,
        {
            "service",
            "database",
            "tenants",
            "slis",
            "degradation",
            "circuit",
            "capacity",
            "query",
            "fallback",
            "audit",
            "sli_evaluation_interval_ms",
        },
        "$",
        errors,
    )
    base = source.parent

    service_raw = _object(_required(raw, "service", "$", errors), "service", errors)
    _unknown(service_raw, {"host", "port", "admin_token", "business_threads"}, "$.service", errors)
    service = ServiceConfig(
        host=_string_value(service_raw, "host", "$.service", errors, default="127.0.0.1"),
        port=_int_value(service_raw, "port", "$.service", errors, minimum=1, default=8000),
        admin_token=_string_value(service_raw, "admin_token", "$.service", errors),
        business_threads=_int_value(
            service_raw, "business_threads", "$.service", errors, minimum=2, default=16
        ),
    )
    if service.port > 65535:
        errors.append("$.service.port: expected integer <= 65535")

    database_raw = _object(_required(raw, "database", "$", errors), "database", errors)
    _unknown(
        database_raw,
        {
            "path",
            "busy_timeout_ms",
            "connection_limit",
            "operation_timeout_ms",
            "init",
            "orders_per_tenant",
            "statuses",
        },
        "$.database",
        errors,
    )
    db_path = _string_value(database_raw, "path", "$.database", errors)
    statuses_raw = database_raw.get("statuses", ["created", "paid", "shipped", "completed", "cancelled"])
    statuses: tuple[str, ...]
    if (
        not isinstance(statuses_raw, list)
        or not statuses_raw
        or any(not isinstance(item, str) or not item.strip() for item in statuses_raw)
    ):
        errors.append("$.database.statuses: expected non-empty array of strings")
        statuses = ("created",)
    else:
        statuses = tuple(item.strip() for item in statuses_raw)
        if len(set(statuses)) != len(statuses):
            errors.append("$.database.statuses: values must be unique")
    database = DatabaseConfig(
        path=_resolve_path(base, db_path) if db_path else base / "orders.db",
        busy_timeout_ms=_int_value(
            database_raw, "busy_timeout_ms", "$.database", errors, minimum=1, default=500
        ),
        connection_limit=_int_value(
            database_raw, "connection_limit", "$.database", errors, minimum=1, default=8
        ),
        operation_timeout_ms=_int_value(
            database_raw, "operation_timeout_ms", "$.database", errors, minimum=1, default=500
        ),
        init=_bool_value(database_raw, "init", "$.database", errors, default=True),
        orders_per_tenant=_int_value(
            database_raw, "orders_per_tenant", "$.database", errors, minimum=1, default=300
        ),
        statuses=statuses,
    )

    tenants_raw = _required(raw, "tenants", "$", errors)
    if not isinstance(tenants_raw, list) or not tenants_raw:
        errors.append("$.tenants: expected non-empty array")
        tenants_raw = []
    tenants: list[TenantConfig] = []
    tenant_ids: set[str] = set()
    for index, item in enumerate(tenants_raw):
        path = f"$.tenants[{index}]"
        tenant_raw = _object(item, path, errors)
        _unknown(
            tenant_raw,
            {"id", "rate_per_second", "burst", "max_inflight", "enabled"},
            path,
            errors,
        )
        tenant_id = _string_value(tenant_raw, "id", path, errors)
        if tenant_id in tenant_ids:
            errors.append(f"{path}.id: duplicate tenant id {tenant_id!r}")
        tenant_ids.add(tenant_id)
        burst = _int_value(tenant_raw, "burst", path, errors, minimum=1, default=1)
        tenants.append(
            TenantConfig(
                tenant_id=tenant_id,
                rate_per_second=_number_value(
                    tenant_raw, "rate_per_second", path, errors, minimum=0.001
                ),
                burst=burst,
                max_inflight=_int_value(
                    tenant_raw, "max_inflight", path, errors, minimum=1, default=1
                ),
                enabled=_bool_value(tenant_raw, "enabled", path, errors, default=True),
            )
        )

    degradation_raw = _object(
        _required(raw, "degradation", "$", errors), "degradation", errors
    )
    _unknown(degradation_raw, {"levels"}, "$.degradation", errors)
    levels_raw = _object(
        _required(degradation_raw, "levels", "$.degradation", errors),
        "degradation.levels",
        errors,
    )
    degrade_levels: dict[int, DegradeLevelConfig] = {}
    for level_key, value in levels_raw.items():
        path = f"$.degradation.levels.{level_key}"
        if not level_key.isdigit():
            errors.append(f"{path}: level key must be an integer")
            continue
        level_num = int(level_key)
        level_raw = _object(value, path, errors)
        _unknown(level_raw, {"name", "query_mode", "page_size_cap"}, path, errors)
        mode = _string_value(level_raw, "query_mode", path, errors)
        if mode not in {"full", "cache_first", "slim"}:
            errors.append(f"{path}.query_mode: expected one of full, cache_first, slim")
        degrade_levels[level_num] = DegradeLevelConfig(
            level=level_num,
            name=_string_value(level_raw, "name", path, errors),
            query_mode=mode,
            page_size_cap=_int_value(
                level_raw, "page_size_cap", path, errors, minimum=1, default=20
            ),
        )
    if 0 not in degrade_levels:
        errors.append("$.degradation.levels.0: required baseline level")
    if not levels_raw:
        errors.append("$.degradation.levels: at least one level is required")

    slis_raw = _object(_required(raw, "slis", "$", errors), "slis", errors)
    slis: dict[str, SLIConfig] = {}
    for sli_name, value in slis_raw.items():
        path = f"$.slis.{sli_name}"
        sli_raw = _object(value, path, errors)
        _unknown(
            sli_raw,
            {
                "kind",
                "window_seconds",
                "target",
                "target_ms",
                "allowed_bad_ratio",
                "min_samples",
                "degrade_level",
                "recover_budget_ratio",
            },
            path,
            errors,
        )
        kind = _string_value(sli_raw, "kind", path, errors)
        if kind not in {"availability", "p99_latency"}:
            errors.append(f"{path}.kind: expected availability or p99_latency")
        target = None
        target_ms = None
        allowed_bad_ratio = 0.01
        if kind == "availability":
            target = _number_value(
                sli_raw,
                "target",
                path,
                errors,
                minimum=0.000001,
                maximum=1.0,
            )
            if target >= 1.0:
                errors.append(f"{path}.target: must be < 1.0")
        elif kind == "p99_latency":
            target_ms = _number_value(sli_raw, "target_ms", path, errors, minimum=0.001)
            allowed_bad_ratio = _number_value(
                sli_raw,
                "allowed_bad_ratio",
                path,
                errors,
                minimum=0.000001,
                maximum=0.999999,
                default=0.01,
            )
        slis[sli_name] = SLIConfig(
            name=sli_name,
            kind=kind,
            window_seconds=_number_value(
                sli_raw, "window_seconds", path, errors, minimum=0.1
            ),
            target=target,
            target_ms=target_ms,
            allowed_bad_ratio=allowed_bad_ratio,
            min_samples=_int_value(
                sli_raw, "min_samples", path, errors, minimum=1, default=10
            ),
            degrade_level=_int_value(
                sli_raw, "degrade_level", path, errors, minimum=1, default=1
            ),
            recover_budget_ratio=_number_value(
                sli_raw,
                "recover_budget_ratio",
                path,
                errors,
                minimum=0.0,
                maximum=1.0,
                default=0.2,
            ),
        )
    if "availability" not in slis:
        errors.append("$.slis.availability: required")
    if "latency" not in slis:
        errors.append("$.slis.latency: required")

    circuit_raw = _object(_required(raw, "circuit", "$", errors), "circuit", errors)
    _unknown(
        circuit_raw,
        {
            "failure_threshold",
            "timeout_ratio_threshold",
            "timeout_min_requests",
            "window_seconds",
            "open_seconds",
            "half_open_max_calls",
            "open_degrade_level",
            "code",
        },
        "$.circuit",
        errors,
    )
    circuit = CircuitConfig(
        failure_threshold=_int_value(
            circuit_raw, "failure_threshold", "$.circuit", errors, minimum=1, default=5
        ),
        timeout_ratio_threshold=_number_value(
            circuit_raw,
            "timeout_ratio_threshold",
            "$.circuit",
            errors,
            minimum=0.000001,
            maximum=1.0,
            default=0.5,
        ),
        timeout_min_requests=_int_value(
            circuit_raw, "timeout_min_requests", "$.circuit", errors, minimum=1, default=5
        ),
        window_seconds=_number_value(
            circuit_raw, "window_seconds", "$.circuit", errors, minimum=0.1, default=20
        ),
        open_seconds=_number_value(
            circuit_raw, "open_seconds", "$.circuit", errors, minimum=0.1, default=5
        ),
        half_open_max_calls=_int_value(
            circuit_raw, "half_open_max_calls", "$.circuit", errors, minimum=1, default=1
        ),
        open_degrade_level=_int_value(
            circuit_raw, "open_degrade_level", "$.circuit", errors, minimum=1, default=2
        ),
        code=_string_value(
            circuit_raw, "code", "$.circuit", errors, default="ORDERS_DB_CIRCUIT_OPEN"
        ),
    )

    capacity_raw = _object(_required(raw, "capacity", "$", errors), "capacity", errors)
    _unknown(
        capacity_raw,
        {
            "sample_interval_ms",
            "worker_threads",
            "thread_occupancy_threshold",
            "queue_depth_threshold",
            "sqlite_busy_events_threshold",
            "enter_after_samples",
            "recover_thread_occupancy",
            "recover_queue_depth",
            "sqlite_busy_recover_events",
            "recover_after_samples",
            "page_size_cap",
            "degrade_level",
            "code",
        },
        "$.capacity",
        errors,
    )
    capacity = CapacityConfig(
        sample_interval_ms=_int_value(
            capacity_raw, "sample_interval_ms", "$.capacity", errors, minimum=50, default=500
        ),
        worker_threads=_int_value(
            capacity_raw, "worker_threads", "$.capacity", errors, minimum=1, default=16
        ),
        thread_occupancy_threshold=_number_value(
            capacity_raw,
            "thread_occupancy_threshold",
            "$.capacity",
            errors,
            minimum=0.0,
            maximum=1.0,
            default=0.8,
        ),
        queue_depth_threshold=_int_value(
            capacity_raw, "queue_depth_threshold", "$.capacity", errors, minimum=1, default=10
        ),
        sqlite_busy_events_threshold=_int_value(
            capacity_raw,
            "sqlite_busy_events_threshold",
            "$.capacity",
            errors,
            minimum=1,
            default=3,
        ),
        enter_after_samples=_int_value(
            capacity_raw, "enter_after_samples", "$.capacity", errors, minimum=1, default=2
        ),
        recover_thread_occupancy=_number_value(
            capacity_raw,
            "recover_thread_occupancy",
            "$.capacity",
            errors,
            minimum=0.0,
            maximum=1.0,
            default=0.35,
        ),
        recover_queue_depth=_int_value(
            capacity_raw, "recover_queue_depth", "$.capacity", errors, minimum=0, default=3
        ),
        sqlite_busy_recover_events=_int_value(
            capacity_raw,
            "sqlite_busy_recover_events",
            "$.capacity",
            errors,
            minimum=0,
            default=0,
        ),
        recover_after_samples=_int_value(
            capacity_raw, "recover_after_samples", "$.capacity", errors, minimum=1, default=3
        ),
        page_size_cap=_int_value(
            capacity_raw, "page_size_cap", "$.capacity", errors, minimum=1, default=20
        ),
        degrade_level=_int_value(
            capacity_raw, "degrade_level", "$.capacity", errors, minimum=1, default=3
        ),
        code=_string_value(
            capacity_raw, "code", "$.capacity", errors, default="CAPACITY_PROTECTED"
        ),
    )

    query_raw = _object(_required(raw, "query", "$", errors), "query", errors)
    _unknown(query_raw, {"default_page_size", "max_page_size"}, "$.query", errors)
    query = QueryConfig(
        default_page_size=_int_value(
            query_raw, "default_page_size", "$.query", errors, minimum=1, default=20
        ),
        max_page_size=_int_value(
            query_raw, "max_page_size", "$.query", errors, minimum=1, default=200
        ),
    )

    fallback_raw = _object(_required(raw, "fallback", "$", errors), "fallback", errors)
    _unknown(
        fallback_raw,
        {"order", "cache_ttl_seconds", "cache_max_entries", "snapshot_max_orders_per_tenant"},
        "$.fallback",
        errors,
    )
    order_raw = fallback_raw.get("order", ["cache", "slim_snapshot"])
    fallback_order: tuple[str, ...]
    if not isinstance(order_raw, list) or not order_raw:
        errors.append("$.fallback.order: expected non-empty array")
        fallback_order = ("slim_snapshot",)
    else:
        fallback_order = tuple(str(item) for item in order_raw)
        if any(item not in {"cache", "slim_snapshot"} for item in fallback_order):
            errors.append("$.fallback.order: values must be cache or slim_snapshot")
        if len(set(fallback_order)) != len(fallback_order):
            errors.append("$.fallback.order: values must be unique")
    fallback = FallbackConfig(
        order=fallback_order,
        cache_ttl_seconds=_number_value(
            fallback_raw,
            "cache_ttl_seconds",
            "$.fallback",
            errors,
            minimum=0.1,
            default=15,
        ),
        cache_max_entries=_int_value(
            fallback_raw,
            "cache_max_entries",
            "$.fallback",
            errors,
            minimum=1,
            default=256,
        ),
        snapshot_max_orders_per_tenant=_int_value(
            fallback_raw,
            "snapshot_max_orders_per_tenant",
            "$.fallback",
            errors,
            minimum=1,
            default=2000,
        ),
    )

    audit_raw = _object(raw.get("audit", {}), "audit", errors)
    _unknown(audit_raw, {"path", "run_id"}, "$.audit", errors)
    audit_path_value = audit_raw.get("path")
    if audit_path_value is not None and (
        not isinstance(audit_path_value, str) or not audit_path_value.strip()
    ):
        errors.append("$.audit.path: expected non-empty string or null")
    audit_path = (
        _resolve_path(base, audit_path_value)
        if isinstance(audit_path_value, str) and audit_path_value.strip()
        else database.path
    )
    run_id_value = audit_raw.get("run_id")
    if run_id_value is not None and (
        not isinstance(run_id_value, str) or not run_id_value.strip()
    ):
        errors.append("$.audit.run_id: expected non-empty string or null")
    audit = AuditConfig(
        path=audit_path,
        run_id=run_id_value.strip() if isinstance(run_id_value, str) else None,
    )

    if query.default_page_size > query.max_page_size:
        errors.append("$.query.default_page_size: must be <= max_page_size")

    for sli in slis.values():
        if sli.degrade_level not in degrade_levels:
            errors.append(
                f"$.slis.{sli.name}.degrade_level: no matching $.degradation.levels.{sli.degrade_level}"
            )
    if circuit.open_degrade_level not in degrade_levels:
        errors.append(
            f"$.circuit.open_degrade_level: no matching $.degradation.levels.{circuit.open_degrade_level}"
        )
    if capacity.degrade_level not in degrade_levels:
        errors.append(
            f"$.capacity.degrade_level: no matching $.degradation.levels.{capacity.degrade_level}"
        )
    for level in degrade_levels.values():
        if level.page_size_cap > query.max_page_size:
            errors.append(
                f"$.degradation.levels.{level.level}.page_size_cap: must be <= $.query.max_page_size"
            )
    if capacity.page_size_cap > query.max_page_size:
        errors.append("$.capacity.page_size_cap: must be <= $.query.max_page_size")
    if capacity.worker_threads > service.business_threads:
        errors.append("$.capacity.worker_threads: must be <= $.service.business_threads")
    if capacity.recover_thread_occupancy >= capacity.thread_occupancy_threshold:
        errors.append(
            "$.capacity.recover_thread_occupancy: must be < thread_occupancy_threshold"
        )
    if capacity.recover_queue_depth >= capacity.queue_depth_threshold:
        errors.append("$.capacity.recover_queue_depth: must be < queue_depth_threshold")
    if capacity.sqlite_busy_recover_events >= capacity.sqlite_busy_events_threshold:
        errors.append(
            "$.capacity.sqlite_busy_recover_events: must be < sqlite_busy_events_threshold"
        )

    sli_interval = _int_value(
        raw,
        "sli_evaluation_interval_ms",
        "$",
        errors,
        minimum=50,
        default=500,
    )

    if errors:
        raise ConfigError(source, errors)

    return Config(
        source_path=source,
        service=service,
        database=database,
        tenants=tuple(tenants),
        slis=slis,
        degrade_levels=degrade_levels,
        circuit=circuit,
        capacity=capacity,
        query=query,
        fallback=fallback,
        audit=audit,
        sli_evaluation_interval_ms=sli_interval,
        raw=raw,
    )
