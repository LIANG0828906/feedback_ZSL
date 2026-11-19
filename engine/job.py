"""One-shot job orchestration and deterministic reporting."""

from __future__ import annotations

import json
import hashlib
import os
import re
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .catalog import Catalog
from .config import ExecutorConfig
from .dialects import get_dialect, normalize_dialect_name
from .errors import (
    ERROR_IO,
    ERROR_INTERNAL,
    EXIT_CODES,
    ExecutorError,
)
from .execution import RuntimeExecutor
from .planner import plan_query
from .runtime import JobWorkspace, state_signature
from .sql import parse_sql
from .streaming import run_streaming_dedup


@dataclass
class JobOptions:
    query_path: Path
    data_dir: Path
    result_path: Path
    report_path: Path
    dialect: str | None = None
    config: ExecutorConfig = field(default_factory=ExecutorConfig)
    run_id: str | None = None
    resume: bool = False


def _relative(path: Path) -> str:
    try:
        return str(path.resolve().relative_to(Path.cwd().resolve()))
    except ValueError:
        return path.name


def _dialect_from_comment(sql: str) -> str | None:
    match = re.search(r"(?:--|/\*)\s*dialect\s*:\s*([A-Za-z]+)", sql)
    return normalize_dialect_name(match.group(1)) if match else None


def _deterministic_memory_bound(config: ExecutorConfig) -> int:
    # DuckDB memory limits are hard per worker. Add a fixed 32 MiB Python
    # bookkeeping allowance per active unit and cap at the job's budget.
    per_worker = config.duckdb_memory_limit_bytes + 32 * 1024 * 1024
    return min(config.memory_limit_bytes, config.max_workers * per_worker)


