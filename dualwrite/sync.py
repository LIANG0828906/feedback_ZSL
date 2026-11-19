from __future__ import annotations

import logging
import threading
import time
from typing import Any

import psycopg
from psycopg.types.json import Jsonb

from .audit import AuditStore
from .config import Config
from .db import DatabaseSet
from .inject import Delivery, DeliveryScheduler
from .logic import json_safe, status_decision

LOGGER = logging.getLogger(__name__)

TABLES = {
    "members": {
        "id_column": "member_id",
    },
    "orders": {
        "id_column": "order_id",
    },
}


def _conflicts_from_event(event: dict[str, Any]) -> list[dict[str, Any]]:
    conflicts: list[dict[str, Any]] = []
    decisions = event.get("field_decisions") or {}
    for column, detail in decisions.items():
        if not detail.get("conflict"):
            continue
        conflicts.append(
            {
                "change_key": event["change_key"],
                "source_site": event["origin_site"],
                "table_name": event["table_name"],
                "row_id": int(event["row_id"]),
                "column_name": column,
                "incoming_version": int(detail["incoming_row_version"]),
                "current_version": int(detail["current_row_version"]),
                "incoming_value": detail.get("incoming_value"),
                "current_value": detail.get("current_value"),
                "adopted_value": detail.get("adopted_value"),
                "basis": detail["basis"],
                "basis_detail": detail.get("basis_detail", detail["basis"]),
                "created_at": event.get("applied_at"),
            }
        )
    return conflicts


