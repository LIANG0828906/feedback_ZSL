"""Input parsing for deterministic SLI samples and handover forms."""

from __future__ import annotations

import bisect
import csv
import json
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable

from .errors import HandoverError
from .policy import SEVERITIES, SEVERITY_RANK


UTC = timezone.utc


def parse_timestamp(value: Any, *, field: str = "timestamp") -> datetime:
    if isinstance(value, datetime):
        parsed = value
    elif isinstance(value, str) and value.strip():
        text = value.strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        try:
            parsed = datetime.fromisoformat(text)
        except ValueError as exc:
            raise HandoverError(
                "INVALID_TIMESTAMP",
                f"The {field} value is not a valid ISO-8601 timestamp.",
                details={"field": field},
            ) from exc
    else:
        raise HandoverError(
            "INVALID_TIMESTAMP",
            f"The {field} value is required.",
            details={"field": field},
        )
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.astimezone(UTC)


def format_timestamp(value: datetime) -> str:
    text = value.astimezone(UTC).isoformat(timespec="microseconds")
    if text.endswith("+00:00"):
        text = text[:-6] + "Z"
    return text.replace(".000000Z", "Z")


def _number(value: Any, *, field: str, integer: bool = False) -> int | float:
    if isinstance(value, bool) or value is None or str(value).strip() == "":
        raise HandoverError(
            "INVALID_SAMPLE",
            f"The {field} value is required.",
            details={"field": field},
        )
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise HandoverError(
            "INVALID_SAMPLE",
            f"The {field} value must be numeric.",
            details={"field": field},
        ) from exc
    if integer:
        if not number.is_integer():
            raise HandoverError(
                "INVALID_SAMPLE",
                f"The {field} value must be an integer.",
                details={"field": field},
            )
        return int(number)
    return number


def _pick(row: dict[str, Any], *names: str) -> Any:
    for name in names:
        if name in row and row[name] not in (None, ""):
            return row[name]
    return None


@dataclass(frozen=True)
class Sample:
    timestamp: datetime
    service: str
    total_requests: int
    success_requests: int
    p99_latency_ms: float
    availability_target: float
    latency_threshold_ms: float


def sample_from_mapping(row: dict[str, Any], *, source: str = "sample") -> Sample:
    service = _pick(row, "service", "service_name")
    if not isinstance(service, str) or not service.strip():
        raise HandoverError(
            "INVALID_SAMPLE",
            "Every SLI sample must have a service name.",
            details={"source": source},
        )
    timestamp = parse_timestamp(
        _pick(row, "timestamp", "time", "minute", "recorded_at"),
        field="timestamp",
    )
    total = _number(
        _pick(row, "total_requests", "requests", "total"),
        field="total_requests",
        integer=True,
    )
    success = _number(
        _pick(row, "success_requests", "successes", "success"),
        field="success_requests",
        integer=True,
    )
    p99 = _number(
        _pick(row, "p99_latency_ms", "p99_ms", "p99", "latency_p99_ms"),
        field="p99_latency_ms",
    )
    target = _number(
        _pick(row, "availability_target", "slo_target", "slo_availability"),
        field="availability_target",
    )
    threshold = _number(
        _pick(row, "latency_threshold_ms", "latency_slo_ms", "latency_target_ms"),
        field="latency_threshold_ms",
    )
    assert isinstance(total, int) and isinstance(success, int)
    if total < 0 or success < 0 or success > total:
        raise HandoverError(
            "INVALID_SAMPLE",
            "Request counts must satisfy 0 <= success_requests <= total_requests.",
            details={"service": service.strip(), "timestamp": format_timestamp(timestamp)},
        )
    if p99 < 0 or threshold <= 0:
        raise HandoverError(
            "INVALID_SAMPLE",
            "Latency values must be non-negative and the threshold must be positive.",
            details={"service": service.strip(), "timestamp": format_timestamp(timestamp)},
        )
    if target > 1:
        target = target / 100.0
    if not 0 < target <= 1:
        raise HandoverError(
            "INVALID_SAMPLE",
            "availability_target must be in (0, 1] or (0, 100].",
            details={"service": service.strip(), "timestamp": format_timestamp(timestamp)},
        )
    return Sample(
        timestamp=timestamp,
        service=service.strip(),
        total_requests=total,
        success_requests=success,
        p99_latency_ms=float(p99),
        availability_target=float(target),
        latency_threshold_ms=float(threshold),
    )


