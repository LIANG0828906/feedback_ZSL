"""Reconcile Redis consumer positions with the replica's applied LSN."""
from __future__ import annotations

import json
import time
from typing import Any

import redis

from config import Config
from db import SlotGuardError, connect, lsn_int, lsn_text
from store import Store


def _decode_position(raw: bytes | None) -> tuple[str | None, str | None]:
    """Accept a plain LSN or a JSON object containing an ``lsn`` field."""
    if raw is None:
        return None, None
    text = raw.decode("utf-8", errors="replace").strip()
    if not text:
        return None, None
    if text.startswith("{"):
        try:
            payload = json.loads(text)
            value = payload.get("lsn") or payload.get("position")
            return (str(value), "json") if value is not None else (None, "json")
        except (ValueError, TypeError):
            return None, "json-invalid"
    return text, "plain"


class OffsetReconciler:
    def __init__(self, cfg: Config, store: Store):
        self.cfg = cfg
        self.store = store
        self.redis = redis.Redis.from_url(
            cfg.redis_url,
            socket_connect_timeout=cfg.connect_timeout_s,
            socket_timeout=cfg.connect_timeout_s,
            decode_responses=False,
        )

    def close(self) -> None:
        self.redis.close()

    def offset_key(self) -> str:
        return f"{self.cfg.redis_key_prefix}offset:{self.cfg.slot_name}"

    def read_applied_lsn(self) -> str:
        """Return the subscription origin's remote LSN (actual apply point)."""
        with connect(
            self.cfg.replica_dsn, self.cfg.connect_timeout_s, autocommit=True
        ) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT s.oid::text, r.remote_lsn::text "
                "FROM pg_subscription s "
                "LEFT JOIN pg_replication_origin_status r "
                "       ON r.external_id = 'pg_' || s.oid::text "
                "WHERE s.subname=%s",
                (self.cfg.subscription_name,),
            )
            row = cur.fetchone()
            if row is None:
                raise SlotGuardError(
                    "SG-E2001", "replica", "pg_subscription",
                    f"subscription {self.cfg.subscription_name!r} does not exist",
                )
            if row[1] is None:
                raise SlotGuardError(
                    "SG-E2002", "replica", "pg_replication_origin_status",
                    f"subscription {self.cfg.subscription_name!r} has no "
                    "replication origin status yet",
                )
            return row[1]

    def reconcile(self, round_id: str) -> dict[str, Any]:
        applied_text = self.read_applied_lsn()
        applied = lsn_int(applied_text)
        if applied is None:
            raise SlotGuardError(
                "SG-E2003", "replica", "pg_replication_origin_status",
                f"invalid applied LSN {applied_text!r}",
            )

        key = self.offset_key()
        raw = self.redis.get(key)
        redis_text, encoding = _decode_position(raw)
        try:
            redis_lsn = lsn_int(redis_text) if redis_text else None
        except (TypeError, ValueError):
            redis_lsn = None

        if raw is None:
            verdict = "missing"
            source = "replica_applied_lsn"
            basis = "Redis key is absent; initialize from replica apply origin"
        elif redis_lsn is None:
            verdict = "invalid_rejected"
            source = "replica_applied_lsn"
            basis = "cached value is not a valid PostgreSQL LSN; rejected"
        elif redis_lsn < applied:
            verdict = "behind"
            source = "replica_applied_lsn"
            basis = "cached LSN is behind the replica apply origin"
        elif redis_lsn > applied:
            verdict = "ahead_rejected"
            source = "replica_applied_lsn"
            basis = (
                "cached LSN is ahead of the replica apply origin and must not "
                "be adopted"
            )
        else:
            verdict = "equal"
            source = "redis_and_replica"
            basis = "cached LSN equals the replica apply origin"

        corrected_text = applied_text
        if verdict != "equal":
            self.redis.set(key, corrected_text)
        self.store.inc_counter(
            "offset_corrections_total" if verdict != "equal" else
            "offset_checks_equal"
        )
        self.store.insert_offset_correction(
            {
                "round_id": round_id,
                "slot_name": self.cfg.slot_name,
                "redis_lsn": redis_text,
                "applied_lsn": applied_text,
                "verdict": verdict,
                "source_of_truth": source,
                "corrected_lsn": corrected_text,
                "basis": basis,
                "created_at": time.time(),
            }
        )
        return {
            "slot_name": self.cfg.slot_name,
            "redis_key": key,
            "redis_lsn": redis_text,
            "redis_encoding": encoding,
            "applied_lsn": applied_text,
            "verdict": verdict,
            "source_of_truth": source,
            "basis": basis,
            "corrected_lsn": corrected_text,
            "reused_position": corrected_text,
        }

    def tamper(self, lsn: str | None = None, delta: int = 0x4000000) -> str:
        key = self.offset_key()
        if lsn is None:
            raw = self.redis.get(key)
            current_text, _ = _decode_position(raw)
            current = lsn_int(current_text) if current_text else 0
            lsn = lsn_text(max(0, (current or 0) + delta))
        self.redis.set(key, lsn)
        return lsn

    def delete_offset(self) -> None:
        # This helper is only used by fault injection.  It deletes exactly one
        # key: the configured consumer-position key.
        self.redis.delete(self.offset_key())
