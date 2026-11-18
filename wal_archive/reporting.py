"""Deterministic archive reports and Prometheus exposition."""

from __future__ import annotations

from typing import Any, Iterable


def _field(row: Any, name: str) -> Any:
    if isinstance(row, dict):
        return row[name]
    if hasattr(row, name):
        return getattr(row, name)
    return row[name]


def _sort_key(instance: str, timeline: int) -> tuple[str, int]:
    return instance, timeline


class Aggregate:
    def __init__(self) -> None:
        self.segment_count = 0
        self.usable_segment_count = 0
        self.usable_bytes = 0
        self.writing_count = 0
        self.truncated_count = 0
        self.checksum_failed_count = 0
        self.conflict_version_count = 0
        self.pages_total = 0
        self.pages_checked = 0
        self.pages_ok = 0
        self.pages_bad = 0
        self.recovery_count = 0
        self.observed_segnos: set[int] = set()
        self.complete_segnos: set[int] = set()
        self.usable_seen: set[int] = set()

    def as_dict(self) -> dict[str, Any]:
        gaps: list[int] = []
        if self.observed_segnos:
            gaps = sorted(
                set(range(min(self.observed_segnos), max(self.observed_segnos) + 1))
                - self.observed_segnos
            )
        pass_rate = (
            float(self.pages_ok) / float(self.pages_checked)
            if self.pages_checked
            else 0.0
        )
        return {
            "checksum_failed_segments": self.checksum_failed_count,
            "conflict_versions": self.conflict_version_count,
            "gap_segment_numbers": gaps,
            "usable_bytes": self.usable_bytes,
            "usable_segment_count": self.usable_segment_count,
            "segment_count": self.segment_count,
            "truncated_segments": self.truncated_count,
            "writing_segments": self.writing_count,
            "verification": {
                "checksum_pass_rate": round(pass_rate, 9),
                "pages_bad": self.pages_bad,
                "pages_checked": self.pages_checked,
                "pages_ok": self.pages_ok,
                "pages_total": self.pages_total,
            },
        }

    def metrics_dict(self) -> dict[str, int]:
        return {
            "checksum_failed_segments": self.checksum_failed_count,
            "committed_recovery_points": self.recovery_count,
            "conflict_versions": self.conflict_version_count,
            "segments": self.segment_count,
            "truncated_segments": self.truncated_count,
            "usable_bytes": self.usable_bytes,
            "writing_segments": self.writing_count,
        }


def aggregate_state(
    versions: Iterable[Any],
    in_progress: Iterable[Any],
    recoveries: Iterable[Any],
) -> tuple[dict[tuple[str, int], Aggregate], set[str], set[tuple[str, int]]]:
    aggregates: dict[tuple[str, int], Aggregate] = {}
    instances: set[str] = set()
    keys: set[tuple[str, int]] = set()

    def get(instance: str, timeline: int) -> Aggregate:
        key = (instance, timeline)
        keys.add(key)
        if key not in aggregates:
            aggregates[key] = Aggregate()
        return aggregates[key]

    for row in versions:
        instance = _field(row, "instance")
        timeline = int(_field(row, "timeline"))
        segno = int(_field(row, "segno"))
        classification = _field(row, "classification")
        conflict = int(_field(row, "conflict"))
        size = int(_field(row, "size"))
        pages_total = int(_field(row, "pages_total"))
        pages_checked = int(_field(row, "pages_checked"))
        pages_ok = int(_field(row, "pages_ok"))
        pages_bad = int(_field(row, "pages_bad"))

        instances.add(instance)
        agg = get(instance, timeline)
        agg.observed_segnos.add(segno)
        agg.pages_total += pages_total
        agg.pages_checked += pages_checked
        agg.pages_ok += pages_ok
        agg.pages_bad += pages_bad
        if classification == "complete":
            agg.complete_segnos.add(segno)
            if conflict == 0 and segno not in agg.usable_seen:
                agg.usable_seen.add(segno)
                agg.usable_bytes += size
        elif classification == "truncated":
            agg.truncated_count += 1
        elif classification == "checksum_failed":
            agg.checksum_failed_count += 1
        if conflict:
            agg.conflict_version_count += 1

    for row in in_progress:
        instance = _field(row, "instance")
        timeline_value = _field(row, "timeline")
        instances.add(instance)
        if timeline_value is None:
            continue
        agg = get(instance, int(timeline_value))
        agg.writing_count += 1

    for row in recoveries:
        instance = _field(row, "instance")
        timeline = int(_field(row, "timeline"))
        instances.add(instance)
        get(instance, timeline).recovery_count += 1

    for agg in aggregates.values():
        agg.segment_count = len(agg.observed_segnos)
        agg.usable_segment_count = len(agg.usable_seen)

    return aggregates, instances, keys


def _label_escape(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def prometheus_text(
    aggregates: dict[tuple[str, int], Aggregate],
    *,
    scan_errors: int = 0,
) -> str:
    definitions = [
        (
            "wal_archive_segments_total",
            "gauge",
            "Distinct WAL segment slots observed for an instance and timeline.",
            "segments",
        ),
        (
            "wal_archive_usable_bytes",
            "gauge",
            "Bytes in complete, non-conflicting WAL segment versions.",
            "usable_bytes",
        ),
        (
            "wal_archive_writing_segments",
            "gauge",
            "WAL segments currently detected as being written.",
            "writing_segments",
        ),
        (
            "wal_archive_truncated_segments_total",
            "gauge",
            "WAL segment versions classified as truncated.",
            "truncated_segments",
        ),
        (
            "wal_archive_checksum_failures_total",
            "gauge",
            "WAL segment versions with failed header or page checksum verification.",
            "checksum_failed_segments",
        ),
        (
            "wal_archive_conflict_versions_total",
            "gauge",
            "WAL segment versions that are not the first completed valid version.",
            "conflict_versions",
        ),
        (
            "wal_archive_committed_recovery_points_total",
            "gauge",
            "Committed recovery points for an instance and timeline.",
            "committed_recovery_points",
        ),
    ]
    lines: list[str] = []
    for metric, metric_type, help_text, field in definitions:
        lines.append(f"# HELP {metric} {help_text}")
        lines.append(f"# TYPE {metric} {metric_type}")
        for instance, timeline in sorted(aggregates, key=lambda key: _sort_key(*key)):
            value = aggregates[(instance, timeline)].metrics_dict()[field]
            labels = (
                f'instance="{_label_escape(instance)}",'
                f'timeline="{timeline}"'
            )
            lines.append(f"{metric}{{{labels}}} {int(value)}")
    lines.append("# HELP wal_archive_scan_errors_total Current scan errors.")
    lines.append("# TYPE wal_archive_scan_errors_total gauge")
    lines.append(f"wal_archive_scan_errors_total {int(scan_errors)}")
    return "\n".join(lines) + "\n"


def recovery_row_to_dict(row: Any) -> dict[str, Any]:
    return {
        "archive_id": row["archive_id"],
        "chain": _json_load(row["chain_json"], []),
        "chain_length": int(row["chain_length"]),
        "created_at": row["created_at"],
        "gaps": _json_load(row["gaps_json"], []),
        "head_segno": int(row["head_segno"]),
        "id": row["id"],
        "instance": row["instance"],
        "request_key": row["request_key"],
        "selection": _json_load(row["selection_json"], []),
        "source_instance": row["source_instance"],
        "status": row["status"],
        "target_time": row["target_time"],
        "timeline": int(row["timeline"]),
        "validation": _json_load(row["validation_json"], {}),
    }


def _json_load(value: str, default: Any) -> Any:
    import json

    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return default
