"""Configuration loading with strict resource defaults."""

from __future__ import annotations

import copy
import json
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import ERROR_CONFIG, ExecutorError


MIB = 1024 * 1024

DEFAULT_CONFIG: dict[str, Any] = {
    "batch_rows": 4096,
    "partitions": 4,
    "max_workers": 2,
    "max_retries": 2,
    "memory_limit_mb": 768,
    "agg_memory_threshold_mb": 8,
    "temp_limit_mb": 1536,
    "temp_dir": "tmp",
    "duckdb_memory_limit_mb": 192,
    "stream_batch_rows": 512,
    "stream_mode": "incremental",
    "allowed_lateness_seconds": 30,
    "result_format": "csv",
    "fault_injection": None,
    "force_spill": False,
    "max_join_tables": 12,
}


def _deep_merge(base: dict[str, Any], extra: dict[str, Any]) -> dict[str, Any]:
    merged = copy.deepcopy(base)
    for key, value in extra.items():
        if isinstance(value, dict) and isinstance(merged.get(key), dict):
            merged[key] = _deep_merge(merged[key], value)
        else:
            merged[key] = value
    return merged


def _env_overrides() -> dict[str, Any]:
    names = {
        "EXECUTOR_BATCH_ROWS": ("batch_rows", int),
        "EXECUTOR_PARTITIONS": ("partitions", int),
        "EXECUTOR_MAX_WORKERS": ("max_workers", int),
        "EXECUTOR_MAX_RETRIES": ("max_retries", int),
        "EXECUTOR_MEMORY_LIMIT_MB": ("memory_limit_mb", int),
        "EXECUTOR_AGG_MEMORY_THRESHOLD_MB": ("agg_memory_threshold_mb", int),
        "EXECUTOR_TEMP_LIMIT_MB": ("temp_limit_mb", int),
        "EXECUTOR_DUCKDB_MEMORY_LIMIT_MB": ("duckdb_memory_limit_mb", int),
        "EXECUTOR_TEMP_DIR": ("temp_dir", str),
        "EXECUTOR_STREAM_BATCH_ROWS": ("stream_batch_rows", int),
        "EXECUTOR_STREAM_MODE": ("stream_mode", str),
        "EXECUTOR_ALLOWED_LATENESS_SECONDS": ("allowed_lateness_seconds", int),
    }
    out: dict[str, Any] = {}
    for env_name, (key, convert) in names.items():
        raw = os.environ.get(env_name)
        if raw is not None and raw != "":
            try:
                out[key] = convert(raw)
            except ValueError as exc:
                raise ExecutorError(
                    ERROR_CONFIG, f"invalid value for {env_name}: {raw!r}"
                ) from exc

    fault_partition = os.environ.get("EXECUTOR_FAULT_PARTITION")
    if fault_partition:
        try:
            batch = int(os.environ.get("EXECUTOR_FAULT_BATCH", "0"))
        except ValueError as exc:
            raise ExecutorError(
                ERROR_CONFIG, "EXECUTOR_FAULT_BATCH must be an integer"
            ) from exc
        out["fault_injection"] = {
            "partition_id": fault_partition,
            "batch": batch,
        }
    return out


@dataclass(frozen=True)
class ExecutorConfig:
    batch_rows: int = 4096
    partitions: int = 4
    max_workers: int = 2
    max_retries: int = 2
    memory_limit_mb: int = 768
    agg_memory_threshold_mb: int = 8
    temp_limit_mb: int = 1536
    temp_dir: str = "tmp"
    duckdb_memory_limit_mb: int = 192
    stream_batch_rows: int = 512
    stream_mode: str = "incremental"
    allowed_lateness_seconds: int = 30
    result_format: str = "csv"
    fault_injection: dict[str, Any] | None = None
    force_spill: bool = False
    max_join_tables: int = 12
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def memory_limit_bytes(self) -> int:
        return self.memory_limit_mb * MIB

    @property
    def agg_memory_threshold_bytes(self) -> int:
        return self.agg_memory_threshold_mb * MIB

    @property
    def temp_limit_bytes(self) -> int:
        return self.temp_limit_mb * MIB

    @property
    def duckdb_memory_limit_bytes(self) -> int:
        return self.duckdb_memory_limit_mb * MIB

    def with_overrides(self, **kwargs: Any) -> "ExecutorConfig":
        raw = copy.deepcopy(self.raw)
        raw.update(kwargs)
        return config_from_dict(raw)


