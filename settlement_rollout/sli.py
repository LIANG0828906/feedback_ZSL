from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable

from .config import PlanConfig, SLODefinition


@dataclass(frozen=True)
class Sample:
    instance_id: str
    sample_id: int
    status: int
    latency_ms: float
    version: str

    def as_dict(self) -> dict[str, Any]:
        return {
            "instance_id": self.instance_id,
            "sample_id": self.sample_id,
            "status": self.status,
            "latency_ms": round(float(self.latency_ms), 6),
            "version": self.version,
        }


def percentile_nearest_rank(values: Iterable[float], percentile: float) -> float:
    ordered = sorted(float(item) for item in values)
    if not ordered:
        return 0.0
    rank = max(1, math.ceil((percentile / 100.0) * len(ordered)))
    return ordered[min(rank, len(ordered)) - 1]


def _ratio(value: float) -> float:
    return round(float(value), 6)


def _budget_consumed(bad_ratio: float, allowed_bad_ratio: float) -> float:
    if allowed_bad_ratio <= 0.0:
        return 0.0 if bad_ratio <= 0.0 else 1.0
    return bad_ratio / allowed_bad_ratio


def evaluate_samples(
    config: PlanConfig,
    samples: list[Sample],
    switched_instances: list[str],
) -> dict[str, Any]:
    """Evaluate one batch from a deterministic first-N-per-instance sample set."""
    slo = config.slo
    quota = config.sampling.quota_per_instance
    selected: list[Sample] = []
    per_instance: dict[str, int] = {}
    for instance_id in switched_instances:
        instance_samples = sorted(
            (
                sample
                for sample in samples
                if sample.instance_id == instance_id
                and sample.version == config.target_version
            ),
            key=lambda sample: sample.sample_id,
        )[:quota]
        per_instance[instance_id] = len(instance_samples)
        selected.extend(instance_samples)

    total = len(selected)
    metrics: dict[str, Any] = {
        "sample_count": total,
        "sample_count_by_instance": dict(sorted(per_instance.items())),
        "availability_percent": 0.0,
        "p99_latency_ms": 0.0,
        "latency_bad_percent": 0.0,
        "availability_error_budget_consumed_ratio": 0.0,
        "latency_error_budget_consumed_ratio": 0.0,
        "error_budget_consumed_ratio": 0.0,
        "error_budget_remaining_ratio": 1.0,
        "switched_instance_count": len(switched_instances),
    }
    triggers: list[dict[str, Any]] = []

    if total < slo.minimum_samples:
        triggers.append(
            {
                "metric": "sample_count",
                "observed": total,
                "threshold": slo.minimum_samples,
                "comparator": ">=",
                "code": "INSUFFICIENT_SAMPLES",
            }
        )
    if total:
        good = sum(1 for sample in selected if sample.status < 500)
        availability = (good / total) * 100.0
        p99 = percentile_nearest_rank(
            (sample.latency_ms for sample in selected),
            slo.latency_percentile,
        )
        latency_bad = sum(
            1 for sample in selected if sample.latency_ms > slo.latency_target_ms
        )
        latency_bad_ratio = latency_bad / total
        latency_bad_percent = latency_bad_ratio * 100.0
        availability_bad_ratio = 1.0 - (good / total)
        availability_budget = _budget_consumed(
            availability_bad_ratio,
            1.0 - (slo.availability_target_percent / 100.0),
        )
        latency_budget = _budget_consumed(
            latency_bad_ratio,
            slo.latency_allowed_bad_percent / 100.0,
        )
        consumed = max(availability_budget, latency_budget)
        metrics.update(
            {
                "availability_percent": _ratio(availability),
                "p99_latency_ms": _ratio(p99),
                "latency_bad_percent": _ratio(latency_bad_percent),
                "availability_error_budget_consumed_ratio": _ratio(
                    availability_budget
                ),
                "latency_error_budget_consumed_ratio": _ratio(latency_budget),
                "error_budget_consumed_ratio": _ratio(consumed),
                "error_budget_remaining_ratio": _ratio(max(0.0, 1.0 - consumed)),
            }
        )
        if availability < slo.availability_target_percent:
            triggers.append(
                {
                    "metric": "availability_percent",
                    "observed": _ratio(availability),
                    "threshold": slo.availability_target_percent,
                    "comparator": ">=",
                    "code": "AVAILABILITY_BELOW_TARGET",
                }
            )
        if p99 > slo.latency_target_ms:
            triggers.append(
                {
                    "metric": "p99_latency_ms",
                    "observed": _ratio(p99),
                    "threshold": slo.latency_target_ms,
                    "comparator": "<=",
                    "code": "P99_ABOVE_TARGET",
                }
            )
        if consumed * 100.0 > slo.max_error_budget_consumed_percent:
            triggers.append(
                {
                    "metric": "error_budget_consumed_percent",
                    "observed": _ratio(consumed * 100.0),
                    "threshold": slo.max_error_budget_consumed_percent,
                    "comparator": "<=",
                    "code": "ERROR_BUDGET_EXCEEDED",
                }
            )

    thresholds = {
        "availability_target_percent": slo.availability_target_percent,
        "latency_percentile": slo.latency_percentile,
        "p99_latency_target_ms": slo.latency_target_ms,
        "latency_allowed_bad_percent": slo.latency_allowed_bad_percent,
        "minimum_samples": slo.minimum_samples,
        "max_error_budget_consumed_percent": (
            slo.max_error_budget_consumed_percent
        ),
    }
    verdict = "PASS" if not triggers else "FAIL"
    reason = "slo_met" if verdict == "PASS" else triggers[0]["code"].lower()
    return {
        "verdict": verdict,
        "reason": reason,
        "metrics": metrics,
        "thresholds": thresholds,
        "triggers": triggers,
        "samples": selected,
    }
