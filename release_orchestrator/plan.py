"""Gray-plan parsing and all-or-nothing validation."""

from __future__ import annotations

import copy
import json
import math
import os
import re
from pathlib import Path
from typing import Any, Mapping

from .errors import PlanValidationError, issue
from .models import BatchPlan, ReleasePlan, Thresholds, digest_json


def _first(mapping: Mapping[str, Any], *keys: str, default: Any = None) -> Any:
    for key in keys:
        if key in mapping and mapping[key] is not None:
            return mapping[key]
    return default


def _number(value: Any, field_name: str, issues: list[dict[str, Any]]) -> float | None:
    if isinstance(value, bool):
        issues.append(
            issue("PLAN_SCHEMA_INVALID", f"{field_name} must be a number")
        )
        return None
    try:
        parsed = float(value)
    except (TypeError, ValueError):
        issues.append(
            issue("PLAN_SCHEMA_INVALID", f"{field_name} must be a number")
        )
        return None
    if not math.isfinite(parsed):
        issues.append(
            issue("PLAN_SCHEMA_INVALID", f"{field_name} must be finite")
        )
        return None
    return parsed


def _batch_number(value: Any, position: int) -> int | None:
    if value is None:
        return position
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    if isinstance(value, str):
        text = value.strip()
        if text.isdigit():
            return int(text)
        match = re.search(r"(\d+)\s*$", text)
        if match:
            return int(match.group(1))
    return None


def _resolve_optional_path(base_dir: str | None, value: Any) -> str | None:
    if value is None or value == "":
        return None
    text = os.fspath(value)
    path = Path(text)
    if not path.is_absolute() and base_dir:
        path = Path(base_dir) / path
    return str(path)


def _parse_thresholds(raw: Mapping[str, Any], issues: list[dict[str, Any]]) -> Thresholds:
    defaults = Thresholds()
    raw_thresholds = raw.get("thresholds") or {}
    if not isinstance(raw_thresholds, Mapping):
        issues.append(
            issue("PLAN_SCHEMA_INVALID", "thresholds must be a JSON object")
        )
        raw_thresholds = {}

    batch_cfg = raw_thresholds.get("batch")
    if not isinstance(batch_cfg, Mapping):
        batch_cfg = raw_thresholds.get("batch_thresholds")
    if not isinstance(batch_cfg, Mapping):
        batch_cfg = {}

    instance_cfg = raw_thresholds.get("instance")
    if not isinstance(instance_cfg, Mapping):
        instance_cfg = raw_thresholds.get("instance_thresholds")
    if not isinstance(instance_cfg, Mapping):
        instance_cfg = {}

    def metric_value(
        source: Mapping[str, Any],
        *keys: str,
        default: float,
        top_level_keys: tuple[str, ...] = (),
        nested_metric: str | None = None,
        scope: str | None = None,
    ) -> float:
        value = _first(source, *keys)
        if value is None:
            value = _first(raw, *top_level_keys)
        if value is None:
            value = _first(raw_thresholds, *keys)
        if isinstance(value, Mapping):
            value = None
        if value is None and nested_metric and scope:
            for candidate in (nested_metric, *keys):
                nested = raw_thresholds.get(candidate)
                if isinstance(nested, Mapping):
                    value = _first(nested, scope)
                    if value is not None:
                        break
        parsed = default if value is None else _number(value, keys[0], issues)
        if parsed is None:
            return default
        if parsed < 0 or parsed > 1:
            issues.append(
                issue(
                    "PLAN_SCHEMA_INVALID",
                    f"threshold {keys[0]} must be between 0 and 1",
                )
            )
            return default
        return parsed

    batch_error = metric_value(
        batch_cfg,
        "error_rate",
        "error_ratio",
        default=defaults.batch_error_rate,
        top_level_keys=(
            "batch_error_rate",
            "batch_error_rate_threshold",
            "error_rate_threshold",
        ),
        nested_metric="error_rate",
        scope="batch",
    )
    batch_reject = metric_value(
        batch_cfg,
        "connection_reject_rate",
        "connection_rejected_rate",
        "connection_rejection_rate",
        "conn_reject_rate",
        default=defaults.batch_connection_reject_rate,
        top_level_keys=(
            "batch_connection_reject_rate",
            "batch_connection_reject_rate_threshold",
        ),
        nested_metric="connection_reject_rate",
        scope="batch",
    )
    instance_error = metric_value(
        instance_cfg,
        "error_rate",
        "error_ratio",
        default=defaults.instance_error_rate,
        top_level_keys=(
            "instance_error_rate",
            "instance_error_rate_threshold",
        ),
        nested_metric="error_rate",
        scope="instance",
    )
    instance_reject = metric_value(
        instance_cfg,
        "connection_reject_rate",
        "connection_rejected_rate",
        "connection_rejection_rate",
        "conn_reject_rate",
        default=defaults.instance_connection_reject_rate,
        top_level_keys=(
            "instance_connection_reject_rate",
            "instance_connection_reject_rate_threshold",
        ),
        nested_metric="connection_reject_rate",
        scope="instance",
    )

    minimum_raw = _first(
        raw_thresholds,
        "minimum_samples_per_instance",
        "min_samples",
        "min_samples_per_instance",
        "minimum_samples",
        "minimum_observed_samples",
    )
    if minimum_raw is None:
        minimum_raw = _first(
            raw,
            "minimum_samples_per_instance",
            "min_samples",
            "min_samples_per_instance",
            "minimum_observed_samples",
            default=defaults.minimum_samples_per_instance,
        )
    minimum = _number(
        minimum_raw, "minimum_samples_per_instance", issues
    )
    if minimum is None:
        minimum = float(defaults.minimum_samples_per_instance)
    if minimum < 1 or not minimum.is_integer():
        issues.append(
            issue(
                "PLAN_SCHEMA_INVALID",
                "minimum_samples_per_instance must be a positive integer",
            )
        )
        minimum = float(defaults.minimum_samples_per_instance)

    return Thresholds(
        batch_error_rate=batch_error,
        batch_connection_reject_rate=batch_reject,
        instance_error_rate=instance_error,
        instance_connection_reject_rate=instance_reject,
        minimum_samples_per_instance=int(minimum),
    )