def _read_csv(path: Path) -> list[dict[str, Any]]:
    try:
        with path.open("r", encoding="utf-8-sig", newline="") as handle:
            return list(csv.DictReader(handle))
    except OSError as exc:
        raise HandoverError(
            "SAMPLE_READ_ERROR",
            "Unable to read an SLI sample file.",
            details={"file": path.name},
        ) from exc


def _read_json_file(path: Path) -> list[dict[str, Any]]:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise HandoverError(
            "SAMPLE_READ_ERROR",
            "Unable to parse an SLI sample file.",
            details={"file": path.name},
        ) from exc
    if isinstance(raw, list):
        rows = raw
    elif isinstance(raw, dict) and isinstance(raw.get("samples"), list):
        rows = raw["samples"]
    else:
        return []
    if not all(isinstance(row, dict) for row in rows):
        raise HandoverError(
            "INVALID_SAMPLE",
            "Every SLI sample must be a JSON object.",
            details={"file": path.name},
        )
    return rows


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    try:
        with path.open("r", encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, start=1):
                if not line.strip():
                    continue
                try:
                    row = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise HandoverError(
                        "INVALID_SAMPLE",
                        "An SLI JSONL record is not valid JSON.",
                        details={"file": path.name, "line": line_number},
                    ) from exc
                if not isinstance(row, dict):
                    raise HandoverError(
                        "INVALID_SAMPLE",
                        "Every SLI JSONL record must be an object.",
                        details={"file": path.name, "line": line_number},
                    )
                rows.append(row)
    except OSError as exc:
        raise HandoverError(
            "SAMPLE_READ_ERROR",
            "Unable to read an SLI JSONL file.",
            details={"file": path.name},
        ) from exc
    return rows


def discover_sample_files(path: str | Path) -> list[Path]:
    candidate = Path(path)
    if candidate.is_file():
        return [candidate]
    if not candidate.is_dir():
        raise HandoverError(
            "SAMPLES_NOT_FOUND",
            "The SLI sample path does not exist.",
            details={"path": candidate.name},
        )
    preferred = candidate / "samples.csv"
    if preferred.is_file():
        return [preferred]
    files = sorted(
        item
        for item in candidate.iterdir()
        if item.is_file() and item.suffix.lower() in {".csv", ".json", ".jsonl"}
    )
    if not files:
        raise HandoverError(
            "SAMPLES_NOT_FOUND",
            "No CSV, JSON, or JSONL SLI sample files were found.",
            details={"directory": candidate.name},
        )
    return files


def load_samples(path: str | Path) -> list[Sample]:
    rows: list[dict[str, Any]] = []
    for file_path in discover_sample_files(path):
        suffix = file_path.suffix.lower()
        if suffix == ".csv":
            rows.extend(_read_csv(file_path))
        elif suffix == ".jsonl":
            rows.extend(_read_jsonl(file_path))
        else:
            rows.extend(_read_json_file(file_path))
    if not rows:
        raise HandoverError("SAMPLES_NOT_FOUND", "The SLI sample set is empty.")
    samples = [sample_from_mapping(row) for row in rows]
    return samples


