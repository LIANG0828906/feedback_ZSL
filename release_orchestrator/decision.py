"""Pure batch decision logic: aggregate plus per-instance safety gates."""

from __future__ import annotations

from typing import Any, Iterable

from .models import (
    BatchPlan,
    Decision,
    ReleasePlan,
    Violation,
    rounded,
    timestamp_to_iso,
    weighted_average,
)


def _normalize_conflicts(conflicts: Iterable[dict[str, Any]]) -> tuple[dict[str, Any], ...]:
    result = []
    for conflict in conflicts:
        result.append(
            {
                "instance_id": str(conflict["instance_id"]),
                "release_id": str(conflict["release_id"]),
                "batch": int(conflict["batch"]),
            }
        )
    return tuple(
        sorted(
            result,
            key=lambda item: (
                item["instance_id"],
                item["release_id"],
                item["batch"],
            ),
        )
    )


def evaluate_batch(
    plan: ReleasePlan,
    batch: BatchPlan,
    samples: Iterable[dict[str, Any]],
    *,
    conflicts: Iterable[dict[str, Any]] = (),
) -> Decision:
    """Evaluate one batch using already-normalized samples.

    A batch only passes when all three conditions hold:
    1. every planned instance has at least the minimum sample count;
    2. both batch-level rates are within their thresholds;
    3. both instance-level rates are within their thresholds for every
       observed instance.

    One bad instance is therefore sufficient to stop a batch even when the
    request-weighted batch average is below its threshold.
    """

    thresholds = plan.thresholds
    minimum = thresholds.minimum_samples_per_instance
    grouped: dict[str, list[dict[str, Any]]] = {
        instance_id: [] for instance_id in batch.instances
    }
    for sample in samples:
        instance_id = str(sample.get("instance_id", ""))
        if instance_id in grouped:
            grouped[instance_id].append(sample)

    instance_rows: list[dict[str, Any]] = []
    violations: list[Violation] = []
    all_valid_rows: list[dict[str, Any]] = []
    all_times: list[float] = []

    for instance_id in batch.instances:
        rows = sorted(
            grouped[instance_id], key=lambda item: float(item["observed_at"])
        )[: batch.observation_seconds]
        times = [float(row["observed_at"]) for row in rows]
        all_times.extend(times)
        valid_rows = [
            row
            for row in rows
            if row.get("error_rate") is not None
            and row.get("connection_reject_rate") is not None
        ]
        all_valid_rows.extend(valid_rows)
        error_rate = weighted_average(valid_rows, "error_rate")
        reject_rate = weighted_average(valid_rows, "connection_reject_rate")
        sample_count = len(valid_rows)
        latest_version = None
        version_rows = [
            row for row in rows if row.get("parameter_version") is not None
        ]
        if version_rows:
            latest_version = version_rows[-1].get("parameter_version")

        instance_rows.append(
            {
                "instance_id": instance_id,
                "sample_count": sample_count,
                "error_rate": rounded(error_rate),
                "connection_reject_rate": rounded(reject_rate),
                "parameter_version": latest_version,
                "observed_from": timestamp_to_iso(min(times) if times else None),
                "observed_to": timestamp_to_iso(max(times) if times else None),
            }
        )

        if sample_count < minimum:
            violations.append(
                Violation(
                    scope="instance",
                    instance_id=instance_id,
                    metric="sample_count",
                    value=float(sample_count),
                    threshold=float(minimum),
                    sample_count=sample_count,
                )
            )
        if error_rate is not None and error_rate > thresholds.instance_error_rate:
            violations.append(
                Violation(
                    scope="instance",
                    instance_id=instance_id,
                    metric="error_rate",
                    value=error_rate,
                    threshold=thresholds.instance_error_rate,
                    sample_count=sample_count,
                )
            )
        if (
            reject_rate is not None
            and reject_rate > thresholds.instance_connection_reject_rate
        ):
            violations.append(
                Violation(
                    scope="instance",
                    instance_id=instance_id,
                    metric="connection_reject_rate",
                    value=reject_rate,
                    threshold=thresholds.instance_connection_reject_rate,
                    sample_count=sample_count,
                )
            )

    aggregate_error = weighted_average(all_valid_rows, "error_rate")
    aggregate_reject = weighted_average(
        all_valid_rows, "connection_reject_rate"
    )
    aggregate_sample_count = len(all_valid_rows)
    aggregate_request_count = round(
        sum(float(row.get("request_count") or 1.0) for row in all_valid_rows), 6
    )
    aggregate = {
        "error_rate": {
            "value": rounded(aggregate_error),
            "threshold": thresholds.batch_error_rate,
            "sample_count": aggregate_sample_count,
            "request_count": aggregate_request_count,
        },
        "connection_reject_rate": {
            "value": rounded(aggregate_reject),
            "threshold": thresholds.batch_connection_reject_rate,
            "sample_count": aggregate_sample_count,
            "request_count": aggregate_request_count,
        },
    }

    aggregate_violations: list[Violation] = []
    if (
        aggregate_error is not None
        and aggregate_error > thresholds.batch_error_rate
    ):
        aggregate_violations.append(
            Violation(
                scope="batch",
                instance_id=None,
                metric="error_rate",
                value=aggregate_error,
                threshold=thresholds.batch_error_rate,
                sample_count=aggregate_sample_count,
            )
        )
    if (
        aggregate_reject is not None
        and aggregate_reject > thresholds.batch_connection_reject_rate
    ):
        aggregate_violations.append(
            Violation(
                scope="batch",
                instance_id=None,
                metric="connection_reject_rate",
                value=aggregate_reject,
                threshold=thresholds.batch_connection_reject_rate,
                sample_count=aggregate_sample_count,
            )
        )

    insufficient = any(
        violation.metric == "sample_count" for violation in violations
    )
    normalized_conflicts = _normalize_conflicts(conflicts)
    if normalized_conflicts:
        result = "aborted"
        reason_code = "INSTANCE_OCCUPANCY_CONFLICT"
    elif insufficient:
        result = "aborted"
        reason_code = "INSUFFICIENT_SAMPLES"
    elif aggregate_violations:
        result = "aborted"
        reason_code = "AGGREGATE_THRESHOLD_EXCEEDED"
    elif violations:
        result = "aborted"
        reason_code = "INSTANCE_THRESHOLD_EXCEEDED"
    else:
        result = "passed"
        reason_code = None

    ordered_violations = tuple(
        sorted(
            [*aggregate_violations, *violations],
            key=lambda item: (
                0 if item.scope == "batch" else 1,
                item.instance_id or "",
                item.metric,
            ),
        )
    )

    return Decision(
        release_id=plan.release_id,
        batch=batch.batch,
        result=result,
        reason_code=reason_code,
        aggregate=aggregate,
        instances=tuple(instance_rows),
        violations=ordered_violations,
        sample_count=aggregate_sample_count,
        observed_from=min(all_times) if all_times else None,
        observed_to=max(all_times) if all_times else None,
        rollback_target_version=plan.previous_stable_version,
        observation_seconds=batch.observation_seconds,
        minimum_samples=minimum,
        conflicts=normalized_conflicts,
    )