def run_job(options: JobOptions) -> tuple[dict[str, Any], int]:
    started = time.perf_counter()
    query_path = Path(options.query_path).resolve()
    data_dir = Path(options.data_dir).resolve()
    result_path = Path(options.result_path).resolve()
    report_path = Path(options.report_path).resolve()
    report_path.parent.mkdir(parents=True, exist_ok=True)
    result_path.parent.mkdir(parents=True, exist_ok=True)
    if result_path.exists():
        result_path.unlink()
    suffix = ".parquet" if options.config.result_format == "parquet" else ".csv"
    result_path = result_path.with_suffix(suffix)

    if options.run_id:
        run_id = options.run_id
    else:
        token = hashlib.sha256(
            f"{_relative(query_path)}|{_relative(result_path)}|{_relative(report_path)}|{options.dialect or 'auto'}".encode(
                "utf-8"
            )
        ).hexdigest()[:12]
        run_id = f"{query_path.stem}-{options.dialect or 'auto'}-{token}"
    workspace = JobWorkspace(
        options.config.temp_dir, run_id, resume=options.resume
    )
    report: dict[str, Any] = {
        "schema_version": 1,
        "job": {
            "query_file": _relative(query_path),
            "data_dir": _relative(data_dir),
            "result_format": options.config.result_format,
            "run_id": run_id,
        },
        "status": "failed",
        "error_code": None,
        "error": None,
        "duration_ms": 0,
    }

    try:
        try:
            sql = query_path.read_text(encoding="utf-8")
            schema_text = (data_dir / "schema.json").read_text(encoding="utf-8")
        except OSError as exc:
            raise ExecutorError(ERROR_IO, f"cannot read job input: {exc}") from exc
        workspace.state.configure_signature(
            state_signature(sql, schema_text, options.config)
        )
        dialect_name = options.dialect or _dialect_from_comment(sql)
        if not dialect_name:
            raise ExecutorError(
                ERROR_INTERNAL,
                "dialect was not supplied and the query has no '-- dialect:' header",
            )
        dialect = get_dialect(dialect_name)
        query = parse_sql(sql)
        catalog = Catalog.load(data_dir)
        planning = plan_query(query, catalog, dialect)
        report["job"]["dialect"] = dialect.name
        report["dialect_semantics"] = dialect.report()
        report["planning"] = {
            "baseline_max_intermediate_rows": round(
                planning.baseline_max_intermediate, 3
            ),
            "planned_max_intermediate_rows": round(
                planning.planned_max_intermediate, 3
            ),
            "filters_pushed_down": sum(len(b.filters) for b in planning.bases.values()),
            "partial_aggregate_pushed_down": planning.partial is not None,
            "statistics": {
                alias: {
                    "table": base.table.name,
                    "rows": int(base.table.row_count or 0),
                    "join_key_cardinality": {
                        column: int(base.table.distinct_count(column))
                        for column in sorted(
                            {
                                col
                                for pred in planning.predicates
                                for table_alias, col in (pred.left, pred.right)
                                if table_alias == alias
                            }
                        )
                    },
                }
                for alias, base in sorted(planning.bases.items())
            },
        }
        report["resource_limits"] = {
            "batch_rows": options.config.batch_rows,
            "max_concurrent_units": options.config.max_workers,
            "memory_limit_bytes": options.config.memory_limit_bytes,
            "duckdb_memory_limit_bytes": options.config.duckdb_memory_limit_bytes,
            "aggregate_spill_threshold_bytes": options.config.agg_memory_threshold_bytes,
            "temp_limit_bytes": options.config.temp_limit_bytes,
            "partitions": options.config.partitions,
        }

        if query.distinct_on:
            execution = run_streaming_dedup(
                query,
                catalog,
                dialect,
                options.config,
                workspace,
                result_path,
            )
        else:
            executor = RuntimeExecutor(planning, options.config, workspace)
            execution = executor.execute(result_path)
            report["planning"]["actual_max_intermediate_rows"] = _max_actual_rows(
                execution.plan_tree
            )

        report.update(
            {
                "status": "success",
                "error_code": None,
                "error": None,
                "plan": execution.plan_tree,
                "operators": execution.metrics,
                "repartition": {
                    "operations": execution.repartition_operations,
                    "partitions": execution.repartition_partitions,
                },
                "faults": {
                    "injection": options.config.fault_injection,
                    "max_retries": options.config.max_retries,
                    "retried_partitions": execution.retry_partitions,
                    "completed_partitions": execution.completed_partitions,
                },
                "stream": execution.stream
                or {
                    "mode": "not_applicable",
                    "late_records": 0,
                    "late_same_key_records": 0,
                    "final_watermark": None,
                },
                "resources": {
                    "peak_memory_bytes": _deterministic_memory_bound(options.config),
                    "peak_memory_is_bounded_estimate": True,
                    "max_active_partitions": execution.max_active_partitions,
                    "spill_bytes": execution.spill_bytes,
                },
                "result": {
                    "path": _relative(result_path),
                    "rows": execution.result_rows,
                    "sha256": execution.result_sha256,
                    "format": options.config.result_format,
                },
                "repartition_count": execution.repartition_partitions,
                "retry_partitions": execution.retry_partitions,
                "late_records": (
                    execution.stream.get("late_records", 0)
                    if execution.stream
                    else 0
                ),
                "spill_bytes": execution.spill_bytes,
            }
        )
        report["duration_ms"] = int((time.perf_counter() - started) * 1000)
        _write_json(report_path, report)
        workspace.cleanup_after_success()
        return report, 0
    except ExecutorError as exc:
        if result_path.exists():
            result_path.unlink()
        report["status"] = "failed"
        report["error_code"] = exc.code
        report["error"] = {
            "message": exc.message,
            "details": exc.details,
        }
        completed = workspace.state.completed_partition_ids()
        report["faults"] = {
            "injection": options.config.fault_injection,
            "max_retries": options.config.max_retries,
            "retried_partitions": workspace.state.retry_list(),
            "completed_partitions": completed,
        }
        report["duration_ms"] = int((time.perf_counter() - started) * 1000)
        failure = {
            "status": "failed",
            "error_code": exc.code,
            "error": {"message": exc.message, "details": exc.details},
            "completed_partitions": completed,
            "retried_partitions": workspace.state.retry_list(),
            "partial_result_removed": True,
        }
        workspace.write_failure_manifest(report_path.parent, failure)
        _write_json(report_path, report)
        return report, EXIT_CODES.get(exc.code, 1)
    except BaseException as exc:
        if result_path.exists():
            result_path.unlink()
        business = ExecutorError(
            ERROR_INTERNAL,
            f"{type(exc).__name__}: {exc}",
        )
        report["status"] = "failed"
        report["error_code"] = business.code
        report["error"] = {"message": business.message, "details": business.details}
        report["faults"] = {
            "injection": options.config.fault_injection,
            "max_retries": options.config.max_retries,
            "retried_partitions": workspace.state.retry_list(),
            "completed_partitions": workspace.state.completed_partition_ids(),
        }
        report["duration_ms"] = int((time.perf_counter() - started) * 1000)
        workspace.write_failure_manifest(report_path.parent, report)
        _write_json(report_path, report)
        return report, EXIT_CODES.get(business.code, 1)


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(
        json.dumps(payload, sort_keys=True, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    os.replace(tmp, path)


def _max_actual_rows(node: dict[str, Any]) -> int:
    current = int(node.get("actual_rows") or 0)
    child_values = [
        _max_actual_rows(child)
        for child in node.get("children", [])
    ]
    return max([current, *child_values])