class SampleSet:
    """Sorted, duplicate-checked samples with per-service lookup."""

    def __init__(self, samples: Iterable[Sample]) -> None:
        ordered = sorted(samples, key=lambda row: (row.service, row.timestamp))
        seen: set[tuple[str, datetime]] = set()
        grouped: dict[str, list[Sample]] = {}
        for row in ordered:
            key = (row.service, row.timestamp)
            if key in seen:
                raise HandoverError(
                    "DUPLICATE_SAMPLE",
                    "Two samples have the same service and timestamp.",
                    details={
                        "service": row.service,
                        "timestamp": format_timestamp(row.timestamp),
                    },
                )
            seen.add(key)
            grouped.setdefault(row.service, []).append(row)
        if not grouped:
            raise HandoverError("SAMPLES_NOT_FOUND", "The SLI sample set is empty.")
        self._samples = tuple(ordered)
        self._grouped = {name: tuple(rows) for name, rows in grouped.items()}
        self._times = {
            name: tuple(row.timestamp for row in rows)
            for name, rows in self._grouped.items()
        }

    @property
    def services(self) -> tuple[str, ...]:
        return tuple(sorted(self._grouped))

    @property
    def samples(self) -> tuple[Sample, ...]:
        return self._samples

    def for_service(self, service: str) -> tuple[Sample, ...]:
        try:
            return self._grouped[service]
        except KeyError as exc:
            raise HandoverError(
                "SERVICE_NOT_FOUND",
                "The requested service is not present in the sample set.",
                http_status=404,
                details={"service": service},
            ) from exc

    def window(self, service: str, start: datetime, end: datetime) -> list[Sample]:
        rows = self.for_service(service)
        times = self._times[service]
        left = bisect.bisect_left(times, start)
        right = bisect.bisect_right(times, end)
        return list(rows[left:right])

    def latest_at_or_before(self, service: str, moment: datetime) -> Sample | None:
        rows = self.for_service(service)
        index = bisect.bisect_right(self._times[service], moment) - 1
        if index < 0:
            return None
        return rows[index]

    def latest_common_reference(self, moment: datetime) -> datetime | None:
        latest: list[datetime] = []
        for service in self.services:
            row = self.latest_at_or_before(service, moment)
            if row is None:
                return None
            latest.append(row.timestamp)
        return min(latest)

    def timestamps_after(
        self, moment: datetime, *, until: datetime | None = None
    ) -> list[datetime]:
        values = {
            row.timestamp
            for row in self._samples
            if row.timestamp > moment and (until is None or row.timestamp <= until)
        }
        return sorted(values)

    def first_timestamp_after(
        self, moment: datetime, *, service: str | None = None
    ) -> datetime | None:
        if service is not None:
            rows = self.for_service(service)
            times = self._times[service]
            index = bisect.bisect_right(times, moment)
            return times[index] if index < len(times) else None
        for row in self._samples:
            if row.timestamp > moment:
                return row.timestamp
        return None

    def last_common_timestamp(self) -> datetime:
        return min(rows[-1].timestamp for rows in self._grouped.values())


def _string(value: Any, *, field: str, required: bool = True) -> str:
    if value is None and not required:
        return ""
    if not isinstance(value, str) or not value.strip():
        raise HandoverError(
            "INVALID_HANDOVER",
            f"The handover field {field} is required.",
            details={"field": field},
        )
    return value.strip()


def _service_list(value: Any, *, field: str) -> list[str]:
    if isinstance(value, str):
        values = [value]
    elif isinstance(value, list):
        values = value
    else:
        values = []
    result: list[str] = []
    for item in values:
        if not isinstance(item, str) or not item.strip():
            raise HandoverError(
                "INVALID_HANDOVER",
                f"{field} must contain non-empty service names.",
                details={"field": field},
            )
        name = item.strip()
        if name not in result:
            result.append(name)
    return result


def _normalize_severity(value: Any, *, field: str = "severity") -> str:
    if not isinstance(value, str):
        raise HandoverError(
            "INVALID_HANDOVER",
            f"{field} must be one of P1, P2, P3, P4.",
            details={"field": field},
        )
    normalized = value.strip().upper()
    if normalized not in SEVERITIES:
        raise HandoverError(
            "INVALID_HANDOVER",
            f"{field} must be one of P1, P2, P3, P4.",
            details={"field": field},
        )
    return normalized