def config_from_dict(values: dict[str, Any]) -> ExecutorConfig:
    merged = _deep_merge(DEFAULT_CONFIG, values or {})
    try:
        cfg = ExecutorConfig(
            batch_rows=int(merged["batch_rows"]),
            partitions=int(merged["partitions"]),
            max_workers=int(merged["max_workers"]),
            max_retries=int(merged["max_retries"]),
            memory_limit_mb=int(merged["memory_limit_mb"]),
            agg_memory_threshold_mb=int(merged["agg_memory_threshold_mb"]),
            temp_limit_mb=int(merged["temp_limit_mb"]),
            temp_dir=str(merged["temp_dir"]),
            duckdb_memory_limit_mb=int(merged["duckdb_memory_limit_mb"]),
            stream_batch_rows=int(merged["stream_batch_rows"]),
            stream_mode=str(merged["stream_mode"]).lower(),
            allowed_lateness_seconds=int(merged["allowed_lateness_seconds"]),
            result_format=str(merged["result_format"]).lower(),
            fault_injection=merged.get("fault_injection"),
            force_spill=bool(merged.get("force_spill", False)),
            max_join_tables=int(merged.get("max_join_tables", 12)),
            raw=merged,
        )
    except (TypeError, ValueError) as exc:
        raise ExecutorError(ERROR_CONFIG, f"invalid executor config: {exc}") from exc

    if cfg.batch_rows < 1:
        raise ExecutorError(ERROR_CONFIG, "batch_rows must be >= 1")
    if cfg.partitions < 1:
        raise ExecutorError(ERROR_CONFIG, "partitions must be >= 1")
    if cfg.max_workers < 1:
        raise ExecutorError(ERROR_CONFIG, "max_workers must be >= 1")
    if cfg.max_workers > 2:
        raise ExecutorError(
            ERROR_CONFIG,
            "max_workers must be <= 2 to keep one logical CPU per unit",
        )
    if cfg.max_retries < 0:
        raise ExecutorError(ERROR_CONFIG, "max_retries must be >= 0")
    if cfg.memory_limit_mb < 64:
        raise ExecutorError(ERROR_CONFIG, "memory_limit_mb must be >= 64")
    if cfg.temp_limit_mb < 1:
        raise ExecutorError(ERROR_CONFIG, "temp_limit_mb must be >= 1")
    if cfg.stream_mode not in {"bounded", "incremental"}:
        raise ExecutorError(ERROR_CONFIG, "stream_mode must be bounded or incremental")
    if cfg.result_format not in {"csv", "parquet"}:
        raise ExecutorError(ERROR_CONFIG, "result_format must be csv or parquet")
    if cfg.fault_injection is not None:
        if not isinstance(cfg.fault_injection, dict):
            raise ExecutorError(ERROR_CONFIG, "fault_injection must be an object")
        partition_id = cfg.fault_injection.get("partition_id")
        if not isinstance(partition_id, str) or not partition_id:
            raise ExecutorError(
                ERROR_CONFIG, "fault_injection.partition_id must be a non-empty string"
            )
        try:
            batch = int(cfg.fault_injection.get("batch", 0))
        except (TypeError, ValueError) as exc:
            raise ExecutorError(
                ERROR_CONFIG, "fault_injection.batch must be an integer"
            ) from exc
        if batch < 0:
            raise ExecutorError(
                ERROR_CONFIG, "fault_injection.batch must be >= 0"
            )
    if (
        cfg.max_workers * (cfg.duckdb_memory_limit_mb + 32)
        > cfg.memory_limit_mb
    ):
        # DuckDB limits are not the entire process, so this is a hard guard.
        raise ExecutorError(
            ERROR_CONFIG,
            "max_workers * (duckdb_memory_limit_mb + 32) exceeds memory_limit_mb",
        )
    return cfg


def load_config(path: str | Path | None = None) -> ExecutorConfig:
    values: dict[str, Any] = {}
    if path:
        config_path = Path(path)
        try:
            values = json.loads(config_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise ExecutorError(
                ERROR_CONFIG, f"cannot read config {config_path}: {exc}"
            ) from exc
    values = _deep_merge(values, _env_overrides())
    return config_from_dict(values)

