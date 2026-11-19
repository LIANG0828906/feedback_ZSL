"""Local metrics and parameter-version report ingestion."""

from __future__ import annotations

import csv
import io
import json
import math
import re
from pathlib import Path
from typing import Any, Mapping

from .errors import BusinessError
from .models import ReleasePlan, parse_timestamp, timestamp_to_iso


def _first(mapping: Mapping[str, Any], *keys: str) -> Any:
    for key in keys:
        if key in mapping and mapping[key] is not None:
            return mapping[key]
    return None


def _float_or_none(value: Any) -> float | None:
    if value is None or value == "":
        return None
    if isinstance(value, bool):
        return None
    try:
        parsed = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(parsed):
        return None
    return parsed


def _read_rows(path: str) -> list[dict[str, Any]]:
    source = Path(path)
    if source.is_dir():
        files = sorted(
            candidate
            for candidate in source.iterdir()
            if candidate.is_file()
            and candidate.suffix.lower() in {".jsonl", ".ndjson", ".json", ".csv"}
        )
        if not files:
            raise BusinessError(
                "DATA_FILE_INVALID",
                "data directory contains no JSON, JSONL, or CSV files",
                details={"path": str(source)},
                status_code=422,
            )
        rows: list[dict[str, Any]] = []
        for file_path in files:
            rows.extend(_read_file_rows(file_path))
        return rows
    return _read_file_rows(source)


def _read_file_rows(file_path: Path) -> list[dict[str, Any]]:
    if not file_path.is_file():
        raise BusinessError(
            "DATA_FILE_NOT_FOUND",
            f"data file not found: {file_path}",
            details={"path": str(file_path)},
            status_code=422,
        )
    try:
        text = file_path.read_text(encoding="utf-8-sig")
    except OSError as exc:
        raise BusinessError(
            "DATA_FILE_UNREADABLE",
            f"cannot read data file: {exc}",
            details={"path": str(file_path)},
            status_code=422,
        ) from exc

    if file_path.suffix.lower() == ".csv":
        reader = csv.DictReader(io.StringIO(text))
        return [dict(row) for row in reader]

    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        rows: list[dict[str, Any]] = []
        for line_number, line in enumerate(text.splitlines(), start=1):
            if not line.strip():
                continue
            try:
                item = json.loads(line)
            except json.JSONDecodeError as exc:
                raise BusinessError(
                    "DATA_FILE_INVALID",
                    f"invalid JSONL at line {line_number}: {exc}",
                    details={"path": str(file_path), "line": line_number},
                    status_code=422,
                ) from exc
            if not isinstance(item, Mapping):
                raise BusinessError(
                    "DATA_FILE_INVALID",
                    f"JSONL row {line_number} is not an object",
                    details={"path": str(file_path), "line": line_number},
                    status_code=422,
                )
            rows.append(dict(item))
        return rows

    if isinstance(parsed, list):
        return [dict(item) for item in parsed if isinstance(item, Mapping)]
    if isinstance(parsed, Mapping):
        for key in ("metrics", "samples", "data", "rows", "items", "reports"):
            value = parsed.get(key)
            if isinstance(value, list):
                return [dict(item) for item in value if isinstance(item, Mapping)]
        if any(
            key in parsed
            for key in ("instance_id", "instance", "host", "service_instance")
        ):
            return [dict(parsed)]
    raise BusinessError(
        "DATA_FILE_INVALID",
        "expected a JSON array, JSONL, CSV, or an object containing a rows array",
        details={"path": str(file_path)},
        status_code=422,
    )


