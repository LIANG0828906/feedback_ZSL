"""Tiered, non-destructive relief advice for inactive logical slots."""
from __future__ import annotations

import time
from typing import Any

from config import Config
from store import Store


def _subscriber_id(
    cfg: Config, view: dict[str, Any], instance: str, slot: dict[str, Any]
) -> str:
    if slot.get("subscription_name"):
        return f"{instance}:{slot['subscription_name']}"
    for payload in view["per_instance"].values():
        for sub in payload.get("subscriptions", []):
            if sub.get("slot_name") == slot["slot_name"]:
                return f"{sub['instance']}:{sub['subscription_name']}"
    if slot["slot_name"] == cfg.slot_name:
        return f"{instance}:{cfg.subscription_name}"
    return f"{instance}:{slot['slot_name']}"


def evaluate(
    cfg: Config,
    store: Store,
    round_id: str,
    view: dict[str, Any],
    now: float | None = None,
) -> list[dict[str, Any]]:
    """Evaluate every logical slot independently on every instance.

    This function only writes advice rows.  It never calls
    pg_drop_replication_slot, and active slots are never recycle candidates.
    """
    now = now or time.time()
    threshold = cfg.wal_retention_threshold_bytes
    advices: list[dict[str, Any]] = []
    seen: set[tuple[str, str]] = set()

    for slot_name, merged in view["merged"].items():
        for instance, slot in merged["instances"].items():
            seen.add((slot_name, instance))
            retained = slot.get("retained_wal_bytes")
            active = bool(slot["active"])
            inactive_s = slot.get("inactive_seconds")
            subscriber = _subscriber_id(cfg, view, instance, slot)

            too_young = (
                not active
                and inactive_s is not None
                and inactive_s < cfg.relief_min_inactive_s
            )
            if active:
                store.revoke_pending_advice(
                    slot_name, instance, round_id, now,
                    "slot active again; advice auto-revoked",
                )
                continue
            if retained is None or retained <= threshold or too_young:
                reason = (
                    "inactive window below configured minimum"
                    if too_young
                    else "WAL retention below threshold"
                )
                store.revoke_pending_advice(
                    slot_name, instance, round_id, now,
                    f"{reason}; advice auto-revoked",
                )
                continue

            prior = store.get_pending_advice(slot_name, instance)
            rechecks = (
                store.count_done_monitor_rounds_since(
                    prior["created_round_id"]
                ) + 1
                if prior is not None
                else 0
            )
            no_progress = bool(
                prior is not None
                and retained >= int(prior["wal_retention"] or 0)
            )
            if prior is not None and no_progress and (
                rechecks >= cfg.relief_recheck_rounds
            ):
                level = "recycle_candidate"
                deferred = True
                detail = (
                    f"no progress for {rechecks} recheck rounds; retained="
                    f"{retained} bytes, confirmed_lsn="
                    f"{slot.get('confirmed_lsn')}, subscriber={subscriber}. "
                    "Deferred reclaim decision only; the service never drops slots."
                )
            else:
                level = "notify_catchup"
                deferred = False
                detail = (
                    f"inactive slot retains {retained} bytes > {threshold}; "
                    f"notify subscriber {subscriber} to catch up from "
                    f"confirmed_lsn={slot.get('confirmed_lsn')}"
                )

            store.upsert_relief_advice(
                slot_name=slot_name,
                instance=instance,
                subscriber_id=subscriber,
                level=level,
                status="pending",
                restart_lsn=slot.get("restart_lsn"),
                confirmed_lsn=slot.get("confirmed_lsn"),
                wal_retention=retained,
                recheck_count=rechecks,
                deferred_reclaim=deferred,
                round_id=round_id,
                now=now,
                detail=detail,
            )
            advices.append(
                {
                    "slot_name": slot_name,
                    "instance": instance,
                    "subscriber_id": subscriber,
                    "level": level,
                    "action": (
                        "notify_subscriber"
                        if level == "notify_catchup"
                        else "defer_reclaim"
                    ),
                    "deferred_reclaim": deferred,
                    "restart_lsn": slot.get("restart_lsn"),
                    "confirmed_lsn": slot.get("confirmed_lsn"),
                    "wal_retention_bytes": retained,
                    "recheck_count": rechecks,
                }
            )

    readable_instances = {
        instance for instance, payload in view["per_instance"].items()
        if not payload.get("errors")
    }
    store.revoke_unseen_advices(seen, readable_instances, round_id, now)
    return advices
