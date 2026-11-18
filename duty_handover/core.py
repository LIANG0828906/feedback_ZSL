"""SLI windows, burn rates, incident grading, and silence decisions."""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any, Iterable, Sequence

from .data import (
    Sample,
    SampleSet,
    format_timestamp,
    handover_datetime,
    infer_slo,
    parse_timestamp,
)
from .errors import HandoverError
from .policy import (
    RISK_ORDER,
    SEVERITY_RANK,
    risk_at_least,
)


def rounded(value: float | None, digits: int = 6) -> float | None:
    if value is None:
        return None
    return round(float(value), digits)


def _safe_ratio(numerator: float, denominator: float) -> float:
    if denominator == 0:
        return 0.0
    return numerator / denominator


def calculate_window(
    samples: Sequence[Sample], start: datetime, end: datetime
) -> dict[str, Any]:
    """Calculate budget and burn for an inclusive timestamp window.

    A minute contributes its failed requests to availability consumption.
    When P99 exceeds the row's latency threshold, every request in that minute
    is additionally treated as a bad event. The two causes are unioned, so a
    failed request is never counted twice.
    """

    if end < start:
        raise HandoverError(
            "INVALID_WINDOW",
            "The window end must not precede its start.",
            details={
                "start": format_timestamp(start),
                "end": format_timestamp(end),
            },
        )
    rows = sorted(samples, key=lambda row: row.timestamp)
    expected_minutes = max(1, int((end - start).total_seconds() // 60) + 1)
    total_requests = sum(row.total_requests for row in rows)
    success_requests = sum(row.success_requests for row in rows)
    failed_requests = sum(
        row.total_requests - row.success_requests for row in rows
    )
    budget_total = sum(
        row.total_requests * (1.0 - row.availability_target) for row in rows
    )
    availability_consumed = float(failed_requests)
    latency_penalty = 0.0
    availability_breaches = 0
    latency_breaches = 0
    both_breaches = 0
    latency_only_breaches = 0
    for row in rows:
        availability_bad = (
            row.total_requests > 0
            and row.success_requests / row.total_requests < row.availability_target
        )
        latency_bad = row.p99_latency_ms > row.latency_threshold_ms
        if availability_bad:
            availability_breaches += 1
        if latency_bad:
            latency_breaches += 1
            latency_penalty += row.success_requests
        if availability_bad and latency_bad:
            both_breaches += 1
        elif latency_bad:
            latency_only_breaches += 1
    consumed = availability_consumed + latency_penalty
    remaining = budget_total - consumed
    remaining_ratio = _safe_ratio(remaining, budget_total)
    burn_rate = _safe_ratio(consumed, budget_total)
    targets = sorted({round(row.availability_target, 12) for row in rows})
    thresholds = sorted({round(row.latency_threshold_ms, 12) for row in rows})
    return {
        "window": {
            "start": format_timestamp(start),
            "end": format_timestamp(end),
            "duration_minutes": expected_minutes,
        },
        "sample_count": len(rows),
        "expected_samples": expected_minutes,
        "coverage_ratio": rounded(min(1.0, _safe_ratio(len(rows), expected_minutes))),
        "total_requests": total_requests,
        "success_requests": success_requests,
        "failed_requests": failed_requests,
        "availability_ratio": rounded(
            _safe_ratio(success_requests, total_requests)
        ),
        "availability_target": (
            rows[0].availability_target if rows and len(targets) == 1 else None
        ),
        "availability_target_min": targets[0] if targets else None,
        "availability_target_max": targets[-1] if targets else None,
        "latency_threshold_ms": (
            rows[0].latency_threshold_ms if rows and len(thresholds) == 1 else None
        ),
        "latency_threshold_min_ms": thresholds[0] if thresholds else None,
        "latency_threshold_max_ms": thresholds[-1] if thresholds else None,
        "budget_total": rounded(budget_total),
        "budget_consumed": rounded(consumed),
        "budget_consumed_availability": rounded(availability_consumed),
        "budget_consumed_latency_penalty": rounded(latency_penalty),
        "budget_remaining": rounded(remaining),
        "budget_remaining_ratio": rounded(remaining_ratio),
        "budget_remaining_pct": rounded(remaining_ratio * 100.0),
        "burn_rate": rounded(burn_rate),
        "availability_breach_minutes": availability_breaches,
        "latency_breach_minutes": latency_breaches,
        "availability_and_latency_breach_minutes": both_breaches,
        "latency_only_breach_minutes": latency_only_breaches,
        "availability_slo_met": availability_breaches == 0 and bool(rows),
        "latency_slo_met": latency_breaches == 0 and bool(rows),
        "slo_met": bool(rows)
        and availability_breaches == 0
        and latency_breaches == 0,
    }


def classify_burn(
    short_burn: float, long_burn: float, policy: dict[str, Any]
) -> str:
    burn_policy = policy["burn"]
    short_trigger = float(burn_policy["short_trigger"])
    long_trigger = float(burn_policy["long_trigger"])
    burst_ratio = float(burn_policy["burst_ratio"])
    short_hot = short_burn >= short_trigger
    long_hot = long_burn >= long_trigger
    if short_hot and short_burn >= long_burn * burst_ratio:
        return "burst"
    if long_hot:
        return "sustained"
    if short_hot:
        return "burst"
    return "normal"


def _dimension_high(
    name: str,
    value: float,
    thresholds: dict[str, Any],
    *,
    service: str | None = None,
    unit: str | None = None,
) -> dict[str, Any]:
    values: list[dict[str, Any]] = []
    mapped = "P4"
    for grade in ("P1", "P2", "P3"):
        threshold = float(thresholds[grade])
        met = float(value) >= threshold
        values.append(
            {
                "grade": grade,
                "operator": ">=",
                "threshold": rounded(threshold),
                "met": met,
            }
        )
        if met:
            mapped = grade
            break
    touched = next((item for item in values if item["grade"] == mapped), None)
    result: dict[str, Any] = {
        "dimension": name,
        "value": rounded(float(value)),
        "mapped_grade": mapped,
        "thresholds": values,
        "touched_threshold": touched,
    }
    if service:
        result["service"] = service
    if unit:
        result["unit"] = unit
    return result


def _dimension_low(
    name: str,
    value: float,
    thresholds: dict[str, Any],
    *,
    service: str | None = None,
    unit: str | None = None,
) -> dict[str, Any]:
    values: list[dict[str, Any]] = []
    mapped = "P4"
    for grade in ("P1", "P2", "P3"):
        threshold = float(thresholds[grade])
        met = float(value) <= threshold
        values.append(
            {
                "grade": grade,
                "operator": "<=",
                "threshold": rounded(threshold),
                "met": met,
            }
        )
        if met:
            mapped = grade
            break
    touched = next((item for item in values if item["grade"] == mapped), None)
    result: dict[str, Any] = {
        "dimension": name,
        "value": rounded(float(value)),
        "mapped_grade": mapped,
        "thresholds": values,
        "touched_threshold": touched,
    }
    if service:
        result["service"] = service
    if unit:
        result["unit"] = unit
    return result


def _dimension_change_risk(
    value: str, mappings: dict[str, str], *, service: str | None = None
) -> dict[str, Any]:
    records = [
        {
            "risk": risk,
            "grade": mappings[risk],
            "met": risk_at_least(value, risk),
        }
        for risk in ("critical", "high", "medium", "low", "none")
    ]
    result: dict[str, Any] = {
        "dimension": "change_risk",
        "value": value,
        "value_order": RISK_ORDER[value],
        "mapped_grade": mappings[value],
        "thresholds": records,
        "touched_threshold": {
            "risk": value,
            "grade": mappings[value],
            "met": True,
        }
        if value != "none"
        else None,
    }
    if service:
        result["service"] = service
    return result


def _worse(left: str, right: str) -> str:
    return left if SEVERITY_RANK[left] <= SEVERITY_RANK[right] else right


def _rule_applies(
    rule: dict[str, Any],
    *,
    affected_services: int,
    change_risk: str,
    budget_remaining_pct: float,
    burn_state: str,
    short_burn: float,
) -> bool:
    condition = rule.get("when", {})
    if "affected_services_gte" in condition:
        if affected_services < int(condition["affected_services_gte"]):
            return False
    if "change_risk_at_least" in condition:
        if not risk_at_least(change_risk, str(condition["change_risk_at_least"])):
            return False
    if "budget_remaining_pct_lte" in condition:
        if budget_remaining_pct > float(condition["budget_remaining_pct_lte"]):
            return False
    if "burn_state" in condition:
        if burn_state != condition["burn_state"]:
            return False
    if "short_burn_gte" in condition:
        if short_burn < float(condition["short_burn_gte"]):
            return False
    return True


def build_grading(
    *,
    budget_remaining_pct: float,
    budget_service: str | None,
    short_burn: float,
    short_service: str | None,
    long_burn: float,
    long_service: str | None,
    affected_services: int,
    change_risk: str,
    change_service: str | None,
    burn_state: str,
    policy: dict[str, Any],
) -> dict[str, Any]:
    severity = policy["severity"]
    dimensions = [
        _dimension_low(
            "error_budget_remaining_pct",
            budget_remaining_pct,
            severity["budget_remaining_pct"],
            service=budget_service,
            unit="percent",
        ),
        _dimension_high(
            "short_burn_rate",
            short_burn,
            severity["short_burn"],
            service=short_service,
            unit="multiple",
        ),
        _dimension_high(
            "long_burn_rate",
            long_burn,
            severity["long_burn"],
            service=long_service,
            unit="multiple",
        ),
        _dimension_high(
            "affected_services",
            float(affected_services),
            severity["affected_services"],
            unit="services",
        ),
        _dimension_change_risk(
            change_risk,
            severity["change_risk"],
            service=change_service,
        ),
    ]
    grade = "P4"
    threshold_hits: list[dict[str, Any]] = []
    for dimension in dimensions:
        grade = _worse(grade, dimension["mapped_grade"])
        touched = dimension["touched_threshold"]
        if touched:
            threshold_hits.append(
                {
                    "dimension": dimension["dimension"],
                    "grade": dimension["mapped_grade"],
                    "value": dimension["value"],
                    "threshold": touched.get("threshold", touched.get("risk")),
                    "operator": touched.get("operator", "risk_at_least"),
                    "service": dimension.get("service"),
                }
            )

    rules: list[dict[str, Any]] = []
    for rule in severity.get("combination_rules", []):
        applies = _rule_applies(
            rule,
            affected_services=affected_services,
            change_risk=change_risk,
            budget_remaining_pct=budget_remaining_pct,
            burn_state=burn_state,
            short_burn=short_burn,
        )
        minimum = str(rule["minimum_grade"]).upper()
        if applies:
            grade = _worse(grade, minimum)
        rules.append(
            {
                "id": rule["id"],
                "applied": applies,
                "minimum_grade": minimum,
                "when": rule.get("when", {}),
            }
        )
    return {
        "grade": grade,
        "dimensions": dimensions,
        "threshold_hits": threshold_hits,
        "combination_rules": rules,
        "burn_state": burn_state,
    }


def active_changes(
    changes: Iterable[dict[str, Any]],
    *,
    at: datetime,
    active_minutes: int,
    service: str | None = None,
) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for change in changes:
        changed_at = parse_timestamp(change["changed_at"], field="changed_at")
        age_minutes = (at - changed_at).total_seconds() / 60.0
        if age_minutes < 0 or age_minutes > active_minutes:
            continue
        services = change.get("services", [])
        if service is not None and service not in services and "*" not in services:
            continue
        result.append(change)
    return result


def highest_risk(changes: Iterable[dict[str, Any]]) -> str:
    risk = "none"
    for change in changes:
        candidate = change.get("risk", "none")
        if RISK_ORDER.get(candidate, 0) > RISK_ORDER[risk]:
            risk = candidate
    return risk


def _select_trigger(grading: dict[str, Any], policy: dict[str, Any]) -> dict[str, Any] | None:
    grade = grading["grade"]
    hits = grading.get("threshold_hits", [])
    candidates = [hit for hit in hits if hit["grade"] == grade]
    if not candidates:
        for rule in grading.get("combination_rules", []):
            if rule["applied"] and rule["minimum_grade"] == grade:
                return {
                    "metric": f"combination:{rule['id']}",
                    "value": rule["when"],
                    "threshold": grade,
                    "operator": "combination_rule",
                    "severity": grade,
                    "service": None,
                    "source": "combination",
                }
        candidates = hits
    priority = policy["severity"].get("trigger_priority", [])
    order = {name: index for index, name in enumerate(priority)}
    if candidates:
        hit = min(
            candidates,
            key=lambda item: (
                order.get(item["dimension"], len(order)),
                item["dimension"],
            ),
        )
        return {
            "metric": hit["dimension"],
            "value": hit["value"],
            "threshold": hit["threshold"],
            "operator": hit["operator"],
            "severity": hit["grade"],
            "service": hit.get("service"),
            "source": "dimension",
        }
    return None


def _service_affected_reasons(
    short_burn: float, long_burn: float, budget_pct: float, policy: dict[str, Any]
) -> list[str]:
    reasons: list[str] = []
    if short_burn >= float(policy["burn"]["short_trigger"]):
        reasons.append("SHORT_BURN_ABOVE_TRIGGER")
    if long_burn >= float(policy["burn"]["long_trigger"]):
        reasons.append("LONG_BURN_ABOVE_TRIGGER")
    if budget_pct <= float(policy["severity"]["budget_remaining_pct"]["P3"]):
        reasons.append("ERROR_BUDGET_BELOW_P3_THRESHOLD")
    return reasons


def _service_actual_grade(handover: dict[str, Any], service: str) -> str:
    grades = [
        incident["severity"]
        for incident in handover.get("open_incidents", [])
        if service in incident.get("services", [])
    ]
    if not grades:
        return "P4"
    return min(grades, key=lambda grade: SEVERITY_RANK[grade])


def evaluate_silences(
    handover: dict[str, Any],
    *,
    at: datetime,
    grade: str,
    affected_services: list[str],
    catalog_services: Sequence[str],
    policy: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
    silence_policy = policy["silence"]
    required_minutes = int(silence_policy["min_remaining_minutes"][grade])
    affected_set = set(affected_services)
    records: list[dict[str, Any]] = []
    for silence in handover.get("silences", []):
        starts_at = parse_timestamp(silence["starts_at"], field="silence.starts_at")
        ends_at = parse_timestamp(silence["ends_at"], field="silence.ends_at")
        services = list(silence.get("services", []))
        wildcard = "*" in services
        covered = set(catalog_services) if wildcard else set(services)
        remaining = (ends_at - at).total_seconds() / 60.0
        active = starts_at <= at < ends_at
        relevant = wildcard or bool(covered & affected_set) or not affected_set
        scope_matches = wildcard or affected_set.issubset(covered)
        severity_matches = grade in silence.get("severity_scope", [])
        blocking: list[str] = []
        if not active:
            blocking.append(
                "SILENCE_EXPIRED" if ends_at <= at else "SILENCE_NOT_STARTED"
            )
        if not scope_matches:
            blocking.append("SILENCE_SCOPE_MISMATCH")
        if not severity_matches:
            blocking.append("SILENCE_SEVERITY_MISMATCH")
        if remaining < required_minutes:
            blocking.append("SILENCE_REMAINING_TOO_SHORT")

        risk_reasons: list[str] = []
        unrelated = sorted(covered - affected_set)
        broad_count = int(silence_policy["overbroad_service_count"])
        if wildcard:
            risk_reasons.append("SILENCE_WILDCARD_SCOPE")
        elif unrelated and len(covered) >= broad_count:
            risk_reasons.append("SILENCE_SCOPE_TOO_BROAD")

        if blocking:
            status = "ineffective"
            recommendation = "replace_or_remove"
        elif risk_reasons:
            status = "risk"
            recommendation = "review"
        else:
            status = "effective"
            recommendation = "keep"
        records.append(
            {
                "silence_id": silence["silence_id"],
                "services": services,
                "severity_scope": silence.get("severity_scope", []),
                "starts_at": silence["starts_at"],
                "ends_at": silence["ends_at"],
                "remaining_minutes": rounded(remaining),
                "required_minutes": required_minutes,
                "active": active,
                "relevant": relevant,
                "covers_services": scope_matches,
                "covers_severity": severity_matches,
                "covers_current_incident": not blocking,
                "status": status,
                "recommendation": recommendation,
                "reasons": blocking + risk_reasons,
                "blocking_reasons": blocking,
                "risk_reasons": risk_reasons,
            }
        )

    relevant_records = [record for record in records if record["relevant"]]
    effective = [record for record in relevant_records if record["status"] == "effective"]
    risky = [record for record in relevant_records if record["status"] == "risk"]
    ineffective = [
        record for record in relevant_records if record["status"] == "ineffective"
    ]
    if risky:
        decision = "review"
    elif effective:
        decision = "keep"
    elif ineffective:
        decision = "replace_or_remove"
    else:
        decision = "no_silence"
    reasons = sorted(
        {reason for record in records for reason in record["reasons"]}
    )
    summary = {
        "decision": decision,
        "suppression_effective": bool(effective or risky),
        "keep_existing_silences": decision == "keep",
        "review_required": bool(risky),
        "current_grade": grade,
        "affected_services": sorted(affected_services),
        "required_minutes": required_minutes,
        "silence_count": len(records),
        "relevant_silence_count": len(relevant_records),
        "effective_count": len(effective),
        "risk_count": len(risky),
        "invalid_count": sum(
            1 for record in records if record["status"] == "ineffective"
        ),
        "relevant_invalid_count": len(ineffective),
        "reasons": reasons,
        "silences": records,
    }

    service_states: dict[str, dict[str, Any]] = {}
    for service in catalog_services:
        applicable = [
            record
            for record in records
            if "*" in record["services"] or service in record["services"]
        ]
        if not applicable:
            state = "none"
        elif service in affected_set:
            if any(record["status"] == "risk" for record in applicable):
                state = "risk"
            elif any(record["status"] == "effective" for record in applicable):
                state = "effective"
            else:
                state = "ineffective"
        elif any(record["status"] == "risk" for record in applicable):
            state = "risk"
        else:
            state = "not_required"
        service_states[service] = {
            "state": state,
            "silence_ids": [record["silence_id"] for record in applicable],
            "invalid_count": sum(
                1 for record in applicable if record["status"] == "ineffective"
            ),
            "reasons": sorted(
                {reason for record in applicable for reason in record["reasons"]}
            ),
        }
    return summary, service_states


def build_snapshot(
    handover: dict[str, Any],
    samples: SampleSet,
    policy: dict[str, Any],
) -> dict[str, Any]:
    at = handover_datetime(handover)
    reference = samples.latest_common_reference(at)
    if reference is None:
        raise HandoverError(
            "INSUFFICIENT_SAMPLES",
            "No complete SLI sample exists at or before the handover time.",
            http_status=422,
            details={"handover_at": format_timestamp(at)},
        )
    short_minutes = int(policy["windows"]["short_minutes"])
    long_minutes = int(policy["windows"]["long_minutes"])
    short_start = reference - timedelta(minutes=short_minutes - 1)
    long_start = reference - timedelta(minutes=long_minutes - 1)

    raw_services: dict[str, dict[str, Any]] = {}
    for service in samples.services:
        short_metrics = calculate_window(
            samples.window(service, short_start, reference), short_start, reference
        )
        long_metrics = calculate_window(
            samples.window(service, long_start, reference), long_start, reference
        )
        if short_metrics["sample_count"] == 0 and long_metrics["sample_count"] == 0:
            continue
        short_burn = float(short_metrics["burn_rate"] or 0.0)
        long_burn = float(long_metrics["burn_rate"] or 0.0)
        budget_pct = float(long_metrics["budget_remaining_pct"] or 0.0)
        state = classify_burn(short_burn, long_burn, policy)
        reasons = _service_affected_reasons(
            short_burn, long_burn, budget_pct, policy
        )
        raw_services[service] = {
            "short_metrics": short_metrics,
            "long_metrics": long_metrics,
            "short_burn": short_burn,
            "long_burn": long_burn,
            "budget_pct": budget_pct,
            "burn_state": state,
            "affected": bool(reasons) or state != "normal",
            "affected_reasons": reasons,
            "slo": infer_slo(samples.samples, service),
        }
    if not raw_services:
        raise HandoverError(
            "INSUFFICIENT_SAMPLES",
            "No service has samples in the decision windows.",
            http_status=422,
            details={"handover_at": format_timestamp(at)},
        )

    affected_services = sorted(
        service for service, record in raw_services.items() if record["affected"]
    )
    active_minutes = int(policy["silence"]["change_risk_active_minutes"])
    global_active_changes = active_changes(
        handover.get("recent_changes", []),
        at=at,
        active_minutes=active_minutes,
    )
    global_risk = highest_risk(global_active_changes)
    global_risk_service = None
    if global_active_changes:
        riskiest = sorted(
            global_active_changes,
            key=lambda item: (-RISK_ORDER.get(item.get("risk", "none"), 0), item["change_id"]),
        )[0]
        global_risk_service = (
            riskiest.get("services", [None])[0] if riskiest.get("services") else None
        )

    worst_budget_service = min(
        raw_services,
        key=lambda name: (raw_services[name]["budget_pct"], name),
    )
    worst_short_service = max(
        raw_services,
        key=lambda name: (raw_services[name]["short_burn"], name),
    )
    worst_long_service = max(
        raw_services,
        key=lambda name: (raw_services[name]["long_burn"], name),
    )
    global_short = raw_services[worst_short_service]["short_burn"]
    global_long = raw_services[worst_long_service]["long_burn"]
    global_state = classify_burn(global_short, global_long, policy)
    global_grading = build_grading(
        budget_remaining_pct=raw_services[worst_budget_service]["budget_pct"],
        budget_service=worst_budget_service,
        short_burn=global_short,
        short_service=worst_short_service,
        long_burn=global_long,
        long_service=worst_long_service,
        affected_services=len(affected_services),
        change_risk=global_risk,
        change_service=global_risk_service,
        burn_state=global_state,
        policy=policy,
    )
    overall_grade = global_grading["grade"]
    silence_summary, service_silence_states = evaluate_silences(
        handover,
        at=at,
        grade=overall_grade,
        affected_services=affected_services,
        catalog_services=samples.services,
        policy=policy,
    )

    services: dict[str, dict[str, Any]] = {}
    for service in sorted(raw_services):
        record = raw_services[service]
        service_changes = active_changes(
            handover.get("recent_changes", []),
            at=at,
            active_minutes=active_minutes,
            service=service,
        )
        service_risk = highest_risk(service_changes)
        service_affected_count = (
            len(affected_services) if record["affected"] else 0
        )
        service_grading = build_grading(
            budget_remaining_pct=record["budget_pct"],
            budget_service=service,
            short_burn=record["short_burn"],
            short_service=service,
            long_burn=record["long_burn"],
            long_service=service,
            affected_services=service_affected_count,
            change_risk=service_risk,
            change_service=service if service_risk != "none" else None,
            burn_state=record["burn_state"],
            policy=policy,
        )
        silence_state = service_silence_states[service]
        long_metrics = record["long_metrics"]
        short_metrics = record["short_metrics"]
        short_to_long_ratio = (
            _safe_ratio(record["short_burn"], record["long_burn"])
            if record["long_burn"] > 0
            else None
        )
        services[service] = {
            "service": service,
            "slo": record["slo"],
            "reference_time": format_timestamp(reference),
            "short_window": short_metrics,
            "long_window": long_metrics,
            "budget_remaining": {
                "long_window": {
                    "requests": long_metrics["budget_remaining"],
                    "ratio": long_metrics["budget_remaining_ratio"],
                    "pct": long_metrics["budget_remaining_pct"],
                },
                "short_window": {
                    "requests": short_metrics["budget_remaining"],
                    "ratio": short_metrics["budget_remaining_ratio"],
                    "pct": short_metrics["budget_remaining_pct"],
                },
            },
            "error_budget_remaining_requests": long_metrics["budget_remaining"],
            "error_budget_remaining_ratio": long_metrics["budget_remaining_ratio"],
            "error_budget_remaining_pct": long_metrics["budget_remaining_pct"],
            "budget_remaining_requests": long_metrics["budget_remaining"],
            "budget_remaining_ratio": long_metrics["budget_remaining_ratio"],
            "budget_remaining_pct": long_metrics["budget_remaining_pct"],
            "burn_rate_short": rounded(record["short_burn"]),
            "short_burn_rate": rounded(record["short_burn"]),
            "burn_rate_long": rounded(record["long_burn"]),
            "long_burn_rate": rounded(record["long_burn"]),
            "short_to_long_ratio": rounded(
                short_to_long_ratio
            ),
            "burn_state": record["burn_state"],
            "burn_state_basis": {
                "short_burn_rate": rounded(record["short_burn"]),
                "long_burn_rate": rounded(record["long_burn"]),
                "short_to_long_ratio": rounded(short_to_long_ratio),
                "short_trigger": rounded(float(policy["burn"]["short_trigger"])),
                "long_trigger": rounded(float(policy["burn"]["long_trigger"])),
                "burst_ratio": rounded(float(policy["burn"]["burst_ratio"])),
                "classification": record["burn_state"],
            },
            "affected": record["affected"],
            "affected_reasons": record["affected_reasons"],
            "current_grade": service_grading["grade"],
            "current_severity": service_grading["grade"],
            "actual_grade": _service_actual_grade(handover, service),
            "grading": service_grading,
            "silence_state": silence_state["state"],
            "silence_status": silence_state["state"],
            "silence_ids": silence_state["silence_ids"],
            "silence_invalid_count": silence_state["invalid_count"],
            "invalid_silence_count": silence_state["invalid_count"],
            "silence_reasons": silence_state["reasons"],
        }

    summary = {
        "overall_grade": overall_grade,
        "actual_grade": handover.get("actual_alert_level", "P4"),
        "affected_services": affected_services,
        "affected_service_count": len(affected_services),
        "burn_state": global_state,
        "burn_state_basis": {
            "short_burn_rate": rounded(global_short),
            "long_burn_rate": rounded(global_long),
            "short_to_long_ratio": rounded(
                _safe_ratio(global_short, global_long) if global_long > 0 else None
            ),
            "short_trigger": rounded(float(policy["burn"]["short_trigger"])),
            "long_trigger": rounded(float(policy["burn"]["long_trigger"])),
            "burst_ratio": rounded(float(policy["burn"]["burst_ratio"])),
            "classification": global_state,
        },
        "short_burn_rate": rounded(global_short),
        "long_burn_rate": rounded(global_long),
        "short_to_long_ratio": rounded(
            _safe_ratio(global_short, global_long) if global_long > 0 else None
        ),
        "minimum_budget_remaining_pct": rounded(
            raw_services[worst_budget_service]["budget_pct"]
        ),
        "change_risk": global_risk,
        "silence_decision": silence_summary["decision"],
        "suppression_effective": silence_summary["suppression_effective"],
        "silence_invalid_count": silence_summary["invalid_count"],
    }
    return {
        "snapshot_schema_version": "1.0",
        "handover_id": handover["handover_id"],
        "handover_at": format_timestamp(at),
        "from_operator": handover["from_operator"],
        "to_operator": handover["to_operator"],
        "reference_time": format_timestamp(reference),
        "window_semantics": {
            "short_window": {
                "lookback_minutes": short_minutes,
                "inclusive_end": True,
                "purpose": "Detect a sudden deterioration and report burst state.",
            },
            "long_window": {
                "lookback_minutes": long_minutes,
                "inclusive_end": True,
                "purpose": "Determine whether deterioration is sustained.",
            },
            "burn_state": {
                "burst": "short burn is at or above its trigger and at least the burst_ratio multiple of long burn",
                "sustained": "long burn is at or above its trigger without a disproportionate short-window spike",
                "normal": "neither trigger is met",
            },
            "budget": {
                "total": "sum(total_requests * (1 - availability_target))",
                "consumed": "failed requests plus successful requests in P99-breached minutes, unioned per minute",
                "remaining": "total - consumed",
            },
            "burn_rate": "consumed / total",
        },
        "policy_version": policy["policy_version"],
        "policy": policy,
        "handover": handover,
        "decision": {
            "summary": summary,
            "grading": global_grading,
            "trigger": _select_trigger(global_grading, policy),
            "silence": silence_summary,
        },
        "services": services,
    }
