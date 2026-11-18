"""Read-only replay of a committed handover decision."""

from __future__ import annotations

import copy
from datetime import timedelta
from typing import Any

from .core import build_snapshot
from .data import (
    SampleSet,
    format_timestamp,
    handover_datetime,
    parse_timestamp,
)
from .errors import HandoverError


def _series_timestamp(value: str | None) -> str | None:
    return value


def replay_snapshot(
    snapshot: dict[str, Any],
    samples: SampleSet,
    *,
    until: str | None = None,
    step_minutes: int = 1,
    include_series: bool = True,
) -> dict[str, Any]:
    if step_minutes <= 0:
        raise HandoverError(
            "REPLAY_RANGE_INVALID",
            "step_minutes must be a positive integer.",
            http_status=422,
        )
    handover = snapshot["handover"]
    start = handover_datetime(handover)
    common_last = samples.last_common_timestamp()
    if until is None:
        end = common_last
    else:
        end = parse_timestamp(until, field="until")
    if end <= start:
        raise HandoverError(
            "REPLAY_RANGE_INVALID",
            "The replay end must be after the handover time.",
            http_status=422,
            details={
                "handover_at": format_timestamp(start),
                "until": format_timestamp(end),
            },
        )
    if end > common_last:
        raise HandoverError(
            "INSUFFICIENT_REPLAY_SAMPLES",
            "The samples do not cover the requested replay interval.",
            http_status=422,
            details={
                "until": format_timestamp(end),
                "available_until": format_timestamp(common_last),
                "handover_id": snapshot["handover_id"],
            },
        )

    first_by_service: dict[str, str] = {}
    for service in samples.services:
        first = samples.first_timestamp_after(start, service=service)
        if first is None or first > end:
            raise HandoverError(
                "INSUFFICIENT_REPLAY_SAMPLES",
                "At least one service has no sample inside the replay interval.",
                http_status=422,
                details={
                    "handover_id": snapshot["handover_id"],
                    "service": service,
                },
            )
        first_by_service[service] = format_timestamp(first)

    available = samples.timestamps_after(start, until=end)
    if not available:
        raise HandoverError(
            "INSUFFICIENT_REPLAY_SAMPLES",
            "No sample lies inside the requested replay interval.",
            http_status=422,
            details={
                "handover_id": snapshot["handover_id"],
                "until": format_timestamp(end),
            },
        )
    if step_minutes == 1:
        references = available
    else:
        references = []
        target = start + timedelta(minutes=step_minutes)
        seen: set[str] = set()
        while target <= end:
            candidates = [
                value for value in available if value <= target
            ]
            if candidates:
                reference = candidates[-1]
                key = format_timestamp(reference)
                if key not in seen:
                    seen.add(key)
                    references.append(reference)
            target += timedelta(minutes=step_minutes)
        if not references:
            raise HandoverError(
                "INSUFFICIENT_REPLAY_SAMPLES",
                "No samples align with the requested replay step.",
                http_status=422,
                details={"handover_id": snapshot["handover_id"]},
            )

    max_points = int(snapshot.get("policy", {}).get("replay", {}).get("max_points", 20000))
    if len(references) > max_points:
        raise HandoverError(
            "REPLAY_TOO_MANY_POINTS",
            "The replay would exceed the configured point limit.",
            http_status=422,
            details={"points": len(references), "max_points": max_points},
        )

    actual_grade = snapshot["decision"]["summary"]["actual_grade"]
    actual_series: list[dict[str, Any]] = []
    recommended_series: list[dict[str, Any]] = []
    first_divergence: dict[str, Any] | None = None
    policy = snapshot["policy"]
    for reference in references:
        replay_handover = copy.deepcopy(handover)
        replay_handover["handover_at"] = format_timestamp(reference)
        replayed = build_snapshot(replay_handover, samples, policy)
        summary = replayed["decision"]["summary"]
        trigger = replayed["decision"]["trigger"]
        timestamp = format_timestamp(reference)
        actual_entry = {"timestamp": timestamp, "grade": actual_grade}
        recommended_entry = {
            "timestamp": timestamp,
            "grade": summary["overall_grade"],
            "affected_services": summary["affected_services"],
            "short_burn_rate": summary["short_burn_rate"],
            "long_burn_rate": summary["long_burn_rate"],
            "error_budget_remaining_pct": summary["minimum_budget_remaining_pct"],
            "burn_state": summary["burn_state"],
            "silence_decision": summary["silence_decision"],
            "suppression_effective": summary["suppression_effective"],
            "silence_invalid_count": summary["silence_invalid_count"],
            "trigger": trigger,
        }
        actual_series.append(actual_entry)
        recommended_series.append(recommended_entry)
        if (
            first_divergence is None
            and recommended_entry["grade"] != actual_grade
        ):
            first_divergence = {
                "timestamp": timestamp,
                "actual_grade": actual_grade,
                "recommended_grade": recommended_entry["grade"],
                "trigger_metric": trigger["metric"] if trigger else None,
                "trigger": trigger,
            }

    report: dict[str, Any] = {
        "snapshot_id": snapshot.get("snapshot_id"),
        "handover_id": snapshot["handover_id"],
        "handover_at": format_timestamp(start),
        "replay_from": format_timestamp(start),
        "replay_until": format_timestamp(end),
        "step_minutes": step_minutes,
        "actual_grade": actual_grade,
        "policy_version": snapshot["policy_version"],
        "sample_services": list(samples.services),
        "first_sample_after_handover": first_by_service,
        "recommended_grade_at_handover": snapshot["decision"]["summary"][
            "overall_grade"
        ],
        "silence_recommendation_at_handover": {
            "decision": snapshot["decision"]["silence"]["decision"],
            "suppression_effective": snapshot["decision"]["silence"][
                "suppression_effective"
            ],
            "invalid_count": snapshot["decision"]["silence"]["invalid_count"],
            "reasons": snapshot["decision"]["silence"]["reasons"],
        },
        "point_count": len(references),
        "first_divergence": first_divergence,
        "first_divergence_at": _series_timestamp(
            first_divergence["timestamp"] if first_divergence else None
        ),
        "first_divergence_trigger_metric": (
            first_divergence["trigger_metric"] if first_divergence else None
        ),
        "historical_records_modified": False,
    }
    if include_series:
        report["actual_series"] = actual_series
        report["recommended_series"] = recommended_series
    return report
