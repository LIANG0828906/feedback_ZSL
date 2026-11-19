from __future__ import annotations

import json
import logging
import os
import sqlite3
import threading
import uuid
from dataclasses import dataclass
from typing import Any, Iterable, Sequence

from .config import TenantConfig
from .util import iso_utc, now_ms, percentile, rounded


LOGGER = logging.getLogger(__name__)
MAX_BACKOFF_MS = 60_000


SCHEMA = """
CREATE TABLE IF NOT EXISTS schema_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id TEXT PRIMARY KEY,
    priority TEXT NOT NULL,
    priority_rank INTEGER NOT NULL,
    max_concurrency INTEGER NOT NULL,
    max_retries INTEGER NOT NULL,
    backoff_base_ms INTEGER NOT NULL,
    timeout_ms INTEGER NOT NULL,
    degrade_after_timeouts INTEGER NOT NULL,
    receiver_url TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tenant_state (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(tenant_id),
    degraded INTEGER NOT NULL DEFAULT 0,
    consecutive_timeouts INTEGER NOT NULL DEFAULT 0,
    degraded_since_ms INTEGER,
    degraded_total_ms INTEGER NOT NULL DEFAULT 0,
    last_dispatch_at_ms INTEGER,
    last_scheduled_at_ms INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    event_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(tenant_id),
    payload_json TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('pending', 'in_flight', 'delivered', 'dead_letter')
    ),
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at_ms INTEGER NOT NULL,
    lease_owner TEXT,
    lease_until_ms INTEGER,
    first_dispatched_at_ms INTEGER,
    last_dispatched_at_ms INTEGER,
    delivered_at_ms INTEGER,
    permanent_failure INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_schedule
    ON events(status, tenant_id, available_at_ms, created_at_ms);
CREATE INDEX IF NOT EXISTS idx_events_tenant_status
    ON events(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_events_delivered
    ON events(status, tenant_id, delivered_at_ms);

CREATE TABLE IF NOT EXISTS dead_letters (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id TEXT NOT NULL REFERENCES tenants(tenant_id),
    event_id TEXT NOT NULL UNIQUE REFERENCES events(event_id),
    reason TEXT NOT NULL,
    permanent_failure INTEGER NOT NULL,
    attempts INTEGER NOT NULL,
    payload_json TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dead_letters_tenant
    ON dead_letters(tenant_id, id);

CREATE TABLE IF NOT EXISTS tenant_counters (
    tenant_id TEXT PRIMARY KEY REFERENCES tenants(tenant_id),
    success_total INTEGER NOT NULL DEFAULT 0,
    failure_total INTEGER NOT NULL DEFAULT 0,
    retry_total INTEGER NOT NULL DEFAULT 0,
    dead_letter_total INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO schema_meta(key, value) VALUES ('schema_version', '1');
"""


