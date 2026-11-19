from __future__ import annotations

from typing import Any

from .audit import AuditStore
from .config import Config
from .db import DatabaseSet
from .logic import json_safe


def normalize_row_type(value: str) -> str:
    aliases = {
        "member": "members",
        "members": "members",
        "order": "orders",
        "orders": "orders",
    }
    try:
        return aliases[value.lower()]
    except KeyError as exc:
        raise ValueError("row type must be member or order") from exc


class TraceService:
    def __init__(self, config: Config, databases: DatabaseSet, audit: AuditStore):
        self.config = config
        self.databases = databases
        self.audit = audit

    def _current_row(self, site: str, row_type: str, row_id: int) -> dict[str, Any] | None:
        if row_type == "members":
            sql = "SELECT * FROM members WHERE member_id = %s"
        else:
            sql = "SELECT * FROM orders WHERE order_id = %s"
        pool = self.databases.pool(site)
        with pool.connection(self.config.limits.api_acquire_timeout_ms) as conn:
            with conn.transaction():
                row = conn.execute(sql, (row_id,)).fetchone()
        return json_safe(dict(row)) if row else None

    def _source_changes(
        self, site: str, row_type: str, row_id: int, limit: int
    ) -> list[dict[str, Any]]:
        pool = self.databases.pool(site)
        with pool.connection(self.config.limits.api_acquire_timeout_ms) as conn:
            with conn.transaction():
                rows = conn.execute(
                    """
                    SELECT change_id, origin_site, table_name, row_id,
                           row_version, op, old_row, new_row, points_delta,
                           old_points, new_points, old_status, new_status,
                           created_at
                    FROM sync_change
                    WHERE table_name = %s AND row_id = %s
                    ORDER BY change_id DESC LIMIT %s
                    """,
                    (row_type, row_id, limit),
                ).fetchall()
        return [
            {
                "change_key": f"{row['origin_site']}:{row['change_id']}",
                "source_site": row["origin_site"],
                "source_change_id": int(row["change_id"]),
                "table_name": row["table_name"],
                "row_id": int(row["row_id"]),
                "row_version": int(row["row_version"]),
                "op": row["op"],
                "old_row": json_safe(row["old_row"]),
                "new_row": json_safe(row["new_row"]),
                "points_delta": row["points_delta"],
                "old_points": row["old_points"],
                "new_points": row["new_points"],
                "old_status": row["old_status"],
                "new_status": row["new_status"],
                "created_at": json_safe(row["created_at"]),
            }
            for row in rows
        ]

    def trace(self, row_type: str, row_id: int, limit: int = 30) -> dict[str, Any]:
        row_type = normalize_row_type(row_type)
        limit = min(200, max(1, limit))
        east_row = self._current_row("east", row_type, row_id)
        south_row = self._current_row("south", row_type, row_id)
        east_changes = self._source_changes("east", row_type, row_id, limit)
        south_changes = self._source_changes("south", row_type, row_id, limit)
        source_changes = sorted(
            east_changes + south_changes,
            key=lambda item: (item["created_at"] or "", item["change_key"]),
            reverse=True,
        )[:limit]
        applied = self.audit.recent_changes(row_type, row_id, limit)
        conflicts = self.audit.conflicts_for_row(row_type, row_id, limit=limit)
        deliveries = self.audit.deliveries_for_row(row_type, row_id, limit=limit)
        field_state = self.audit.field_state(row_type, row_id)
        final_column = "points" if row_type == "members" else "status"
        if final_column not in field_state:
            value = (
                east_row.get(final_column)
                if east_row is not None
                else south_row.get(final_column) if south_row is not None else None
            )
            if source_changes:
                field_state[final_column] = {
                    "value": value,
                    "last_change_key": source_changes[0]["change_key"],
                    "basis": "local_source_change_or_seed",
                    "updated_at": source_changes[0].get("created_at"),
                }
            else:
                field_state[final_column] = {
                    "value": value,
                    "last_change_key": None,
                    "basis": "seed_value",
                    "updated_at": None,
                }
        return {
            "row_type": row_type,
            "row_id": row_id,
            "current": {
                "east": east_row,
                "south": south_row,
            },
            "converged": (
                east_row is not None
                and south_row is not None
                and (
                    east_row.get("points") == south_row.get("points")
                    if row_type == "members"
                    else east_row.get("status") == south_row.get("status")
                )
            ),
            "final_adopted_values": {
                column: detail.get("value")
                for column, detail in field_state.items()
            },
            "field_decisions": field_state,
            "recent_changes": applied,
            "source_outbox_changes": source_changes,
            "conflicts": conflicts,
            "duplicate_deliveries_visible": [
                item
                for item in deliveries
                if item.get("duplicate_of") or item.get("outcome") == "duplicate"
            ],
            "delivery_history": deliveries,
        }
