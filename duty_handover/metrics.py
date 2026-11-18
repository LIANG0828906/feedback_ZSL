"""Prometheus text exposition."""

from __future__ import annotations

from typing import Any

from .policy import SEVERITY_RANK


def _escape_label(value: str) -> str:
    return (
        str(value)
        .replace("\\", "\\\\")
        .replace("\n", "\\n")
        .replace('"', '\\"')
    )


def _value(value: Any) -> str:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return "0"
    if number == 0:
        number = 0.0
    return format(number, ".12g")


def render_metrics(service: Any) -> str:
    latest = service.store.get_latest_snapshot()
    catalog = list(service.samples.services)
    snapshot_counts = {name: 0 for name in catalog}
    invalid_totals = {name: 0 for name in catalog}
    for record in service.store.iter_snapshots():
        snapshot = record["snapshot"]
        for name, item in snapshot.get("services", {}).items():
            if name not in snapshot_counts:
                snapshot_counts[name] = 0
                invalid_totals[name] = 0
            snapshot_counts[name] += 1
            invalid_totals[name] += int(item.get("silence_invalid_count", 0))
    services = sorted(set(catalog) | set(snapshot_counts))
    latest_services = latest["snapshot"].get("services", {}) if latest else {}

    lines: list[str] = [
        "# HELP handover_error_budget_remaining_ratio Remaining error budget ratio.",
        "# TYPE handover_error_budget_remaining_ratio gauge",
    ]
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_error_budget_remaining_ratio{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("error_budget_remaining_ratio", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_error_budget_remaining_pct Remaining error budget percentage.",
            "# TYPE handover_error_budget_remaining_pct gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_error_budget_remaining_pct{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("error_budget_remaining_pct", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_error_budget_remaining_requests Remaining error budget in request equivalents.",
            "# TYPE handover_error_budget_remaining_requests gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_error_budget_remaining_requests{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("error_budget_remaining_requests", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_burn_rate_short Short-window burn rate.",
            "# TYPE handover_burn_rate_short gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_burn_rate_short{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("burn_rate_short", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_burn_rate_long Long-window burn rate.",
            "# TYPE handover_burn_rate_long gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_burn_rate_long{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("burn_rate_long", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_current_severity Current recommended severity (1=P1, 2=P2, 3=P3, 4=P4, 0=unknown).",
            "# TYPE handover_current_severity gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        grade = item.get("current_grade")
        lines.append(
            f'handover_current_severity{{service="{_escape_label(name)}"}} '
            f'{SEVERITY_RANK.get(grade, 0)}'
        )
    lines.extend(
        [
            "# HELP handover_current_grade Current recommended severity label.",
            "# TYPE handover_current_grade gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        grade = item.get("current_grade", "UNKNOWN")
        lines.append(
            f'handover_current_grade{{service="{_escape_label(name)}",grade="{_escape_label(grade)}"}} 1'
        )
    lines.extend(
        [
            "# HELP handover_silence_invalid_count Invalid temporary silences in the latest snapshot.",
            "# TYPE handover_silence_invalid_count gauge",
        ]
    )
    for name in services:
        item = latest_services.get(name, {})
        lines.append(
            f'handover_silence_invalid_count{{service="{_escape_label(name)}"}} '
            f'{_value(item.get("silence_invalid_count", 0))}'
        )
    lines.extend(
        [
            "# HELP handover_silence_invalid_total Historical sum of invalid temporary silences.",
            "# TYPE handover_silence_invalid_total counter",
        ]
    )
    for name in services:
        lines.append(
            f'handover_silence_invalid_total{{service="{_escape_label(name)}"}} '
            f'{_value(invalid_totals.get(name, 0))}'
        )
    lines.extend(
        [
            "# HELP handover_snapshot_count Number of committed snapshots containing this service.",
            "# TYPE handover_snapshot_count gauge",
        ]
    )
    for name in services:
        lines.append(
            f'handover_snapshot_count{{service="{_escape_label(name)}"}} '
            f'{_value(snapshot_counts.get(name, 0))}'
        )
    lines.extend(
        [
            "# HELP handover_snapshot_total Total number of committed snapshots.",
            "# TYPE handover_snapshot_total gauge",
            f"handover_snapshot_total {_value(service.store.snapshot_count())}",
        ]
    )
    return "\n".join(lines) + "\n"
