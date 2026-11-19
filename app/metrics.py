from __future__ import annotations

from .database import Database


def _label(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def render_metrics(database: Database) -> str:
    rows = database.metrics_rows()
    lines = [
        "# HELP webhook_dispatch_success_total Successfully delivered events.",
        "# TYPE webhook_dispatch_success_total counter",
    ]
    for row in rows:
        lines.append(
            f'webhook_dispatch_success_total{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["success_total"])}'
        )

    lines.extend(
        [
            "# HELP webhook_dispatch_failure_total Failed dispatch attempts.",
            "# TYPE webhook_dispatch_failure_total counter",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_dispatch_failure_total{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["failure_total"])}'
        )

    lines.extend(
        [
            "# HELP webhook_dispatch_retry_total Retries scheduled after transient failures.",
            "# TYPE webhook_dispatch_retry_total counter",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_dispatch_retry_total{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["retry_total"])}'
        )

    lines.extend(
        [
            "# HELP webhook_dispatch_dead_letter_total Events moved to the dead-letter table.",
            "# TYPE webhook_dispatch_dead_letter_total counter",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_dispatch_dead_letter_total{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["dead_letter_total"])}'
        )

    lines.extend(
        [
            "# HELP webhook_dispatch_inflight Dispatch requests currently in flight.",
            "# TYPE webhook_dispatch_inflight gauge",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_dispatch_inflight{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["inflight"])}'
        )

    lines.extend(
        [
            "# HELP webhook_tenant_queue_depth Pending events, including a degraded tenant's waiting queue.",
            "# TYPE webhook_tenant_queue_depth gauge",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_tenant_queue_depth{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(row["queue_depth"])}'
        )

    lines.extend(
        [
            "# HELP webhook_tenant_degraded Whether a tenant is quarantined for timeout degradation.",
            "# TYPE webhook_tenant_degraded gauge",
        ]
    )
    for row in rows:
        lines.append(
            f'webhook_tenant_degraded{{tenant="{_label(row["tenant_id"])}"}} '
            f'{int(bool(row["degraded"]))}'
        )
    return "\n".join(lines) + "\n"

