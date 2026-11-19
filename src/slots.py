"""Read logical slots and subscriptions from both instances.

Each query is isolated.  A failure on one query or one instance becomes a
stable SG-E1001/SG-E1002 error entry and never aborts the other instance's
summary.
"""
from __future__ import annotations

import time
from typing import Any

from config import Config
from db import SlotGuardError, instance_conn, lsn_int, safe_query

SLOT_SQL = """
SELECT s.slot_name,
       s.plugin,
       s.slot_type,
       s.active,
       s.active_pid,
       s.restart_lsn::text,
       s.confirmed_flush_lsn::text,
       s.wal_status,
       s.safe_wal_size,
       s.inactive_since,
       CASE WHEN s.restart_lsn IS NULL THEN NULL
            ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), s.restart_lsn)::bigint
       END AS retained_wal_bytes,
       pg_current_wal_lsn()::text AS current_lsn,
       sub.subname AS subscription_name
FROM pg_replication_slots s
LEFT JOIN pg_subscription sub ON sub.subslotname = s.slot_name
WHERE s.slot_type = 'logical'
ORDER BY s.slot_name
"""

SUB_SQL = """
SELECT s.subname, s.subslotname, s.subenabled, s.subconninfo,
       r.remote_lsn::text AS applied_lsn
FROM pg_subscription s
LEFT JOIN pg_replication_origin_status r
       ON r.external_id = 'pg_' || s.oid::text
ORDER BY s.subname
"""


def _error_payload(exc: SlotGuardError) -> dict[str, str]:
    return {
        "code": exc.code,
        "instance": exc.instance,
        "target": exc.target,
        "message": exc.message,
    }


def _read_instance(cfg: Config, instance: str, dsn: str) -> dict[str, Any]:
    result: dict[str, Any] = {
        "slots": [],
        "subscriptions": [],
        "errors": [],
        "error": None,
    }
    try:
        with instance_conn(
            dsn, cfg.connect_timeout_s, instance, autocommit=True
        ) as conn:
            try:
                rows = safe_query(conn, instance, "pg_replication_slots", SLOT_SQL)
                for row in rows:
                    (
                        slot_name, plugin, slot_type, active, active_pid,
                        restart_lsn, confirmed_lsn, wal_status, safe_wal_size,
                        inactive_since, retained_bytes, current_lsn,
                        subscription_name,
                    ) = row
                    current_i = lsn_int(current_lsn)
                    confirmed_i = lsn_int(confirmed_lsn)
                    lag_bytes = (
                        max(0, current_i - confirmed_i)
                        if current_i is not None and confirmed_i is not None
                        else None
                    )
                    inactive_seconds = None
                    if inactive_since is not None:
                        inactive_seconds = max(
                            0.0, (time.time() - inactive_since.timestamp())
                        )
                    result["slots"].append(
                        {
                            "instance": instance,
                            "slot_name": slot_name,
                            "plugin": plugin,
                            "slot_type": slot_type,
                            "active": bool(active),
                            "active_pid": active_pid,
                            "restart_lsn": restart_lsn,
                            "confirmed_lsn": confirmed_lsn,
                            "current_lsn": current_lsn,
                            "lag_bytes": lag_bytes,
                            "wal_status": wal_status,
                            "safe_wal_size": safe_wal_size,
                            "inactive_since": inactive_since.isoformat()
                            if inactive_since is not None
                            else None,
                            "inactive_seconds": inactive_seconds,
                            "retained_wal_bytes": int(retained_bytes)
                            if retained_bytes is not None
                            else None,
                            "subscription_name": subscription_name,
                        }
                    )
            except SlotGuardError as exc:
                result["errors"].append(_error_payload(exc))

            try:
                rows = safe_query(conn, instance, "pg_subscription", SUB_SQL)
                for row in rows:
                    subname, subslotname, subenabled, subconninfo, applied_lsn = row
                    result["subscriptions"].append(
                        {
                            "instance": instance,
                            "subscription_name": subname,
                            "slot_name": subslotname,
                            "enabled": bool(subenabled),
                            "conninfo": subconninfo,
                            "last_lsn": applied_lsn,
                        }
                    )
            except SlotGuardError as exc:
                result["errors"].append(_error_payload(exc))
    except SlotGuardError as exc:
        result["errors"].append(_error_payload(exc))

    if result["errors"]:
        first = result["errors"][0]
        result["error"] = (
            f"[{first['code']}] instance={first['instance']} "
            f"target={first['target']}: {first['message']}"
        )
    return result


def collect(cfg: Config) -> dict[str, Any]:
    """Return per-instance payloads and a merged logical-slot view."""
    per_instance = {
        name: _read_instance(cfg, name, dsn)
        for name, dsn in cfg.instances.items()
    }
    merged: dict[str, dict[str, Any]] = {}
    for instance, payload in per_instance.items():
        for slot in payload["slots"]:
            view = merged.setdefault(
                slot["slot_name"],
                {
                    "slot_name": slot["slot_name"],
                    "instances": {},
                    "exists_on_both": False,
                    "max_lag_bytes": 0,
                    "max_retained_wal_bytes": 0,
                },
            )
            view["instances"][instance] = slot
            view["exists_on_both"] = len(view["instances"]) > 1
            if slot["lag_bytes"] is not None:
                view["max_lag_bytes"] = max(
                    view["max_lag_bytes"], slot["lag_bytes"]
                )
            if slot["retained_wal_bytes"] is not None:
                view["max_retained_wal_bytes"] = max(
                    view["max_retained_wal_bytes"],
                    slot["retained_wal_bytes"],
                )
    return {"per_instance": per_instance, "merged": merged}