def parse_plan(raw: Any, *, base_dir: str | None = None) -> ReleasePlan:
    """Validate a gray plan and return an immutable canonical plan.

    Every validation issue is collected so an operator can fix all obvious
    problems in one pass. No state is written by this function.
    """

    issues: list[dict[str, Any]] = []
    if not isinstance(raw, Mapping):
        raise PlanValidationError(
            [issue("PLAN_SCHEMA_INVALID", "plan must be a JSON object")]
        )

    payload = copy.deepcopy(dict(raw))
    versions = payload.get("versions")
    if not isinstance(versions, Mapping):
        versions = {}
    parameter_versions = payload.get("parameter_versions")
    if not isinstance(parameter_versions, Mapping):
        parameter_versions = {}

    release_id = _first(payload, "release_id", "releaseId", "id")
    if not isinstance(release_id, str) or not release_id.strip():
        issues.append(
            issue("PLAN_SCHEMA_INVALID", "release_id is required")
        )
        release_id = ""
    else:
        release_id = release_id.strip()

    parameter_key = _first(payload, "parameter_key", "parameterKey", "key")
    if not isinstance(parameter_key, str) or not parameter_key.strip():
        issues.append(
            issue("PLAN_SCHEMA_INVALID", "parameter_key is required")
        )
        parameter_key = ""
    else:
        parameter_key = parameter_key.strip()

    target_version = _first(
        payload,
        "target_version",
        "targetVersion",
        "target_parameter_version",
    )
    if target_version is None:
        target_version = _first(versions, "target", "target_version")
    if target_version is None:
        target_version = _first(
            parameter_versions, "target", "target_version"
        )
    if not isinstance(target_version, str) or not target_version.strip():
        issues.append(
            issue(
                "PLAN_VERSION_MISSING",
                "target parameter version is missing",
                field="target_version",
            )
        )
        target_version = ""
    else:
        target_version = target_version.strip()

    previous_stable = _first(
        payload,
        "previous_stable_version",
        "previous_version",
        "rollback_version",
        "stable_version",
    )
    if previous_stable is None:
        previous_stable = _first(
            versions,
            "previous_stable",
            "previous_stable_version",
            "stable",
        )
    if previous_stable is None:
        previous_stable = _first(
            parameter_versions,
            "previous_stable",
            "previous_stable_version",
            "stable",
        )
    if not isinstance(previous_stable, str) or not previous_stable.strip():
        issues.append(
            issue(
                "PLAN_VERSION_MISSING",
                "previous stable parameter version is missing",
                field="previous_stable_version",
            )
        )
        previous_stable = ""
    else:
        previous_stable = previous_stable.strip()

    if target_version and previous_stable and target_version == previous_stable:
        issues.append(
            issue(
                "PLAN_VERSION_MISSING",
                "target and previous stable parameter versions must differ",
                field="previous_stable_version",
            )
        )

    thresholds = _parse_thresholds(payload, issues)

    raw_batches = _first(payload, "batches", "gray_batches", "rollout_batches")
    if not isinstance(raw_batches, list) or not raw_batches:
        issues.append(
            issue("PLAN_SCHEMA_INVALID", "batches must be a non-empty array")
        )
        raw_batches = []

    batch_plans: list[BatchPlan] = []
    batch_numbers: dict[int, int] = {}
    instance_batches: dict[str, list[int]] = {}
    default_start = 0.0
    batch_version_flags: list[tuple[int, bool, str | None]] = []

    for position, raw_batch in enumerate(raw_batches, start=1):
        if not isinstance(raw_batch, Mapping):
            issues.append(
                issue(
                    "PLAN_SCHEMA_INVALID",
                    "each batch must be a JSON object",
                    batch=position,
                )
            )
            continue

        raw_batch_no = _first(
            raw_batch, "batch", "batch_no", "batch_id", "index", "number"
        )
        batch_no = _batch_number(raw_batch_no, position)
        if batch_no is None or batch_no < 1:
            issues.append(
                issue(
                    "PLAN_SCHEMA_INVALID",
                    "batch number must be a positive integer",
                    batch=position,
                )
            )
            continue

        if batch_no in batch_numbers:
            issues.append(
                issue(
                    "PLAN_BATCH_OVERLAP",
                    f"duplicate batch number {batch_no}",
                    batch=batch_no,
                    other_batch=batch_numbers[batch_no],
                )
            )
            continue
        batch_numbers[batch_no] = position

        raw_instances = _first(
            raw_batch, "instances", "instance_ids", "members", "hosts"
        )
        if not isinstance(raw_instances, list) or not raw_instances:
            issues.append(
                issue(
                    "PLAN_SCHEMA_INVALID",
                    "batch instances must be a non-empty array",
                    batch=batch_no,
                )
            )
            raw_instances = []

        instances: list[str] = []
        local_seen = set()
        for raw_instance in raw_instances:
            if not isinstance(raw_instance, str) or not raw_instance.strip():
                issues.append(
                    issue(
                        "PLAN_SCHEMA_INVALID",
                        "instance id must be a non-empty string",
                        batch=batch_no,
                    )
                )
                continue
            instance_id = raw_instance.strip()
            if instance_id in local_seen:
                issues.append(
                    issue(
                        "PLAN_DUPLICATE_INSTANCE",
                        "instance is assigned twice in one batch",
                        batch=batch_no,
                        instance_id=instance_id,
                        batches=[batch_no],
                    )
                )
                continue
            local_seen.add(instance_id)
            instances.append(instance_id)
            instance_batches.setdefault(instance_id, []).append(batch_no)

        observation_raw = _first(
            raw_batch,
            "observation_seconds",
            "observe_seconds",
            "duration_seconds",
            "window_seconds",
        )
        observation_seconds = _number(
            observation_raw,
            f"batch {batch_no} observation_seconds",
            issues,
        )
        if (
            observation_seconds is None
            or observation_seconds <= 0
            or not observation_seconds.is_integer()
        ):
            if observation_seconds is not None:
                issues.append(
                    issue(
                        "PLAN_SCHEMA_INVALID",
                        "observation_seconds must be a positive integer",
                        batch=batch_no,
                    )
                )
            observation_seconds = 1.0

        start_raw = _first(
            raw_batch,
            "start_offset_seconds",
            "observation_start_seconds",
            "start_seconds",
        )
        start_offset: float | None = None
        if start_raw is not None:
            start_offset = _number(
                start_raw,
                f"batch {batch_no} start_offset_seconds",
                issues,
            )
            if start_offset is not None and start_offset < 0:
                issues.append(
                    issue(
                        "PLAN_SCHEMA_INVALID",
                        "start_offset_seconds cannot be negative",
                        batch=batch_no,
                    )
                )

        batch_target = _first(
            raw_batch,
            "target_version",
            "parameter_version",
            "version",
        )
        batch_version_flags.append(
            (
                batch_no,
                any(
                    key in raw_batch
                    for key in ("target_version", "parameter_version", "version")
                ),
                instances[0] if instances else None,
            )
        )
        if batch_target is not None and (
            not isinstance(batch_target, str) or not batch_target.strip()
        ):
            issues.append(
                issue(
                    "PLAN_VERSION_MISSING",
                    "batch parameter version is empty",
                    batch=batch_no,
                )
            )

        batch_plans.append(
            BatchPlan(
                batch=batch_no,
                instances=tuple(sorted(instances)),
                observation_seconds=int(observation_seconds),
                start_offset_seconds=start_offset,
            )
        )
        if start_offset is None:
            default_start += float(observation_seconds)

    for instance_id, batches in sorted(instance_batches.items()):
        if len(batches) > 1:
            issues.append(
                issue(
                    "PLAN_DUPLICATE_INSTANCE",
                    "instance is assigned to more than one batch",
                    batch=batches[1],
                    instance_id=instance_id,
                    batches=sorted(set(batches)),
                )
            )

    if any(flag for _, flag, _ in batch_version_flags) and not all(
        flag for _, flag, _ in batch_version_flags
    ):
        for batch_no, flag, first_instance in batch_version_flags:
            if not flag:
                issues.append(
                    issue(
                        "PLAN_VERSION_MISSING",
                        "batch parameter version is missing",
                        batch=batch_no,
                        instance_id=first_instance,
                    )
                )

    # Explicit start offsets and durations define observation windows. When a
    # plan omits offsets, batches are interpreted as sequential windows.
    intervals: list[tuple[int, float, float]] = []
    cursor = 0.0
    for batch in sorted(batch_plans, key=lambda item: item.batch):
        start = (
            batch.start_offset_seconds
            if batch.start_offset_seconds is not None
            else cursor
        )
        end = start + batch.observation_seconds
        intervals.append((batch.batch, start, end))
        cursor = max(cursor, end)
    for left_index, (left_batch, left_start, left_end) in enumerate(intervals):
        for right_batch, right_start, right_end in intervals[left_index + 1 :]:
            if left_start < right_end and right_start < left_end:
                issues.append(
                    issue(
                        "PLAN_BATCH_OVERLAP",
                        "batch observation windows overlap",
                        batch=left_batch,
                        other_batch=right_batch,
                    )
                )

    metrics_value = _first(
        payload,
        "metrics_file",
        "metrics_path",
        "samples_file",
    )
    reports_value = _first(
        payload,
        "version_reports_file",
        "version_reports_path",
        "parameter_reports_file",
    )
    metrics_file = _resolve_optional_path(base_dir, metrics_value)
    version_reports_file = _resolve_optional_path(base_dir, reports_value)

    if issues:
        raise PlanValidationError(issues)

    ordered_batches = tuple(sorted(batch_plans, key=lambda item: item.batch))
    plan = ReleasePlan(
        release_id=release_id,
        parameter_key=parameter_key,
        target_version=target_version,
        previous_stable_version=previous_stable,
        batches=ordered_batches,
        thresholds=thresholds,
        metrics_file=metrics_file,
        version_reports_file=version_reports_file,
        raw=payload,
    )
    canonical = plan.to_dict()
    return ReleasePlan(
        release_id=plan.release_id,
        parameter_key=plan.parameter_key,
        target_version=plan.target_version,
        previous_stable_version=plan.previous_stable_version,
        batches=plan.batches,
        thresholds=plan.thresholds,
        metrics_file=plan.metrics_file,
        version_reports_file=plan.version_reports_file,
        raw=payload,
        plan_sha256=digest_json(canonical),
    )


def load_plan(path: str | os.PathLike[str]) -> ReleasePlan:
    plan_path = Path(path).expanduser().resolve()
    if not plan_path.is_file():
        raise PlanValidationError(
            [
                issue(
                    "PLAN_NOT_FOUND",
                    f"plan file not found: {plan_path}",
                )
            ]
        )
    try:
        raw = json.loads(plan_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise PlanValidationError(
            [
                issue(
                    "PLAN_SCHEMA_INVALID",
                    f"cannot read plan JSON: {exc}",
                )
            ]
        ) from exc
    plan = parse_plan(raw, base_dir=str(plan_path.parent))
    return ReleasePlan(
        release_id=plan.release_id,
        parameter_key=plan.parameter_key,
        target_version=plan.target_version,
        previous_stable_version=plan.previous_stable_version,
        batches=plan.batches,
        thresholds=plan.thresholds,
        metrics_file=plan.metrics_file,
        version_reports_file=plan.version_reports_file,
        plan_path=str(plan_path),
        plan_sha256=plan.plan_sha256,
        raw=plan.raw,
    )