def load_metrics(path: str, plan: ReleasePlan) -> list[dict[str, Any]]:
    """Load and normalize per-second metric rows for ``plan.release_id``."""

    instance_batch: dict[str, int] = {}
    for batch in plan.batches:
        for instance_id in batch.instances:
            instance_batch[instance_id] = batch.batch

    rows = _read_rows(path)
    normalized: dict[tuple[int, str, float], dict[str, Any]] = {}
    for row_index, row in enumerate(rows):
        row_release = _first(row, "release_id", "releaseId")
        if row_release is not None and str(row_release) != plan.release_id:
            continue

        instance_raw = _first(
            row,
            "instance_id",
            "instance",
            "host",
            "service_instance",
            "service_instance_id",
            "member",
        )
        if instance_raw is None:
            raise BusinessError(
                "METRIC_DATA_INVALID",
                "metric row is missing instance_id",
                details={"path": path, "row": row_index + 1},
                status_code=422,
            )
        instance_id = str(instance_raw).strip()
        if instance_id not in instance_batch:
            # A shared metrics file can contain several releases/plans.
            continue

        batch_raw = _first(
            row, "batch", "batch_no", "batch_id", "rollout_batch"
        )
        if batch_raw is None:
            batch_no = instance_batch[instance_id]
        else:
            batch_text = str(batch_raw).strip()
            if batch_text.isdigit():
                batch_no = int(batch_text)
            else:
                match = re.search(r"(\d+)\s*$", batch_text)
                batch_no = int(match.group(1)) if match else -1
        if plan.batch(batch_no) is None:
            raise BusinessError(
                "METRIC_DATA_INVALID",
                "metric row references a batch not present in the plan",
                details={
                    "path": path,
                    "row": row_index + 1,
                    "batch": batch_raw,
                    "instance_id": instance_id,
                },
                status_code=422,
            )

        timestamp_raw = _first(
            row,
            "timestamp",
            "observed_at",
            "observation_time",
            "recorded_at",
            "ts",
            "time",
        )
        if timestamp_raw is None:
            # Missing timestamps are accepted only to make hand-written
            # fixtures convenient; row order becomes a deterministic clock.
            observed_at = float(row_index)
            observed_at_raw = None
        else:
            try:
                observed_at = parse_timestamp(timestamp_raw)
            except (TypeError, ValueError) as exc:
                raise BusinessError(
                    "METRIC_DATA_INVALID",
                    f"invalid observation timestamp: {exc}",
                    details={
                        "path": path,
                        "row": row_index + 1,
                        "instance_id": instance_id,
                    },
                    status_code=422,
                ) from exc
            observed_at_raw = str(timestamp_raw)

        request_count = _float_or_none(
            _first(
                row,
                "request_count",
                "requests",
                "request_total",
                "total_requests",
            )
        )
        error_rate = _float_or_none(
            _first(
                row,
                "error_rate",
                "error_ratio",
                "http_error_rate",
            )
        )
        if error_rate is None:
            error_count = _float_or_none(
                _first(row, "error_count", "errors", "http_errors")
            )
            if (
                error_count is not None
                and request_count is not None
                and request_count > 0
            ):
                error_rate = error_count / request_count

        reject_rate = _float_or_none(
            _first(
                row,
                "connection_reject_rate",
                "connection_rejected_rate",
                "conn_reject_rate",
                "connection_rejection_rate",
                "reject_rate",
                "connection_rejected_ratio",
                "connection_rejection_ratio",
            )
        )
        if reject_rate is None:
            reject_count = _float_or_none(
                _first(
                    row,
                    "connection_reject_count",
                    "connection_rejected_count",
                    "rejected_connections",
                    "conn_reject_count",
                )
            )
            if (
                reject_count is not None
                and request_count is not None
                and request_count > 0
            ):
                reject_rate = reject_count / request_count

        for metric_name, metric_value in (
            ("error_rate", error_rate),
            ("connection_reject_rate", reject_rate),
        ):
            if metric_value is not None and (
                metric_value < 0 or metric_value > 1
            ):
                raise BusinessError(
                    "METRIC_DATA_INVALID",
                    f"{metric_name} must be between 0 and 1",
                    details={
                        "path": path,
                        "row": row_index + 1,
                        "instance_id": instance_id,
                    },
                    status_code=422,
                )

        parameter_version = _first(
            row,
            "parameter_version",
            "version",
            "param_version",
            "deployed_version",
        )
        if parameter_version is not None:
            parameter_version = str(parameter_version)

        normalized[(batch_no, instance_id, observed_at)] = {
            "release_id": plan.release_id,
            "batch": batch_no,
            "instance_id": instance_id,
            "observed_at": observed_at,
            "observed_at_raw": observed_at_raw,
            "error_rate": error_rate,
            "connection_reject_rate": reject_rate,
            "request_count": request_count if request_count is not None else 1.0,
            "parameter_version": parameter_version,
        }

    return [normalized[key] for key in sorted(normalized)]


def load_version_reports(
    path: str, plan: ReleasePlan
) -> list[dict[str, Any]]:
    rows = _read_rows(path)
    known_instances = set(plan.all_instances)
    normalized: dict[tuple[str, float], dict[str, Any]] = {}
    for row_index, row in enumerate(rows):
        row_release = _first(row, "release_id", "releaseId")
        if row_release is not None and str(row_release) != plan.release_id:
            continue
        instance_raw = _first(
            row,
            "instance_id",
            "instance",
            "host",
            "service_instance",
            "service_instance_id",
            "member",
        )
        if instance_raw is None:
            raise BusinessError(
                "VERSION_REPORT_INVALID",
                "version report is missing instance_id",
                details={"path": path, "row": row_index + 1},
                status_code=422,
            )
        instance_id = str(instance_raw).strip()
        if instance_id not in known_instances:
            continue
        timestamp_raw = _first(
            row,
            "timestamp",
            "observed_at",
            "observation_time",
            "recorded_at",
            "ts",
            "time",
        )
        if timestamp_raw is None:
            observed_at = float(row_index)
            observed_at_raw = None
        else:
            try:
                observed_at = parse_timestamp(timestamp_raw)
            except (TypeError, ValueError) as exc:
                raise BusinessError(
                    "VERSION_REPORT_INVALID",
                    f"invalid observation timestamp: {exc}",
                    details={
                        "path": path,
                        "row": row_index + 1,
                        "instance_id": instance_id,
                    },
                    status_code=422,
                ) from exc
            observed_at_raw = str(timestamp_raw)
        version = _first(
            row,
            "parameter_version",
            "version",
            "param_version",
            "deployed_version",
        )
        if version is None:
            raise BusinessError(
                "VERSION_REPORT_INVALID",
                "version report is missing parameter_version",
                details={
                    "path": path,
                    "row": row_index + 1,
                    "instance_id": instance_id,
                },
                status_code=422,
            )
        normalized[(instance_id, observed_at)] = {
            "release_id": plan.release_id,
            "instance_id": instance_id,
            "observed_at": observed_at,
            "observed_at_raw": observed_at_raw,
            "parameter_version": str(version),
        }
    return [normalized[key] for key in sorted(normalized)]


def report_records(
    rows: list[dict[str, Any]], value_key: str
) -> list[dict[str, str | None]]:
    """Convert normalized rows to the stable JSON report shape."""

    return [
        {
            "instance_id": row["instance_id"],
            "parameter_version": row.get(value_key),
            "observed_at": timestamp_to_iso(row["observed_at"]),
        }
        for row in sorted(
            rows, key=lambda item: (item["instance_id"], item["observed_at"])
        )
    ]