class IngestError(Exception):
    def __init__(self, code: str, message: str, event_id: str | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.event_id = event_id


@dataclass(frozen=True)
class IngestResult:
    inserted: int
    duplicates: int


@dataclass(frozen=True)
class EventRecord:
    event_id: str
    tenant_id: str
    payload_json: str
    attempts: int
    lease_owner: str
    lease_until_ms: int
    receiver_url: str
    timeout_ms: int
    max_retries: int
    backoff_base_ms: int
    degrade_after_timeouts: int


class Database:
    def __init__(self, path: str):
        self.path = path
        self._initialization_lock = threading.Lock()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path,
            timeout=15.0,
            isolation_level=None,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout = 15000")
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA synchronous = FULL")
        return connection

    @staticmethod
    def _rollback(connection: sqlite3.Connection) -> None:
        try:
            connection.execute("ROLLBACK")
        except sqlite3.Error:
            pass

    def initialize(self) -> None:
        with self._initialization_lock:
            directory = os.path.dirname(os.path.abspath(self.path))
            os.makedirs(directory, exist_ok=True)
            connection = self._connect()
            try:
                connection.execute("PRAGMA journal_mode = WAL")
                connection.executescript(SCHEMA)
            finally:
                connection.close()

    def sync_tenants(self, tenants: Sequence[TenantConfig]) -> None:
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            for tenant in tenants:
                connection.execute(
                    """
                    INSERT INTO tenants(
                        tenant_id, priority, priority_rank, max_concurrency,
                        max_retries, backoff_base_ms, timeout_ms,
                        degrade_after_timeouts, receiver_url
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(tenant_id) DO UPDATE SET
                        priority = excluded.priority,
                        priority_rank = excluded.priority_rank,
                        max_concurrency = excluded.max_concurrency,
                        max_retries = excluded.max_retries,
                        backoff_base_ms = excluded.backoff_base_ms,
                        timeout_ms = excluded.timeout_ms,
                        degrade_after_timeouts = excluded.degrade_after_timeouts,
                        receiver_url = excluded.receiver_url
                    """,
                    (
                        tenant.tenant_id,
                        tenant.priority,
                        tenant.priority_rank,
                        tenant.max_concurrency,
                        tenant.max_retries,
                        tenant.backoff_base_ms,
                        tenant.timeout_ms,
                        tenant.degrade_after_timeouts,
                        tenant.receiver_url,
                    ),
                )
                connection.execute(
                    "INSERT OR IGNORE INTO tenant_state(tenant_id) VALUES (?)",
                    (tenant.tenant_id,),
                )
                connection.execute(
                    "INSERT OR IGNORE INTO tenant_counters(tenant_id) VALUES (?)",
                    (tenant.tenant_id,),
                )
            connection.execute("COMMIT")
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()

    def ingest_events(
        self, records: Iterable[tuple[str, str, Any]]
    ) -> IngestResult:
        inserted = 0
        duplicates = 0
        created = now_ms()
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            for event_id, tenant_id, payload in records:
                tenant = connection.execute(
                    "SELECT 1 FROM tenants WHERE tenant_id = ?", (tenant_id,)
                ).fetchone()
                if tenant is None:
                    raise IngestError(
                        "EVENT_TENANT_UNKNOWN",
                        f"unknown tenant {tenant_id}",
                        event_id,
                    )
                payload_json = json.dumps(
                    payload, sort_keys=True, separators=(",", ":"), ensure_ascii=True
                )
                existing = connection.execute(
                    "SELECT tenant_id, payload_json FROM events WHERE event_id = ?",
                    (event_id,),
                ).fetchone()
                if existing is not None:
                    if (
                        existing["tenant_id"] != tenant_id
                        or existing["payload_json"] != payload_json
                    ):
                        raise IngestError(
                            "EVENT_ID_CONFLICT",
                            f"event_id {event_id} already exists with different content",
                            event_id,
                        )
                    duplicates += 1
                    continue
                connection.execute(
                    """
                    INSERT INTO events(
                        event_id, tenant_id, payload_json, status, attempts,
                        available_at_ms, created_at_ms, updated_at_ms
                    ) VALUES (?, ?, ?, 'pending', 0, ?, ?, ?)
                    """,
                    (event_id, tenant_id, payload_json, created, created, created),
                )
                inserted += 1
            connection.execute("COMMIT")
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()
        return IngestResult(inserted=inserted, duplicates=duplicates)

    def claim_next(
        self,
        worker_id: str,
        lease_ms: int,
        *,
        current_ms: int | None = None,
    ) -> EventRecord | None:
        current = current_ms if current_ms is not None else now_ms()
        claim_owner = f"{worker_id}:{uuid.uuid4().hex}"
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                """
                UPDATE events
                SET status = 'pending',
                    lease_owner = NULL,
                    lease_until_ms = NULL,
                    available_at_ms = ?,
                    last_error = COALESCE(last_error, 'lease_expired'),
                    updated_at_ms = ?
                WHERE status = 'in_flight' AND lease_until_ms <= ?
                """,
                (current, current, current),
            )

            tenant_row = connection.execute(
                """
                SELECT t.tenant_id, t.max_concurrency
                FROM tenants t
                JOIN tenant_state s ON s.tenant_id = t.tenant_id
                WHERE s.degraded = 0
                  AND (
                    SELECT COUNT(*)
                    FROM events active
                    WHERE active.tenant_id = t.tenant_id
                      AND active.status = 'in_flight'
                  ) < t.max_concurrency
                  AND EXISTS (
                    SELECT 1
                    FROM events waiting
                    WHERE waiting.tenant_id = t.tenant_id
                      AND waiting.status = 'pending'
                      AND waiting.available_at_ms <= ?
                  )
                ORDER BY
                    t.priority_rank ASC,
                    (
                      SELECT COUNT(*)
                      FROM events active
                      WHERE active.tenant_id = t.tenant_id
                        AND active.status = 'in_flight'
                    ) * 1.0 / t.max_concurrency ASC,
                    s.last_scheduled_at_ms ASC,
                    t.tenant_id ASC
                LIMIT 1
                """,
                (current,),
            ).fetchone()
            if tenant_row is None:
                connection.execute("COMMIT")
                return None

            tenant_id = tenant_row["tenant_id"]
            event_row = connection.execute(
                """
                SELECT event_id, attempts
                FROM events
                WHERE tenant_id = ?
                  AND status = 'pending'
                  AND available_at_ms <= ?
                ORDER BY created_at_ms ASC, event_id ASC
                LIMIT 1
                """,
                (tenant_id, current),
            ).fetchone()
            if event_row is None:
                connection.execute("COMMIT")
                return None

            lease_until = current + max(1, lease_ms)
            updated = connection.execute(
                """
                UPDATE events
                SET status = 'in_flight',
                    lease_owner = ?,
                    lease_until_ms = ?,
                    first_dispatched_at_ms = COALESCE(first_dispatched_at_ms, ?),
                    last_dispatched_at_ms = ?,
                    updated_at_ms = ?
                WHERE event_id = ? AND status = 'pending'
                """,
                (
                    claim_owner,
                    lease_until,
                    current,
                    current,
                    current,
                    event_row["event_id"],
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("claimed event changed unexpectedly")
            connection.execute(
                "UPDATE tenant_state SET last_scheduled_at_ms = ? WHERE tenant_id = ?",
                (current, tenant_id),
            )
            claimed = connection.execute(
                """
                SELECT e.*, t.receiver_url, t.timeout_ms, t.max_retries,
                       t.backoff_base_ms, t.degrade_after_timeouts
                FROM events e
                JOIN tenants t ON t.tenant_id = e.tenant_id
                WHERE e.event_id = ?
                """,
                (event_row["event_id"],),
            ).fetchone()
            connection.execute("COMMIT")
            return EventRecord(
                event_id=claimed["event_id"],
                tenant_id=claimed["tenant_id"],
                payload_json=claimed["payload_json"],
                attempts=claimed["attempts"],
                lease_owner=claim_owner,
                lease_until_ms=lease_until,
                receiver_url=claimed["receiver_url"],
                timeout_ms=claimed["timeout_ms"],
                max_retries=claimed["max_retries"],
                backoff_base_ms=claimed["backoff_base_ms"],
                degrade_after_timeouts=claimed["degrade_after_timeouts"],
            )
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()

    def complete_success(self, claim: EventRecord) -> bool:
        current = now_ms()
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            updated = connection.execute(
                """
                UPDATE events
                SET status = 'delivered',
                    attempts = attempts + 1,
                    delivered_at_ms = ?,
                    lease_owner = NULL,
                    lease_until_ms = NULL,
                    permanent_failure = 0,
                    last_error = NULL,
                    updated_at_ms = ?
                WHERE event_id = ? AND status = 'in_flight' AND lease_owner = ?
                """,
                (current, current, claim.event_id, claim.lease_owner),
            )
            if updated.rowcount != 1:
                connection.execute("COMMIT")
                return False
            connection.execute(
                """
                UPDATE tenant_counters
                SET success_total = success_total + 1
                WHERE tenant_id = ?
                """,
                (claim.tenant_id,),
            )
            connection.execute(
                """
                UPDATE tenant_state
                SET consecutive_timeouts = 0, last_dispatch_at_ms = ?
                WHERE tenant_id = ?
                """,
                (current, claim.tenant_id),
            )
            connection.execute("COMMIT")
            return True
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()

    def complete_failure(
        self,
        claim: EventRecord,
        *,
        kind: str,
        message: str,
        timed_out: bool,
        permanent: bool,
    ) -> str | None:
        current = now_ms()
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            event = connection.execute(
                """
                SELECT e.*, t.max_retries, t.backoff_base_ms,
                       t.degrade_after_timeouts
                FROM events e
                JOIN tenants t ON t.tenant_id = e.tenant_id
                WHERE e.event_id = ?
                """,
                (claim.event_id,),
            ).fetchone()
            if (
                event is None
                or event["status"] != "in_flight"
                or event["lease_owner"] != claim.lease_owner
            ):
                connection.execute("COMMIT")
                return None

            attempts = int(event["attempts"]) + 1
            connection.execute(
                """
                UPDATE tenant_counters
                SET failure_total = failure_total + 1
                WHERE tenant_id = ?
                """,
                (claim.tenant_id,),
            )
            reason = f"{kind}: {message}"[:1000]
            if timed_out:
                state = connection.execute(
                    """
                    SELECT degraded, consecutive_timeouts, degraded_since_ms
                    FROM tenant_state WHERE tenant_id = ?
                    """,
                    (claim.tenant_id,),
                ).fetchone()
                consecutive = int(state["consecutive_timeouts"]) + 1
                should_degrade = (
                    not bool(state["degraded"])
                    and consecutive > int(event["degrade_after_timeouts"])
                )
                connection.execute(
                    """
                    UPDATE tenant_state
                    SET consecutive_timeouts = ?,
                        degraded = CASE WHEN ? THEN 1 ELSE degraded END,
                        degraded_since_ms = CASE
                            WHEN ? THEN ? ELSE degraded_since_ms
                        END
                    WHERE tenant_id = ?
                    """,
                    (
                        consecutive,
                        int(should_degrade),
                        int(should_degrade),
                        current,
                        claim.tenant_id,
                    ),
                )
                if should_degrade:
                    LOGGER.warning(
                        "tenant %s degraded after %s consecutive timeouts",
                        claim.tenant_id,
                        consecutive,
                    )
            else:
                connection.execute(
                    """
                    UPDATE tenant_state
                    SET consecutive_timeouts = 0
                    WHERE tenant_id = ?
                    """,
                    (claim.tenant_id,),
                )

            exhausted = permanent or attempts > int(event["max_retries"])
            if exhausted:
                permanent_value = 1 if permanent else 0
                connection.execute(
                    """
                    INSERT OR IGNORE INTO dead_letters(
                        tenant_id, event_id, reason, permanent_failure,
                        attempts, payload_json, created_at_ms
                    ) VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        claim.tenant_id,
                        claim.event_id,
                        reason,
                        permanent_value,
                        attempts,
                        event["payload_json"],
                        current,
                    ),
                )
                connection.execute(
                    """
                    UPDATE events
                    SET status = 'dead_letter',
                        attempts = ?,
                        permanent_failure = ?,
                        lease_owner = NULL,
                        lease_until_ms = NULL,
                        last_error = ?,
                        updated_at_ms = ?
                    WHERE event_id = ? AND status = 'in_flight' AND lease_owner = ?
                    """,
                    (
                        attempts,
                        permanent_value,
                        reason,
                        current,
                        claim.event_id,
                        claim.lease_owner,
                    ),
                )
                connection.execute(
                    """
                    UPDATE tenant_counters
                    SET dead_letter_total = dead_letter_total + 1
                    WHERE tenant_id = ?
                    """,
                    (claim.tenant_id,),
                )
                action = "dead_letter"
            else:
                base_delay = int(event["backoff_base_ms"])
                exponent = min(30, max(0, attempts - 1))
                delay = (
                    min(MAX_BACKOFF_MS, base_delay * (1 << exponent))
                    if base_delay > 0
                    else 0
                )
                connection.execute(
                    """
                    UPDATE events
                    SET status = 'pending',
                        attempts = ?,
                        available_at_ms = ?,
                        lease_owner = NULL,
                        lease_until_ms = NULL,
                        last_error = ?,
                        updated_at_ms = ?
                    WHERE event_id = ? AND status = 'in_flight' AND lease_owner = ?
                    """,
                    (
                        attempts,
                        current + delay,
                        reason,
                        current,
                        claim.event_id,
                        claim.lease_owner,
                    ),
                )
                connection.execute(
                    """
                    UPDATE tenant_counters
                    SET retry_total = retry_total + 1
                    WHERE tenant_id = ?
                    """,
                    (claim.tenant_id,),
                )
                action = "retry"
            connection.execute(
                """
                UPDATE tenant_state SET last_dispatch_at_ms = ?
                WHERE tenant_id = ?
                """,
                (current, claim.tenant_id),
            )
            connection.execute("COMMIT")
            return action
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()

    def degraded_tenants(self) -> list[dict[str, Any]]:
        connection = self._connect()
        try:
            rows = connection.execute(
                """
                SELECT t.tenant_id, t.receiver_url, t.timeout_ms,
                       s.degraded_since_ms
                FROM tenants t
                JOIN tenant_state s ON s.tenant_id = t.tenant_id
                WHERE s.degraded = 1
                ORDER BY t.tenant_id
                """
            ).fetchall()
            return [dict(row) for row in rows]
        finally:
            connection.close()

    def recover_tenant(self, tenant_id: str) -> bool:
        current = now_ms()
        connection = self._connect()
        try:
            connection.execute("BEGIN IMMEDIATE")
            state = connection.execute(
                """
                SELECT degraded, degraded_since_ms
                FROM tenant_state WHERE tenant_id = ?
                """,
                (tenant_id,),
            ).fetchone()
            if state is None or not bool(state["degraded"]):
                connection.execute("COMMIT")
                return False
            started = state["degraded_since_ms"]
            duration = max(0, current - started) if started is not None else 0
            connection.execute(
                """
                UPDATE tenant_state
                SET degraded = 0,
                    consecutive_timeouts = 0,
                    degraded_since_ms = NULL,
                    degraded_total_ms = degraded_total_ms + ?
                WHERE tenant_id = ? AND degraded = 1
                """,
                (duration, tenant_id),
            )
            connection.execute("COMMIT")
            LOGGER.info("tenant %s recovered after %sms degraded", tenant_id, duration)
            return True
        except Exception:
            self._rollback(connection)
            raise
        finally:
            connection.close()

    def tenant_config(self, tenant_id: str) -> dict[str, Any] | None:
        connection = self._connect()
        try:
            row = connection.execute(
                "SELECT * FROM tenants WHERE tenant_id = ?", (tenant_id,)
            ).fetchone()
            return dict(row) if row else None
        finally:
            connection.close()

    def status(self) -> dict[str, Any]:
        connection = self._connect()
        try:
            current = now_ms()
            rows = connection.execute(
                """
                SELECT t.tenant_id, t.priority, t.max_concurrency,
                       s.degraded, s.consecutive_timeouts,
                       s.degraded_since_ms, s.degraded_total_ms,
                       s.last_dispatch_at_ms,
                       (SELECT COUNT(*) FROM events e
                        WHERE e.tenant_id = t.tenant_id
                          AND e.status = 'pending') AS queue_depth,
                       (SELECT COUNT(*) FROM events e
                        WHERE e.tenant_id = t.tenant_id
                          AND e.status = 'in_flight') AS inflight
                FROM tenants t
                JOIN tenant_state s ON s.tenant_id = t.tenant_id
                ORDER BY t.tenant_id
                """
            ).fetchall()
            tenants: list[dict[str, Any]] = []
            for row in rows:
                current_degraded_ms = 0
                if row["degraded"] and row["degraded_since_ms"] is not None:
                    current_degraded_ms = max(
                        0, current - int(row["degraded_since_ms"])
                    )
                tenants.append(
                    {
                        "tenant_id": row["tenant_id"],
                        "priority": row["priority"],
                        "queue_depth": int(row["queue_depth"]),
                        "degraded_queue_depth": (
                            int(row["queue_depth"])
                            if bool(row["degraded"])
                            else 0
                        ),
                        "inflight": int(row["inflight"]),
                        "max_concurrency": int(row["max_concurrency"]),
                        "degraded": bool(row["degraded"]),
                        "consecutive_timeouts": int(
                            row["consecutive_timeouts"]
                        ),
                        "degraded_duration_ms": int(row["degraded_total_ms"])
                        + current_degraded_ms,
                        "last_dispatch_at": iso_utc(row["last_dispatch_at_ms"]),
                    }
                )
            return {
                "tenant_count": len(tenants),
                "degraded_count": sum(1 for item in tenants if item["degraded"]),
                "tenants": tenants,
            }
        finally:
            connection.close()

    def report(self) -> dict[str, Any]:
        connection = self._connect()
        try:
            current = now_ms()
            rows = connection.execute(
                """
                SELECT t.tenant_id, t.priority,
                       s.degraded, s.degraded_since_ms, s.degraded_total_ms,
                       COUNT(e.event_id) AS events_submitted,
                       SUM(CASE WHEN e.status = 'delivered' THEN 1 ELSE 0 END)
                           AS delivered,
                       SUM(CASE WHEN e.status = 'dead_letter' THEN 1 ELSE 0 END)
                           AS dead_letters,
                       SUM(CASE WHEN e.status = 'pending' THEN 1 ELSE 0 END)
                           AS queue_depth,
                       SUM(CASE WHEN e.status = 'in_flight' THEN 1 ELSE 0 END)
                           AS inflight,
                       SUM(CASE WHEN e.permanent_failure = 1 THEN 1 ELSE 0 END)
                           AS permanent_failures,
                       COALESCE(SUM(
                           CASE WHEN e.attempts > 0 THEN e.attempts - 1 ELSE 0 END
                       ), 0) AS retries
                FROM tenants t
                JOIN tenant_state s ON s.tenant_id = t.tenant_id
                LEFT JOIN events e ON e.tenant_id = t.tenant_id
                GROUP BY t.tenant_id
                ORDER BY t.tenant_id
                """
            ).fetchall()
            latency_rows = connection.execute(
                """
                SELECT tenant_id, CASE
                    WHEN delivered_at_ms IS NOT NULL AND delivered_at_ms >= created_at_ms
                    THEN delivered_at_ms - created_at_ms
                    ELSE 0
                END AS latency_ms
                FROM events
                WHERE status = 'delivered'
                ORDER BY tenant_id, event_id
                """
            ).fetchall()
            latencies: dict[str, list[int]] = {}
            all_latencies: list[int] = []
            for latency_row in latency_rows:
                value = int(latency_row["latency_ms"])
                latencies.setdefault(latency_row["tenant_id"], []).append(value)
                all_latencies.append(value)

            tenants: list[dict[str, Any]] = []
            for row in rows:
                tenant_latencies = latencies.get(row["tenant_id"], [])
                submitted = int(row["events_submitted"] or 0)
                delivered = int(row["delivered"] or 0)
                current_degraded_ms = 0
                if row["degraded"] and row["degraded_since_ms"] is not None:
                    current_degraded_ms = max(
                        0, current - int(row["degraded_since_ms"])
                    )
                average = (
                    sum(tenant_latencies) / len(tenant_latencies)
                    if tenant_latencies
                    else 0.0
                )
                tenants.append(
                    {
                        "tenant_id": row["tenant_id"],
                        "priority": row["priority"],
                        "events_submitted": submitted,
                        "delivered": delivered,
                        "dead_letters": int(row["dead_letters"] or 0),
                        "permanent_failures": int(
                            row["permanent_failures"] or 0
                        ),
                        "queue_depth": int(row["queue_depth"] or 0),
                        "inflight": int(row["inflight"] or 0),
                        "success_rate": rounded(
                            delivered / submitted if submitted else 0.0, 6
                        ),
                        "avg_latency_ms": rounded(average),
                        "p95_latency_ms": rounded(
                            percentile(tenant_latencies, 0.95)
                        ),
                        "retries": int(row["retries"] or 0),
                        "degraded": bool(row["degraded"]),
                        "degradation_ms": int(row["degraded_total_ms"])
                        + current_degraded_ms,
                    }
                )

            total_submitted = sum(item["events_submitted"] for item in tenants)
            total_delivered = sum(item["delivered"] for item in tenants)
            total_dead = sum(item["dead_letters"] for item in tenants)
            total_retries = sum(item["retries"] for item in tenants)
            total_degradation = sum(item["degradation_ms"] for item in tenants)
            total_permanent = sum(
                item["permanent_failures"] for item in tenants
            )
            average_all = (
                sum(all_latencies) / len(all_latencies) if all_latencies else 0.0
            )
            return {
                "tenants": tenants,
                "totals": {
                    "tenant_count": len(tenants),
                    "events_submitted": total_submitted,
                    "delivered": total_delivered,
                    "dead_letters": total_dead,
                    "permanent_failures": total_permanent,
                    "queue_depth": sum(
                        item["queue_depth"] for item in tenants
                    ),
                    "inflight": sum(item["inflight"] for item in tenants),
                    "success_rate": rounded(
                        total_delivered / total_submitted
                        if total_submitted
                        else 0.0,
                        6,
                    ),
                    "avg_latency_ms": rounded(average_all),
                    "p95_latency_ms": rounded(
                        percentile(all_latencies, 0.95)
                    ),
                    "retries": total_retries,
                    "degraded_tenants": sum(
                        1 for item in tenants if item["degraded"]
                    ),
                    "degradation_ms": total_degradation,
                },
            }
        finally:
            connection.close()

    def dead_letters(self, limit: int = 100) -> list[dict[str, Any]]:
        connection = self._connect()
        try:
            rows = connection.execute(
                """
                SELECT event_id, tenant_id, reason, permanent_failure,
                       attempts, created_at_ms
                FROM dead_letters
                ORDER BY tenant_id, id
                LIMIT ?
                """,
                (limit,),
            ).fetchall()
            return [
                {
                    "event_id": row["event_id"],
                    "tenant_id": row["tenant_id"],
                    "reason": row["reason"],
                    "permanent_failure": bool(row["permanent_failure"]),
                    "attempts": int(row["attempts"]),
                    "created_at": iso_utc(row["created_at_ms"]),
                }
                for row in rows
            ]
        finally:
            connection.close()

    def event_status(self, event_id: str) -> dict[str, Any] | None:
        connection = self._connect()
        try:
            row = connection.execute(
                """
                SELECT event_id, tenant_id, status, attempts, permanent_failure,
                       last_error, created_at_ms, first_dispatched_at_ms,
                       delivered_at_ms, updated_at_ms
                FROM events WHERE event_id = ?
                """,
                (event_id,),
            ).fetchone()
            if row is None:
                return None
            return {
                "event_id": row["event_id"],
                "tenant_id": row["tenant_id"],
                "status": row["status"],
                "attempts": int(row["attempts"]),
                "permanent_failure": bool(row["permanent_failure"]),
                "last_error": row["last_error"],
                "created_at": iso_utc(row["created_at_ms"]),
                "first_dispatched_at": iso_utc(
                    row["first_dispatched_at_ms"]
                ),
                "delivered_at": iso_utc(row["delivered_at_ms"]),
                "updated_at": iso_utc(row["updated_at_ms"]),
            }
        finally:
            connection.close()

    def metrics_rows(self) -> list[dict[str, Any]]:
        connection = self._connect()
        try:
            rows = connection.execute(
                """
                SELECT t.tenant_id, s.degraded,
                       c.success_total, c.failure_total,
                       c.retry_total, c.dead_letter_total,
                       (SELECT COUNT(*) FROM events e
                        WHERE e.tenant_id = t.tenant_id
                          AND e.status = 'in_flight') AS inflight,
                       (SELECT COUNT(*) FROM events e
                        WHERE e.tenant_id = t.tenant_id
                          AND e.status = 'pending') AS queue_depth
                FROM tenants t
                JOIN tenant_state s ON s.tenant_id = t.tenant_id
                JOIN tenant_counters c ON c.tenant_id = t.tenant_id
                ORDER BY t.tenant_id
                """
            ).fetchall()
            return [dict(row) for row in rows]
        finally:
            connection.close()

    def health(self) -> bool:
        connection = self._connect()
        try:
            connection.execute("SELECT 1").fetchone()
            return True
        finally:
            connection.close()