def normalize_handover(raw: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise HandoverError("INVALID_HANDOVER", "The handover must be a JSON object.")
    handover_id = _string(_pick(raw, "handover_id", "id"), field="handover_id")
    handover_at = parse_timestamp(
        _pick(raw, "handover_at", "handed_over_at", "timestamp"),
        field="handover_at",
    )
    from_operator = _string(
        _pick(raw, "from_operator", "outgoing_operator", "from"),
        field="from_operator",
    )
    to_operator = _string(
        _pick(raw, "to_operator", "incoming_operator", "to"),
        field="to_operator",
    )

    incident_values = _pick(raw, "open_incidents", "incidents", "current_incidents") or []
    if not isinstance(incident_values, list):
        raise HandoverError(
            "INVALID_HANDOVER", "open_incidents must be a list."
        )
    incidents: list[dict[str, Any]] = []
    for index, item in enumerate(incident_values):
        if not isinstance(item, dict):
            raise HandoverError(
                "INVALID_HANDOVER",
                "Every open incident must be an object.",
                details={"index": index},
            )
        services = _service_list(
            item.get("services", item.get("service")), field="incident.services"
        )
        incident: dict[str, Any] = {
            "incident_id": _string(
                _pick(item, "incident_id", "id"),
                field=f"open_incidents[{index}].incident_id",
            ),
            "severity": _normalize_severity(
                _pick(item, "severity", "alert_level"),
                field=f"open_incidents[{index}].severity",
            ),
            "services": services,
            "title": _string(
                item.get("title", item.get("summary", "")),
                field=f"open_incidents[{index}].title",
                required=False,
            ),
            "status": _string(
                item.get("status", "open"),
                field=f"open_incidents[{index}].status",
                required=False,
            ),
        }
        started_at = item.get("started_at")
        if started_at not in (None, ""):
            incident["started_at"] = format_timestamp(
                parse_timestamp(started_at, field=f"open_incidents[{index}].started_at")
            )
        incidents.append(incident)

    change_values: list[Any] = []
    if isinstance(raw.get("recent_changes"), list):
        change_values.extend(raw["recent_changes"])
    for incident in incident_values:
        nested = incident.get("recent_changes") if isinstance(incident, dict) else None
        if isinstance(nested, list):
            change_values.extend(nested)
    changes: list[dict[str, Any]] = []
    seen_changes: set[str] = set()
    for index, item in enumerate(change_values):
        if not isinstance(item, dict):
            raise HandoverError(
                "INVALID_HANDOVER",
                "Every recent change must be an object.",
                details={"index": index},
            )
        change_id = _string(
            _pick(item, "change_id", "id"),
            field=f"recent_changes[{index}].change_id",
        )
        if change_id in seen_changes:
            continue
        seen_changes.add(change_id)
        risk = _string(
            _pick(item, "risk", "risk_level"),
            field=f"recent_changes[{index}].risk",
        ).lower()
        if risk not in {"critical", "high", "medium", "low", "none"}:
            raise HandoverError(
                "INVALID_HANDOVER",
                "Change risk must be critical, high, medium, low, or none.",
                details={"change_id": change_id},
            )
        changes.append(
            {
                "change_id": change_id,
                "services": _service_list(
                    item.get("services", item.get("service")),
                    field=f"recent_changes[{index}].services",
                ),
                "risk": risk,
                "changed_at": format_timestamp(
                    parse_timestamp(
                        _pick(item, "changed_at", "at", "timestamp"),
                        field=f"recent_changes[{index}].changed_at",
                    )
                ),
                "description": _string(
                    item.get("description", ""),
                    field=f"recent_changes[{index}].description",
                    required=False,
                ),
            }
        )
    changes.sort(key=lambda item: (item["changed_at"], item["change_id"]))

    silence_values = _pick(raw, "silences", "temporary_silences", "mutes") or []
    if not isinstance(silence_values, list):
        raise HandoverError("INVALID_HANDOVER", "silences must be a list.")
    silences: list[dict[str, Any]] = []
    for index, item in enumerate(silence_values):
        if not isinstance(item, dict):
            raise HandoverError(
                "INVALID_HANDOVER",
                "Every silence must be an object.",
                details={"index": index},
            )
        raw_scope = item.get("severity_scope", item.get("severities"))
        if raw_scope in (None, "", "all", "ALL", "*"):
            severity_scope = list(SEVERITIES)
        elif isinstance(raw_scope, str):
            severity_scope = [_normalize_severity(raw_scope, field="silence.severity_scope")]
        elif isinstance(raw_scope, list):
            severity_scope = [
                _normalize_severity(value, field="silence.severity_scope")
                for value in raw_scope
            ]
        else:
            raise HandoverError(
                "INVALID_HANDOVER",
                "silence.severity_scope must be a string or a list.",
            )
        silence_id = _string(
            _pick(item, "silence_id", "id"),
            field=f"silences[{index}].silence_id",
            required=False,
        ) or f"SILENCE-{index + 1}"
        services = _service_list(
            item.get("services", item.get("service")), field="silence.services"
        )
        if not services:
            raise HandoverError(
                "INVALID_HANDOVER",
                "Every silence must declare at least one service.",
                details={"silence_id": silence_id},
            )
        starts_at = parse_timestamp(
            _pick(item, "starts_at", "start", "created_at"),
            field=f"silences[{index}].starts_at",
        )
        ends_at = parse_timestamp(
            _pick(item, "ends_at", "end", "expires_at"),
            field=f"silences[{index}].ends_at",
        )
        if ends_at <= starts_at:
            raise HandoverError(
                "INVALID_HANDOVER",
                "A silence must end after it starts.",
                details={"silence_id": silence_id},
            )
        silences.append(
            {
                "silence_id": silence_id,
                "services": services,
                "severity_scope": severity_scope,
                "starts_at": format_timestamp(starts_at),
                "ends_at": format_timestamp(ends_at),
                "reason": _string(
                    item.get("reason", ""),
                    field=f"silences[{index}].reason",
                    required=False,
                ),
                "created_by": _string(
                    _pick(item, "created_by", "operator"),
                    field=f"silences[{index}].created_by",
                    required=False,
                ),
            }
        )

    actual_override = raw.get("actual_alert_level")
    if actual_override:
        actual_grade = _normalize_severity(actual_override, field="actual_alert_level")
    elif incidents:
        actual_grade = min(
            (incident["severity"] for incident in incidents),
            key=lambda grade: SEVERITY_RANK[grade],
        )
    else:
        actual_grade = "P4"

    return {
        "handover_id": handover_id,
        "from_operator": from_operator,
        "to_operator": to_operator,
        "handover_at": format_timestamp(handover_at),
        "open_incidents": incidents,
        "recent_changes": changes,
        "silences": silences,
        "actual_alert_level": actual_grade,
        "notes": _string(
            raw.get("notes", ""), field="notes", required=False
        ),
    }


def load_handover(path: str | Path) -> dict[str, Any]:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise HandoverError(
            "HANDOVER_NOT_FOUND",
            "The handover file was not found.",
            details={"file": Path(path).name},
        ) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise HandoverError(
            "INVALID_HANDOVER",
            "The handover file is not valid JSON.",
            details={"file": Path(path).name},
        ) from exc
    if not isinstance(raw, dict):
        raise HandoverError("INVALID_HANDOVER", "The handover must be a JSON object.")
    return normalize_handover(raw)


def handover_datetime(handover: dict[str, Any]) -> datetime:
    return parse_timestamp(handover["handover_at"], field="handover_at")


def infer_slo(samples: Iterable[Sample], service: str) -> dict[str, Any]:
    rows = [row for row in samples if row.service == service]
    if not rows:
        raise HandoverError(
            "SERVICE_NOT_FOUND",
            "The service is not present in the sample set.",
            http_status=404,
            details={"service": service},
        )
    targets = {round(row.availability_target, 12) for row in rows}
    thresholds = {round(row.latency_threshold_ms, 12) for row in rows}
    return {
        "availability_target": rows[0].availability_target,
        "latency_threshold_ms": rows[0].latency_threshold_ms,
        "availability_target_constant": len(targets) == 1,
        "latency_threshold_constant": len(thresholds) == 1,
    }


def add_minutes(value: datetime, minutes: int) -> datetime:
    return value + timedelta(minutes=minutes)