class SyncEngine:
    def __init__(self, config: Config, databases: DatabaseSet, audit: AuditStore):
        self.config = config
        self.databases = databases
        self.audit = audit
        self.scheduler = DeliveryScheduler(config.injection)
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._next_fetch: dict[str, int] = {}
        self._last_error: str | None = None
        for source, target in self.directions():
            self._next_fetch[source] = audit.cursor_position(source, target) + 1

    def directions(self) -> list[tuple[str, str]]:
        names = sorted(self.config.sites)
        return [(names[0], names[1]), (names[1], names[0])]

    def start(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._run, name="dualwrite-sync", daemon=True
        )
        self._thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=timeout)

    @property
    def last_error(self) -> str | None:
        return self._last_error

    @property
    def in_flight_deliveries(self) -> int:
        return self.scheduler.pending

    def _fetch(
        self, source: str, target: str, after_change_id: int
    ) -> list[dict[str, Any]]:
        pool = self.databases.pool(source)
        with pool.connection(self.config.limits.sync_acquire_timeout_ms) as conn:
            with conn.transaction():
                rows = conn.execute(
                    """
                    SELECT change_id, origin_site, table_name, row_id,
                           row_version, op, old_row, new_row,
                           points_delta, old_points, new_points,
                           old_status, new_status, created_at
                    FROM sync_change
                    WHERE change_id > %s
                    ORDER BY change_id
                    LIMIT %s
                    """,
                    (after_change_id, self.config.sync.fetch_size),
                ).fetchall()
        return [dict(row) for row in rows]

    def _run(self) -> None:
        while not self._stop.is_set():
            did_work = False
            try:
                if self.scheduler.pending < 10000:
                    for source, target in self.directions():
                        next_id = self._next_fetch.get(source, 1)
                        changes = self._fetch(source, target, next_id - 1)
                        for change in changes:
                            self.scheduler.enqueue(source, target, change)
                            next_id = max(next_id, int(change["change_id"]) + 1)
                        self._next_fetch[source] = next_id
                        did_work = did_work or bool(changes)

                for _ in range(200):
                    delivery = self.scheduler.pop_ready()
                    if delivery is None:
                        break
                    did_work = True
                    self._process_delivery(delivery)
            except Exception as exc:
                self._last_error = f"{type(exc).__name__}: {exc}"
                LOGGER.exception("sync loop error")
                time.sleep(max(0.05, self.config.sync.poll_interval_ms / 1000.0))
                continue

            if not did_work:
                next_due = self.scheduler.next_due_in()
                sleep_for = self.config.sync.poll_interval_ms / 1000.0
                if next_due is not None:
                    sleep_for = min(sleep_for, max(0.001, next_due))
                self._stop.wait(sleep_for)

    def _process_delivery(self, delivery: Delivery) -> None:
        try:
            self.audit.record_delivery(
                delivery_id=delivery.delivery_id,
                change_key=delivery.change_key,
                source_site=delivery.source_site,
                target_site=delivery.target_site,
                source_change_id=delivery.source_change_id,
                table_name=delivery.change["table_name"],
                row_id=int(delivery.change["row_id"]),
                row_version=int(delivery.change["row_version"]),
                duplicate_of=delivery.duplicate_of,
            )
            result = self._apply_to_target(delivery)
            self.audit.record_apply_bundle(
                delivery={
                    "delivery_id": delivery.delivery_id,
                    "change_key": delivery.change_key,
                    "source_site": delivery.source_site,
                    "target_site": delivery.target_site,
                    "source_change_id": delivery.source_change_id,
                    "duplicate_of": delivery.duplicate_of,
                },
                event=result["event"],
                conflicts=result["conflicts"],
                field_updates=result["field_updates"],
                duplicate=bool(result["duplicate"]),
            )
            self._last_error = None
        except Exception as exc:
            self._last_error = f"{type(exc).__name__}: {exc}"
            LOGGER.warning(
                "delivery %s (%s) failed, retrying: %s",
                delivery.delivery_id,
                delivery.change_key,
                exc,
            )
            self.scheduler.requeue(delivery, delay_ms=100)

    def _apply_to_target(self, delivery: Delivery) -> dict[str, Any]:
        target = delivery.target_site
        change = delivery.change
        change_key = delivery.change_key
        pool = self.databases.pool(target)
        with pool.connection(self.config.limits.sync_acquire_timeout_ms) as conn:
            with conn.transaction():
                conn.execute("SELECT set_config('app.sync_apply', 'on', true)")
                claimed = conn.execute(
                    """
                    INSERT INTO sync_applied (
                        change_key, origin_site, source_change_id,
                        target_site, table_name, row_id, row_version
                    ) VALUES (%s, %s, %s, %s, %s, %s, %s)
                    ON CONFLICT (change_key) DO NOTHING
                    RETURNING change_key
                    """,
                    (
                        change_key,
                        delivery.source_site,
                        delivery.source_change_id,
                        target,
                        change["table_name"],
                        int(change["row_id"]),
                        int(change["row_version"]),
                    ),
                ).fetchone()
                if not claimed:
                    existing = conn.execute(
                        "SELECT * FROM sync_apply_events WHERE change_key = %s",
                        (change_key,),
                    ).fetchone()
                    if not existing:
                        raise RuntimeError(
                            f"change {change_key} is claimed but has no application event"
                        )
                    event = dict(existing)
                    return {
                        "event": event,
                        "conflicts": _conflicts_from_event(event),
                        "field_updates": self._field_updates_from_event(event),
                        "duplicate": True,
                    }

                applied = self._apply_claimed_change(conn, delivery)
                event_row = conn.execute(
                    """
                    INSERT INTO sync_apply_events (
                        change_key, origin_site, source_change_id, target_site,
                        table_name, row_id, row_version, outcome,
                        stale_fields, field_decisions, old_row, new_row,
                        points_delta, points_merged, status_overridden
                    ) VALUES (
                        %s, %s, %s, %s, %s, %s, %s, %s,
                        %s, %s, %s, %s, %s, %s, %s
                    )
                    RETURNING *
                    """,
                    (
                        change_key,
                        delivery.source_site,
                        delivery.source_change_id,
                        target,
                        change["table_name"],
                        int(change["row_id"]),
                        int(change["row_version"]),
                        applied["outcome"],
                        Jsonb(applied["stale_fields"]),
                        Jsonb(applied["field_decisions"]),
                        Jsonb(json_safe(applied["old_row"])),
                        Jsonb(json_safe(applied["new_row"])),
                        applied.get("points_delta"),
                        applied["points_merged"],
                        applied["status_overridden"],
                    ),
                ).fetchone()
                return {
                    "event": dict(event_row),
                    "conflicts": applied["conflicts"],
                    "field_updates": applied["field_updates"],
                    "duplicate": False,
                }

    def _field_updates_from_event(
        self, event: dict[str, Any]
    ) -> list[dict[str, Any]]:
        updates: list[dict[str, Any]] = []
        table_name = event["table_name"]
        row_id = int(event["row_id"])
        decisions = event.get("field_decisions") or {}
        new_row = event.get("new_row") or {}
        if table_name == "members" and decisions.get("points", {}).get("applied"):
            updates.append(
                {
                    "table_name": table_name,
                    "row_id": row_id,
                    "column_name": "points",
                    "value": new_row.get("points"),
                    "last_change_key": event["change_key"],
                    "basis": decisions["points"]["basis"],
                }
            )
        if table_name == "orders" and decisions.get("status", {}).get("adopted"):
            updates.append(
                {
                    "table_name": table_name,
                    "row_id": row_id,
                    "column_name": "status",
                    "value": new_row.get("status"),
                    "last_change_key": event["change_key"],
                    "basis": decisions["status"]["basis"],
                }
            )
        return updates

    def _load_state(
        self,
        conn: psycopg.Connection,
        table_name: str,
        row_id: int,
        row: dict[str, Any] | None,
        origin_site: str,
    ) -> dict[str, Any]:
        state = conn.execute(
            """
            SELECT * FROM sync_row_state
            WHERE table_name = %s AND row_id = %s
            FOR UPDATE
            """,
            (table_name, row_id),
        ).fetchone()
        if state:
            return dict(state)
        version = int(row["row_version"]) if row else 1
        return {
            "table_name": table_name,
            "row_id": row_id,
            "row_version": version,
            "points_version": version if table_name == "members" else None,
            "points_site": origin_site if table_name == "members" else None,
            "last_points_change_key": None,
            "status_version": version if table_name == "orders" else None,
            "status_site": origin_site if table_name == "orders" else None,
            "last_status_change_key": None,
        }

    def _apply_claimed_change(
        self, conn: psycopg.Connection, delivery: Delivery
    ) -> dict[str, Any]:
        change = delivery.change
        table_name = str(change["table_name"])
        info = TABLES[table_name]
        id_column = info["id_column"]
        row_id = int(change["row_id"])
        origin = delivery.source_site
        incoming_version = int(change["row_version"])
        incoming_updated_at = (change.get("new_row") or {}).get("updated_at")

        row = conn.execute(
            f"SELECT * FROM {table_name} WHERE {id_column} = %s FOR UPDATE",
            (row_id,),
        ).fetchone()
        current_row = dict(row) if row else None
        state = self._load_state(conn, table_name, row_id, current_row, delivery.target_site)
        stale_fields: list[str] = []
        decisions: dict[str, Any] = {}
        conflicts: list[dict[str, Any]] = []
        field_updates: list[dict[str, Any]] = []
        points_merged = False
        status_overridden = False

        if current_row is None:
            payload = dict(change["new_row"] or {})
            payload["row_version"] = incoming_version
            if table_name == "members":
                result_row = conn.execute(
                    """
                    INSERT INTO members (member_id, points, row_version, updated_at)
                    VALUES (%s, %s, %s, COALESCE(%s::timestamptz, clock_timestamp()))
                    RETURNING *
                    """,
                    (
                        row_id,
                        int(payload.get("points", 0)),
                        incoming_version,
                        incoming_updated_at,
                    ),
                ).fetchone()
                decisions["points"] = {
                    "operation": "remote_insert",
                    "applied": True,
                    "incoming_row_version": incoming_version,
                    "current_row_version": state.get("points_version") or 1,
                    "incoming_value": payload.get("points"),
                    "current_value": None,
                    "adopted_value": int(payload.get("points", 0)),
                    "basis": "remote_insert",
                    "basis_detail": "target row absent; inserted source value",
                    "conflict": False,
                }
                state["points_version"] = incoming_version
                state["points_site"] = origin
                state["last_points_change_key"] = delivery.change_key
                state["row_version"] = incoming_version
                field_updates.append(
                    {
                        "table_name": table_name,
                        "row_id": row_id,
                        "column_name": "points",
                        "value": payload.get("points"),
                        "last_change_key": delivery.change_key,
                        "basis": "remote_insert",
                    }
                )
            else:
                status = payload.get("status", change.get("new_status"))
                result_row = conn.execute(
                    """
                    INSERT INTO orders (order_id, status, row_version, updated_at)
                    VALUES (%s, %s, %s, COALESCE(%s::timestamptz, clock_timestamp()))
                    RETURNING *
                    """,
                    (row_id, status, incoming_version, incoming_updated_at),
                ).fetchone()
                decisions["status"] = {
                    "operation": "remote_insert",
                    "applied": True,
                    "adopted": True,
                    "incoming_row_version": incoming_version,
                    "current_row_version": state.get("status_version") or 1,
                    "incoming_value": status,
                    "current_value": None,
                    "adopted_value": status,
                    "basis": "remote_insert",
                    "basis_detail": "target row absent; inserted source status",
                    "conflict": False,
                }
                status_overridden = True
                state["status_version"] = incoming_version
                state["status_site"] = origin
                state["last_status_change_key"] = delivery.change_key
                state["row_version"] = incoming_version
                field_updates.append(
                    {
                        "table_name": table_name,
                        "row_id": row_id,
                        "column_name": "status",
                        "value": status,
                        "last_change_key": delivery.change_key,
                        "basis": "remote_insert",
                    }
                )
        elif table_name == "members":
            current_points = int(current_row["points"])
            delta = int(change.get("points_delta") or 0)
            field_version = int(state.get("points_version") or current_row["row_version"])
            field_site = state.get("points_site")
            adopted_points = current_points + delta
            overlap = (
                delta != 0
                and field_site is not None
                and field_site != origin
                and incoming_version <= field_version
            )
            basis = "incremental_merge_overlap" if overlap else "incremental_merge"
            decisions["points"] = {
                "operation": "incremental_add",
                "applied": delta != 0,
                "incoming_row_version": incoming_version,
                "current_row_version": field_version,
                "incoming_value": int(change.get("new_points") or 0),
                "delta": delta,
                "current_value": current_points,
                "adopted_value": adopted_points,
                "basis": basis,
                "basis_detail": (
                    "peer current value plus source delta; stale version still carries "
                    "a commutative points delta"
                    if overlap
                    else "peer current value plus source delta"
                ),
                "conflict": overlap,
            }
            if incoming_version < field_version:
                stale_fields.append("points")
            state["last_points_change_key"] = delivery.change_key
            if incoming_version >= field_version:
                state["points_version"] = incoming_version
                state["points_site"] = origin
            if delta != 0:
                points_merged = True
                field_updates.append(
                    {
                        "table_name": table_name,
                        "row_id": row_id,
                        "column_name": "points",
                        "value": adopted_points,
                        "last_change_key": delivery.change_key,
                        "basis": basis,
                    }
                )
            result_row = conn.execute(
                """
                UPDATE members
                SET points = %s,
                    row_version = GREATEST(row_version, %s),
                    updated_at = GREATEST(
                        updated_at,
                        COALESCE(%s::timestamptz, updated_at)
                    )
                WHERE member_id = %s
                RETURNING *
                """,
                (adopted_points, incoming_version, incoming_updated_at, row_id),
            ).fetchone()
        else:
            current_status = str(current_row["status"])
            incoming_status = str(
                change.get("new_status")
                if change.get("new_status") is not None
                else (change.get("new_row") or {}).get("status")
            )
            field_version = int(state.get("status_version") or current_row["row_version"])
            field_site = state.get("status_site") or delivery.target_site
            decision = status_decision(
                incoming_version=incoming_version,
                incoming_site=origin,
                current_version=field_version,
                current_site=field_site,
                priorities=self.config.priorities,
            )
            changed = decision.adopt and incoming_status != current_status
            overlap = (
                incoming_status != current_status
                and field_site != origin
                and incoming_version <= field_version
            )
            if incoming_version < field_version or (
                not decision.adopt and incoming_status != current_status
            ):
                stale_fields.append("status")
            if decision.adopt:
                state["status_version"] = incoming_version
                state["status_site"] = origin
                state["last_status_change_key"] = delivery.change_key
                field_updates.append(
                    {
                        "table_name": table_name,
                        "row_id": row_id,
                        "column_name": "status",
                        "value": incoming_status,
                        "last_change_key": delivery.change_key,
                        "basis": decision.basis,
                    }
                )
                if changed:
                    status_overridden = True
            decisions["status"] = {
                "operation": "exclusive_replace",
                "applied": changed,
                "adopted": decision.adopt,
                "incoming_row_version": incoming_version,
                "current_row_version": field_version,
                "incoming_site": origin,
                "current_site": field_site,
                "incoming_value": incoming_status,
                "current_value": current_status,
                "adopted_value": incoming_status if decision.adopt else current_status,
                "incoming_key": list(decision.incoming_key),
                "current_key": list(decision.current_key),
                "basis": decision.basis,
                "basis_detail": (
                    "newer row version wins; equal versions use stable site priority"
                ),
                "conflict": overlap,
            }
            result_row = conn.execute(
                """
                UPDATE orders
                SET status = %s,
                    row_version = GREATEST(row_version, %s),
                    updated_at = GREATEST(
                        updated_at,
                        COALESCE(%s::timestamptz, updated_at)
                    )
                WHERE order_id = %s
                RETURNING *
                """,
                (
                    incoming_status if decision.adopt else current_status,
                    incoming_version,
                    incoming_updated_at,
                    row_id,
                ),
            ).fetchone()

        if result_row is None:
            raise RuntimeError(f"failed to apply {delivery.change_key}")
        result = dict(result_row)
        state["row_version"] = max(
            int(state.get("row_version") or 0),
            int(result.get("row_version") or 0),
            incoming_version,
        )
        conn.execute(
            """
            INSERT INTO sync_row_state (
                table_name, row_id, row_version,
                points_version, points_site, last_points_change_key,
                status_version, status_site, last_status_change_key
            ) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
            ON CONFLICT (table_name, row_id) DO UPDATE
            SET row_version = GREATEST(sync_row_state.row_version, excluded.row_version),
                points_version = COALESCE(excluded.points_version, sync_row_state.points_version),
                points_site = COALESCE(excluded.points_site, sync_row_state.points_site),
                last_points_change_key = COALESCE(
                    excluded.last_points_change_key,
                    sync_row_state.last_points_change_key
                ),
                status_version = COALESCE(excluded.status_version, sync_row_state.status_version),
                status_site = COALESCE(excluded.status_site, sync_row_state.status_site),
                last_status_change_key = COALESCE(
                    excluded.last_status_change_key,
                    sync_row_state.last_status_change_key
                ),
                updated_at = clock_timestamp()
            """,
            (
                table_name,
                row_id,
                state["row_version"],
                state.get("points_version"),
                state.get("points_site"),
                state.get("last_points_change_key"),
                state.get("status_version"),
                state.get("status_site"),
                state.get("last_status_change_key"),
            ),
        )

        for column, detail in decisions.items():
            if not detail.get("conflict"):
                continue
            conflicts.append(
                {
                    "change_key": delivery.change_key,
                    "source_site": origin,
                    "table_name": table_name,
                    "row_id": row_id,
                    "column_name": column,
                    "incoming_version": incoming_version,
                    "current_version": int(detail["current_row_version"]),
                    "incoming_value": detail.get("incoming_value"),
                    "current_value": detail.get("current_value"),
                    "adopted_value": detail.get("adopted_value"),
                    "basis": detail["basis"],
                    "basis_detail": detail["basis_detail"],
                }
            )

        return {
            "outcome": "partial_stale" if stale_fields else "applied",
            "stale_fields": stale_fields,
            "field_decisions": decisions,
            "old_row": current_row,
            "new_row": result,
            "points_delta": change.get("points_delta"),
            "points_merged": points_merged,
            "status_overridden": status_overridden,
            "conflicts": conflicts,
            "field_updates": field_updates,
        }
