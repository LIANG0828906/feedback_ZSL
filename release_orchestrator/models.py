"""Core immutable data structures and deterministic serialization helpers."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from typing import Any, Iterable


def utc_now() -> float:
    return datetime.now(timezone.utc).timestamp()


def timestamp_to_iso(value: float | int | None) -> str | None:
    if value is None:
        return None
    return (
        datetime.fromtimestamp(float(value), tz=timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


def parse_timestamp(value: Any) -> float:
    """Parse Unix seconds, numeric strings, or ISO-8601 timestamps."""

    if isinstance(value, bool):
        raise ValueError("boolean is not a timestamp")
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            raise ValueError("empty timestamp")
        try:
            return float(text)
        except ValueError:
            pass
        normalized = text[:-1] + "+00:00" if text.endswith("Z") else text
        parsed = datetime.fromisoformat(normalized)
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=timezone.utc)
        return parsed.timestamp()
    raise ValueError(f"unsupported timestamp: {value!r}")


def stable_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        default=str,
    )


def digest_json(value: Any) -> str:
    return hashlib.sha256(stable_json(value).encode("utf-8")).hexdigest()


def rounded(value: float | None, digits: int = 8) -> float | None:
    if value is None:
        return None
    return round(float(value), digits)


def weighted_average(rows: Iterable[dict[str, Any]], key: str) -> float | None:
    numerator = 0.0
    denominator = 0.0
    seen = False
    for row in rows:
        value = row.get(key)
        if value is None:
            continue
        weight = float(row.get("request_count") or 1.0)
        if weight <= 0:
            weight = 1.0
        numerator += float(value) * weight
        denominator += weight
        seen = True
    if not seen or denominator <= 0:
        return None
    return numerator / denominator


@dataclass(frozen=True)
class Thresholds:
    batch_error_rate: float = 0.02
    batch_connection_reject_rate: float = 0.01
    instance_error_rate: float = 0.04
    instance_connection_reject_rate: float = 0.025
    minimum_samples_per_instance: int = 5

    def to_dict(self) -> dict[str, Any]:
        return {
            "batch": {
                "error_rate": self.batch_error_rate,
                "connection_reject_rate": self.batch_connection_reject_rate,
            },
            "instance": {
                "error_rate": self.instance_error_rate,
                "connection_reject_rate": self.instance_connection_reject_rate,
            },
            "minimum_samples_per_instance": self.minimum_samples_per_instance,
        }


@dataclass(frozen=True)
class BatchPlan:
    batch: int
    instances: tuple[str, ...]
    observation_seconds: int
    start_offset_seconds: float | None = None

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "batch": self.batch,
            "instances": list(self.instances),
            "observation_seconds": self.observation_seconds,
        }
        if self.start_offset_seconds is not None:
            payload["start_offset_seconds"] = self.start_offset_seconds
        return payload


@dataclass(frozen=True)
class ReleasePlan:
    release_id: str
    parameter_key: str
    target_version: str
    previous_stable_version: str
    batches: tuple[BatchPlan, ...]
    thresholds: Thresholds = field(default_factory=Thresholds)
    metrics_file: str | None = None
    version_reports_file: str | None = None
    plan_path: str | None = None
    plan_sha256: str | None = None
    raw: dict[str, Any] = field(default_factory=dict, compare=False)

    @property
    def all_instances(self) -> tuple[str, ...]:
        values = {instance for batch in self.batches for instance in batch.instances}
        return tuple(sorted(values))

    @property
    def batch_count(self) -> int:
        return len(self.batches)

    def batch(self, batch_no: int) -> BatchPlan | None:
        for batch in self.batches:
            if batch.batch == batch_no:
                return batch
        return None

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "release_id": self.release_id,
            "parameter_key": self.parameter_key,
            "target_version": self.target_version,
            "previous_stable_version": self.previous_stable_version,
            "thresholds": self.thresholds.to_dict(),
            "batches": [batch.to_dict() for batch in self.batches],
            "metrics_file": self.metrics_file,
            "version_reports_file": self.version_reports_file,
        }


@dataclass(frozen=True)
class Violation:
    scope: str
    instance_id: str | None
    metric: str
    value: float | None
    threshold: float
    sample_count: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "scope": self.scope,
            "instance_id": self.instance_id,
            "metric": self.metric,
            "value": rounded(self.value),
            "threshold": rounded(self.threshold),
            "sample_count": self.sample_count,
        }


@dataclass(frozen=True)
class Decision:
    release_id: str
    batch: int
    result: str
    reason_code: str | None
    aggregate: dict[str, Any]
    instances: tuple[dict[str, Any], ...]
    violations: tuple[Violation, ...]
    sample_count: int
    observed_from: float | None
    observed_to: float | None
    rollback_target_version: str
    observation_seconds: int
    minimum_samples: int
    conflicts: tuple[dict[str, Any], ...] = ()

    @property
    def passed(self) -> bool:
        return self.result == "passed"

    @property
    def threshold_instances(self) -> tuple[str, ...]:
        return tuple(
            sorted(
                {
                    violation.instance_id
                    for violation in self.violations
                    if violation.scope == "instance" and violation.instance_id
                }
            )
        )

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "schema_version": 1,
            "release_id": self.release_id,
            "batch": self.batch,
            "observation_seconds": self.observation_seconds,
            "minimum_samples_per_instance": self.minimum_samples,
            "sample_count": self.sample_count,
            "observed_from": timestamp_to_iso(self.observed_from),
            "observed_to": timestamp_to_iso(self.observed_to),
            "aggregate": self.aggregate,
            "instances": list(self.instances),
            "violations": [item.to_dict() for item in self.violations],
            "result": self.result,
            "reason_code": self.reason_code,
            "rollback_target_version": self.rollback_target_version,
        }
        if self.conflicts:
            payload["conflicts"] = list(self.conflicts)
        return payload


def threshold_instance_ids(decision: dict[str, Any] | None) -> tuple[str, ...]:
    if not decision:
        return ()
    result = set()
    for violation in decision.get("violations", []):
        if violation.get("scope") == "instance" and violation.get("instance_id"):
            result.add(str(violation["instance_id"]))
    return tuple(sorted(result))


def decision_violations(decision: dict[str, Any] | None) -> list[dict[str, Any]]:
    if not decision:
        return []
    return sorted(
        list(decision.get("violations", [])),
        key=lambda item: (
            str(item.get("instance_id") or ""),
            str(item.get("metric") or ""),
            str(item.get("scope") or ""),
        ),
    )
