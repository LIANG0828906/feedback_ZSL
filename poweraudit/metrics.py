"""Prometheus text exposition for the latest committed audit batch."""

from __future__ import annotations

from typing import Any


def _escape(value: Any) -> str:
    return str(value).replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def _labels(device: dict[str, Any]) -> str:
    return (
        f'device_id="{_escape(device["device_id"])}",'
        f'device_type="{_escape(device["device_type"])}"'
    )


def render_metrics(
    audit: dict[str, Any] | None,
    *,
    infeasible_plan_count: int,
    plan_count_value: int,
) -> str:
    lines: list[str] = [
        "# HELP poweraudit_device_online Whether a UPS or PDU is reachable and not offline.",
        "# TYPE poweraudit_device_online gauge",
    ]
    devices = sorted((audit or {}).get("devices", []), key=lambda item: item["device_id"])
    for device in devices:
        lines.append(
            f"poweraudit_device_online{{{_labels(device)}}} "
            f"{1 if device['online'] else 0}"
        )

    metric_defs = [
        (
            "poweraudit_device_server_count",
            "Unique servers with a power path attached to the device.",
            "server_count",
        ),
        (
            "poweraudit_device_all_outage_servers",
            "Servers that lose all power if the device fails.",
            "all_outage_count",
        ),
        (
            "poweraudit_device_one_path_loss_servers",
            "Servers that lose one power path if the device fails.",
            "one_path_loss_count",
        ),
        (
            "poweraudit_device_pseudo_redundant_servers",
            "Pseudo-redundant servers attached to the device.",
            "pseudo_redundant_count",
        ),
        (
            "poweraudit_device_single_path_servers",
            "Single-path servers attached to the device.",
            "single_path_count",
        ),
        (
            "poweraudit_device_topology_known",
            "Whether the device topology is fully known.",
            "topology_known",
        ),
    ]
    for name, help_text, field in metric_defs:
        lines.append(f"# HELP {name} {help_text}")
        lines.append(f"# TYPE {name} gauge")
        for device in devices:
            value = device[field]
            if isinstance(value, bool):
                value = 1 if value else 0
            lines.append(f"{name}{{{_labels(device)}}} {value}")

    lines.extend(
        [
            "# HELP poweraudit_current_audit_batch_info Current committed audit batch.",
            "# TYPE poweraudit_current_audit_batch_info gauge",
        ]
    )
    if audit is not None:
        lines.append(
            'poweraudit_current_audit_batch_info{batch_id="'
            + _escape(audit["batch_id"])
            + '"} 1'
        )
    else:
        lines.append("poweraudit_current_audit_batch_info 0")

    lines.extend(
        [
            "# HELP poweraudit_audit_servers Servers by redundancy status in the current batch.",
            "# TYPE poweraudit_audit_servers gauge",
        ]
    )
    totals = (audit or {}).get("totals", {})
    mapping = {
        "independent": "independent_servers",
        "pseudo_redundant": "pseudo_redundant_servers",
        "single_path": "single_path_servers",
        "topology_unknown": "topology_unknown_servers",
    }
    for status, field in mapping.items():
        lines.append(
            f'poweraudit_audit_servers{{redundancy_status="{status}"}} '
            f"{totals.get(field, 0)}"
        )

    lines.extend(
        [
            "# HELP poweraudit_topology_unknown_devices Devices excluded from normal risk ranking.",
            "# TYPE poweraudit_topology_unknown_devices gauge",
            f"poweraudit_topology_unknown_devices {totals.get('topology_unknown_devices', 0)}",
            "# HELP poweraudit_infeasible_plans Persisted non-executable maintenance plans.",
            "# TYPE poweraudit_infeasible_plans gauge",
            f"poweraudit_infeasible_plans {infeasible_plan_count}",
            "# HELP poweraudit_plans Persisted plan count.",
            "# TYPE poweraudit_plans gauge",
            f"poweraudit_plans {plan_count_value}",
        ]
    )
    return "\n".join(lines) + "\n"
