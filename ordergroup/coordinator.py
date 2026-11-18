from __future__ import annotations

import json
import random
import threading
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Iterable

from .backends import BackendError, Record
from .config import Config
from .errors import (
    INVALID_REQUEST,
    KAFKA_UNAVAILABLE,
    MEMBER_NOT_ACTIVE,
    OFFSET_REGRESSION,
    PARTITION_NOT_OWNED,
    PARTITION_PAUSED,
    REBALANCE_IN_PROGRESS,
    STALE_GENERATION,
    UNKNOWN_MEMBER,
    api_error,
)
from .storage import Store


ACTIVE = "active"
TIMED_OUT = "timed_out"
LEFT = "left"


def _now_ms() -> int:
    return time.time_ns() // 1_000_000


def _json_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


@dataclass
class Member:
    member_id: str
    session_timeout_ms: int
    heartbeat_interval_ms: int
    status: str
    joined_at_ms: int
    last_heartbeat_ms: int
    removed_at_ms: int | None = None
    removal_reason: str | None = None
    incarnation: int = 1

    def as_dict(self) -> dict[str, Any]:
        return {
            "member_id": self.member_id,
            "session_timeout_ms": self.session_timeout_ms,
            "heartbeat_interval_ms": self.heartbeat_interval_ms,
            "status": self.status,
            "active": self.status == ACTIVE,
            "joined_at_ms": self.joined_at_ms,
            "last_heartbeat_ms": self.last_heartbeat_ms,
            "removed_at_ms": self.removed_at_ms,
            "removal_reason": self.removal_reason,
            "incarnation": self.incarnation,
        }


@dataclass
class Assignment:
    partition: int
    member_id: str
    generation: int
    assigned_at_ms: int
    state: str = ACTIVE
    paused: bool = False
    paused_at_ms: int | None = None

    def as_dict(self) -> dict[str, Any]:
        return {
            "partition": self.partition,
            "member_id": self.member_id,
            "generation": self.generation,
            "assigned_at_ms": self.assigned_at_ms,
            "state": self.state,
            "paused": self.paused,
            "paused_at_ms": self.paused_at_ms,
        }


@dataclass
class CommitRecord:
    partition: int
    offset: int
    member_id: str
    generation: int
    updated_at_ms: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "partition": self.partition,
            "committed_offset": self.offset,
            "member_id": self.member_id,
            "generation": self.generation,
            "updated_at_ms": self.updated_at_ms,
        }


@dataclass
class QuarantineRecord:
    partition: int
    former_member_id: str | None
    reason: str
    since_ms: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "partition": self.partition,
            "former_member_id": self.former_member_id,
            "reason": self.reason,
            "since_ms": self.since_ms,
        }


@dataclass
class PendingRebalance:
    id: str
    reason: str
    trigger_member: str | None
    involved: set[int]
    requested_at_ms: int
    scheduled_at_ms: int
    suppression_reason: str | None = None
    status: str = "scheduled"
    deferred: bool = False

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "reason": self.reason,
            "trigger_member": self.trigger_member,
            "involved_partitions": sorted(self.involved),
            "requested_at_ms": self.requested_at_ms,
            "scheduled_at_ms": self.scheduled_at_ms,
            "suppression_reason": self.suppression_reason,
            "status": self.status,
            "deferred": self.deferred,
        }


@dataclass
class InProgressRebalance:
    id: str
    reason: str
    target: dict[int, str]
    revoking: dict[str, set[int]]
    acked: dict[str, set[int]] = field(default_factory=dict)
    started_at_ms: int = 0
    deadline_ms: int = 0
    generation: int = 0
    involved: set[int] = field(default_factory=set)
    trigger_member: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "reason": self.reason,
            "involved_partitions": sorted(self.involved),
            "revoking": {
                member: sorted(partitions)
                for member, partitions in sorted(self.revoking.items())
            },
            "acked": {
                member: sorted(partitions)
                for member, partitions in sorted(self.acked.items())
            },
            "started_at_ms": self.started_at_ms,
            "deadline_ms": self.deadline_ms,
            "generation": self.generation,
            "trigger_member": self.trigger_member,
        }


class Coordinator:
    def __init__(self, config: Config, backend: Any) -> None:
        self.config = config
        self.backend = backend
        self.store = Store(config.db_path)
        self.topic = config.topic
        self.partition_count = config.partitions
        self.group_id = str(
            config.get("group", "group_id", "order-events-consumers")
        )
        self.lock = threading.RLock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._started_at_ms = _now_ms()
        self.members: dict[str, Member] = {}
        self.assignments: dict[int, Assignment] = {}
        self.commits: dict[int, CommitRecord] = {}
        self.quarantine: dict[int, QuarantineRecord] = {}
        self.pending: PendingRebalance | None = None
        self.in_progress: InProgressRebalance | None = None
        # A delivered batch is not committed yet. Rebalance finalization is
        # blocked until the old owner commits or explicitly releases it.
        self.inflight: dict[int, tuple[str, int]] = {}
        self.generation = 0
        self.last_rebalance_completed_ms: int | None = None
        self.counters: dict[tuple[str, str, tuple[tuple[str, str], ...]], int] = {}
        self._end_offsets_cache: dict[int, int] = {}
        self._end_offsets_at_ms = 0
        self._round_robin_partition = 0
        self._load()
        for partition in range(self.partition_count):
            self._end_offsets_cache.setdefault(partition, 0)

    # ------------------------------------------------------------------
    # Lifecycle and persistence
    # ------------------------------------------------------------------
    def start(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._maintenance_loop,
            name="coordinator-maintenance",
            daemon=True,
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        thread = self._thread
        if thread and thread.is_alive():
            thread.join(timeout=2.0)
        self._thread = None
        self.store.close()

    def _maintenance_loop(self) -> None:
        interval = max(
            0.01, int(self.config.get("group", "sweep_interval_ms", 50)) / 1000.0
        )
        while not self._stop.wait(interval):
            try:
                with self.lock:
                    self._tick_locked(_now_ms())
            except Exception:
                # A maintenance failure must not kill timeout enforcement.
                continue

    def _load(self) -> None:
        for row in self.store.fetchall("SELECT * FROM members"):
            self.members[row["member_id"]] = Member(
                member_id=row["member_id"],
                session_timeout_ms=row["session_timeout_ms"],
                heartbeat_interval_ms=row["heartbeat_interval_ms"],
                status=row["status"],
                joined_at_ms=row["joined_at_ms"],
                last_heartbeat_ms=row["last_heartbeat_ms"],
                removed_at_ms=row["removed_at_ms"],
                removal_reason=row["removal_reason"],
                incarnation=row["incarnation"],
            )
        for row in self.store.fetchall("SELECT * FROM assignments"):
            state = row["state"]
            if state == "revoking":
                state = ACTIVE
            assignment = Assignment(
                partition=row["partition"],
                member_id=row["member_id"],
                generation=row["generation"],
                assigned_at_ms=row["assigned_at_ms"],
                state=state,
                paused=bool(row["paused"]),
                paused_at_ms=row["paused_at_ms"],
            )
            self.assignments[assignment.partition] = assignment
        self.store.execute("UPDATE assignments SET state = 'active' WHERE state = 'revoking'")
        self.store.execute(
            "UPDATE rebalances SET status = 'aborted' WHERE status IN ('scheduled', 'suppressed', 'revoking', 'queued')"
        )
        for row in self.store.fetchall("SELECT * FROM commits"):
            self.commits[row["partition"]] = CommitRecord(
                partition=row["partition"],
                offset=row["offset"],
                member_id=row["member_id"],
                generation=row["generation"],
                updated_at_ms=row["updated_at_ms"],
            )
        for row in self.store.fetchall("SELECT * FROM quarantine"):
            self.quarantine[row["partition"]] = QuarantineRecord(
                partition=row["partition"],
                former_member_id=row["former_member_id"],
                reason=row["reason"],
                since_ms=row["since_ms"],
            )
        generation = self.store.get_metadata("generation")
        if generation is not None:
            self.generation = int(generation)
        completed = self.store.get_metadata("last_rebalance_completed_ms")
        if completed is not None:
            self.last_rebalance_completed_ms = int(completed)
        for row in self.store.fetchall("SELECT * FROM counters"):
            try:
                labels = tuple(
                    sorted(json.loads(row["labels_json"]).items())
                )
            except (json.JSONDecodeError, AttributeError):
                labels = ()
            self.counters[(row["scope"], row["name"], labels)] = int(row["value"])

    def _persist_member(self, member: Member) -> None:
        self.store.execute(
            """
            INSERT INTO members(
                member_id, session_timeout_ms, heartbeat_interval_ms, status,
                joined_at_ms, last_heartbeat_ms, removed_at_ms, removal_reason,
                incarnation
            ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(member_id) DO UPDATE SET
                session_timeout_ms = excluded.session_timeout_ms,
                heartbeat_interval_ms = excluded.heartbeat_interval_ms,
                status = excluded.status,
                joined_at_ms = excluded.joined_at_ms,
                last_heartbeat_ms = excluded.last_heartbeat_ms,
                removed_at_ms = excluded.removed_at_ms,
                removal_reason = excluded.removal_reason,
                incarnation = excluded.incarnation
            """,
            (
                member.member_id,
                member.session_timeout_ms,
                member.heartbeat_interval_ms,
                member.status,
                member.joined_at_ms,
                member.last_heartbeat_ms,
                member.removed_at_ms,
                member.removal_reason,
                member.incarnation,
            ),
        )

    def _persist_assignment(self, assignment: Assignment) -> None:
        self.store.execute(
            """
            INSERT INTO assignments(
                partition, member_id, generation, assigned_at_ms, state,
                paused, paused_at_ms
            ) VALUES(?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(partition) DO UPDATE SET
                member_id = excluded.member_id,
                generation = excluded.generation,
                assigned_at_ms = excluded.assigned_at_ms,
                state = excluded.state,
                paused = excluded.paused,
                paused_at_ms = excluded.paused_at_ms
            """,
            (
                assignment.partition,
                assignment.member_id,
                assignment.generation,
                assignment.assigned_at_ms,
                assignment.state,
                int(assignment.paused),
                assignment.paused_at_ms,
            ),
        )

    def _persist_commit(self, commit: CommitRecord) -> None:
        self.store.execute(
            """
            INSERT INTO commits(partition, offset, member_id, generation, updated_at_ms)
            VALUES(?, ?, ?, ?, ?)
            ON CONFLICT(partition) DO UPDATE SET
                offset = excluded.offset,
                member_id = excluded.member_id,
                generation = excluded.generation,
                updated_at_ms = excluded.updated_at_ms
            """,
            (
                commit.partition,
                commit.offset,
                commit.member_id,
                commit.generation,
                commit.updated_at_ms,
            ),
        )

    def _set_quarantine(
        self,
        partition: int,
        former_member_id: str | None,
        reason: str,
        now: int,
    ) -> None:
        record = QuarantineRecord(
            partition=partition,
            former_member_id=former_member_id,
            reason=reason,
            since_ms=now,
        )
        self.quarantine[partition] = record
        self.store.execute(
            """
            INSERT INTO quarantine(partition, former_member_id, reason, since_ms)
            VALUES(?, ?, ?, ?)
            ON CONFLICT(partition) DO UPDATE SET
                former_member_id = excluded.former_member_id,
                reason = excluded.reason,
                since_ms = excluded.since_ms
            """,
            (partition, former_member_id, reason, now),
        )

    def _clear_quarantine(self, partition: int) -> None:
        self.quarantine.pop(partition, None)
        self.store.execute("DELETE FROM quarantine WHERE partition = ?", (partition,))

    def _increment(
        self,
        scope: str,
        name: str,
        labels: dict[str, str] | None = None,
        amount: int = 1,
    ) -> None:
        label_items = tuple(sorted((labels or {}).items()))
        key = (scope, name, label_items)
        self.counters[key] = self.counters.get(key, 0) + amount
        label_json = _json_dumps(dict(label_items))
        self.store.execute(
            """
            INSERT INTO counters(scope, name, labels_json, value)
            VALUES(?, ?, ?, ?)
            ON CONFLICT(scope, name, labels_json) DO UPDATE SET
                value = counters.value + excluded.value
            """,
            (scope, name, label_json, amount),
        )

    def _counter(
        self, scope: str, name: str, labels: dict[str, str] | None = None
    ) -> int:
        label_items = tuple(sorted((labels or {}).items()))
        return self.counters.get((scope, name, label_items), 0)

    def _event(
        self,
        kind: str,
        now: int,
        member_id: str | None = None,
        partition: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        self.store.append_event(
            kind=kind,
            at_ms=now,
            member_id=member_id,
            partition=partition,
            details=details,
        )

    # ------------------------------------------------------------------
    # Membership
    # ------------------------------------------------------------------
    def join(self, payload: dict[str, Any]) -> dict[str, Any]:
        member_id = payload.get(
            "member_id", payload.get("memberId", payload.get("member.id"))
        )
        if not isinstance(member_id, str) or not member_id.strip():
            raise api_error(INVALID_REQUEST, "member_id must be a non-empty string")
        if len(member_id) > 200:
            raise api_error(INVALID_REQUEST, "member_id is too long")
        try:
            session_timeout_ms = int(
                payload.get(
                    "session_timeout_ms",
                    payload.get(
                        "session.timeout.ms",
                        payload.get(
                            "sessionTimeoutMs",
                            self.config.get(
                                "group", "default_session_timeout_ms", 10000
                            ),
                        ),
                    ),
                )
            )
            heartbeat_interval_ms = int(
                payload.get(
                    "heartbeat_interval_ms",
                    payload.get(
                        "heartbeat_ms",
                        payload.get(
                            "heartbeat.interval.ms",
                            payload.get(
                                "heartbeatIntervalMs",
                                self.config.get(
                                    "group",
                                    "default_heartbeat_interval_ms",
                                    1000,
                                ),
                            ),
                        ),
                    ),
                )
            )
        except (TypeError, ValueError):
            raise api_error(
                INVALID_REQUEST, "session_timeout_ms and heartbeat_interval_ms must be integers"
            ) from None
        if session_timeout_ms <= 0 or heartbeat_interval_ms <= 0:
            raise api_error(
                INVALID_REQUEST, "session_timeout_ms and heartbeat_interval_ms must be positive"
            )
        now = _now_ms()
        with self.lock:
            existing = self.members.get(member_id)
            if existing and existing.status == ACTIVE:
                existing.session_timeout_ms = session_timeout_ms
                existing.heartbeat_interval_ms = heartbeat_interval_ms
                existing.last_heartbeat_ms = now
                self._persist_member(existing)
                self._event(
                    "member_rejoined_idempotent",
                    now,
                    member_id,
                    details={"session_timeout_ms": session_timeout_ms},
                )
            else:
                incarnation = (existing.incarnation + 1) if existing else 1
                member = Member(
                    member_id=member_id,
                    session_timeout_ms=session_timeout_ms,
                    heartbeat_interval_ms=heartbeat_interval_ms,
                    status=ACTIVE,
                    joined_at_ms=now,
                    last_heartbeat_ms=now,
                    incarnation=incarnation,
                )
                self.members[member_id] = member
                self._persist_member(member)
                self._event(
                    "member_joined",
                    now,
                    member_id,
                    details={
                        "session_timeout_ms": session_timeout_ms,
                        "heartbeat_interval_ms": heartbeat_interval_ms,
                        "incarnation": incarnation,
                    },
                )
                self._request_rebalance_locked("member_join", member_id, set(), now)
            self._tick_locked(now)
            return self._join_response_locked(member_id)

    def _join_response_locked(self, member_id: str) -> dict[str, Any]:
        member = self.members[member_id]
        owned = self._owned_partitions_locked(member_id)
        return {
            "ok": True,
            "group_id": self.group_id,
            "topic": self.topic,
            "member_id": member_id,
            "session_timeout_ms": member.session_timeout_ms,
            "heartbeat_interval_ms": member.heartbeat_interval_ms,
            "generation": self.generation,
            "assignment_generation": self.generation,
            "owned_partitions": owned,
            "paused_partitions": self._paused_partitions_locked(member_id),
            "assignments": self._visible_assignments_locked(),
            "rebalance_pending": self.pending is not None,
            "rebalance_in_progress": self.in_progress is not None,
        }

    def heartbeat(self, member_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        now = _now_ms()
        with self.lock:
            member = self.members.get(member_id)
            if member is None:
                raise api_error(UNKNOWN_MEMBER, f"member {member_id!r} is not known")
            if member.status != ACTIVE:
                raise api_error(
                    MEMBER_NOT_ACTIVE,
                    f"member {member_id!r} is {member.status}",
                    status=member.status,
                    removal_reason=member.removal_reason,
                )
            if "session_timeout_ms" in payload:
                try:
                    value = int(payload["session_timeout_ms"])
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "session_timeout_ms must be an integer"
                    ) from None
                if value <= 0:
                    raise api_error(
                        INVALID_REQUEST, "session_timeout_ms must be positive"
                    )
                member.session_timeout_ms = value
            elif "session.timeout.ms" in payload or "sessionTimeoutMs" in payload:
                raw = payload.get("session.timeout.ms", payload.get("sessionTimeoutMs"))
                try:
                    value = int(raw)
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "session.timeout.ms must be an integer"
                    ) from None
                if value <= 0:
                    raise api_error(
                        INVALID_REQUEST, "session.timeout.ms must be positive"
                    )
                member.session_timeout_ms = value
            if "heartbeat_interval_ms" in payload:
                try:
                    value = int(payload["heartbeat_interval_ms"])
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "heartbeat_interval_ms must be an integer"
                    ) from None
                if value <= 0:
                    raise api_error(
                        INVALID_REQUEST, "heartbeat_interval_ms must be positive"
                    )
                member.heartbeat_interval_ms = value
            member.last_heartbeat_ms = now
            self._persist_member(member)
            revoked = self._revoked_for_member_locked(member_id)
            response = {
                "ok": True,
                "member_id": member_id,
                "active": True,
                "status": ACTIVE,
                "generation": self.generation,
                "assignment_generation": self.generation,
                "owned_partitions": self._owned_partitions_locked(member_id),
                "paused_partitions": self._paused_partitions_locked(member_id),
                "revoked_partitions": sorted(revoked),
                "revoke_ack_required": bool(revoked),
                "rebalance_pending": self.pending is not None,
                "rebalance_in_progress": self.in_progress is not None,
                "session_timeout_ms": member.session_timeout_ms,
                "heartbeat_interval_ms": member.heartbeat_interval_ms,
                "server_time_ms": now,
            }
            self._tick_locked(now)
            return response

    def ack_revoke(
        self, member_id: str, payload: dict[str, Any]
    ) -> dict[str, Any]:
        now = _now_ms()
        with self.lock:
            member = self.members.get(member_id)
            if member is None:
                raise api_error(UNKNOWN_MEMBER, f"member {member_id!r} is not known")
            progress = self.in_progress
            if progress is None:
                return {
                    "ok": True,
                    "member_id": member_id,
                    "acknowledged": [],
                    "rebalance_in_progress": False,
                }
            requested = payload.get("partitions")
            own_revoking = set(progress.revoking.get(member_id, set()))
            if requested is None:
                acknowledged = own_revoking
            else:
                if not isinstance(requested, list):
                    raise api_error(INVALID_REQUEST, "partitions must be an array")
                try:
                    acknowledged = {int(item) for item in requested}
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "partition numbers must be integers"
                    ) from None
                invalid = acknowledged - own_revoking
                if invalid:
                    raise api_error(
                        PARTITION_NOT_OWNED,
                        "member is not revoking the supplied partitions",
                        partitions=sorted(invalid),
                    )
            progress.acked.setdefault(member_id, set()).update(acknowledged)
            for partition in acknowledged:
                self.inflight.pop(partition, None)
            self._event(
                "revocation_acknowledged",
                now,
                member_id,
                details={"partitions": sorted(acknowledged)},
            )
            self._tick_locked(now)
            return {
                "ok": True,
                "member_id": member_id,
                "acknowledged": sorted(acknowledged),
                "rebalance_in_progress": self.in_progress is not None,
                "generation": self.generation,
            }

    def leave(self, member_id: str) -> dict[str, Any]:
        now = _now_ms()
        with self.lock:
            member = self.members.get(member_id)
            if member is None:
                raise api_error(UNKNOWN_MEMBER, f"member {member_id!r} is not known")
            if member.status != ACTIVE:
                return {
                    "ok": True,
                    "member_id": member_id,
                    "status": member.status,
                    "recovered_partitions": [],
                    "idempotent": True,
                }
            recovered = self._recover_member_locked(
                member, LEFT, "member_leave", now
            )
            self._request_rebalance_locked("member_leave", member_id, recovered, now)
            self._tick_locked(now)
            return {
                "ok": True,
                "member_id": member_id,
                "status": LEFT,
                "recovered_partitions": recovered,
                "recovered_at_ms": now,
                "recovery_reason": "member_leave",
                "generation": self.generation,
            }

    def _recover_member_locked(
        self,
        member: Member,
        status: str,
        reason: str,
        now: int,
    ) -> list[int]:
        recovered: list[int] = []
        for partition, assignment in list(self.assignments.items()):
            if assignment.member_id != member.member_id:
                continue
            recovered.append(partition)
            self.assignments.pop(partition, None)
            self.inflight.pop(partition, None)
            self.store.execute(
                "DELETE FROM assignments WHERE partition = ?", (partition,)
            )
            self._set_quarantine(partition, member.member_id, reason, now)
            self._event(
                "partition_reclaimed",
                now,
                member.member_id,
                partition,
                {
                    "reason": reason,
                    "previous_state": assignment.state,
                    "paused": assignment.paused,
                    "reclaimed_at_ms": now,
                },
            )
        member.status = status
        member.removed_at_ms = now
        member.removal_reason = reason
        self._persist_member(member)
        self._event(
            "member_recovered",
            now,
            member.member_id,
            details={
                "status": status,
                "reason": reason,
                "partitions": sorted(recovered),
                "recovered_at_ms": now,
            },
        )
        return sorted(recovered)

    def _expire_members_locked(self, now: int) -> None:
        for member in list(self.members.values()):
            if member.status != ACTIVE:
                continue
            silent_ms = now - member.last_heartbeat_ms
            if silent_ms <= member.session_timeout_ms:
                continue
            recovered = self._recover_member_locked(
                member, TIMED_OUT, "heartbeat_timeout", now
            )
            self._increment(
                "member",
                "heartbeat_timeout_total",
                {"member": member.member_id},
            )
            self._event(
                "member_heartbeat_timeout",
                now,
                member.member_id,
                details={
                    "silent_ms": silent_ms,
                    "session_timeout_ms": member.session_timeout_ms,
                    "recovered_partitions": recovered,
                    "recovered_at_ms": now,
                    "reason": "heartbeat_timeout",
                },
            )
            self._request_rebalance_locked(
                "heartbeat_timeout", member.member_id, recovered, now
            )

    # ------------------------------------------------------------------
    # Assignment and rebalance
    # ------------------------------------------------------------------
    def _active_members_locked(self) -> list[str]:
        return sorted(
            member_id
            for member_id, member in self.members.items()
            if member.status == ACTIVE
        )

    def _compute_target_locked(self) -> dict[int, str]:
        members = self._active_members_locked()
        if not members:
            return {}
        target = {
            partition: members[partition % len(members)]
            for partition in range(self.partition_count)
        }
        # A paused partition remains pinned to its current owner. Pausing is
        # explicitly not a handoff: it may only be reassigned after the member
        # leaves/times out, which clears the pause and quarantines the partition.
        active_set = set(members)
        for partition, assignment in self.assignments.items():
            if (
                assignment.state == ACTIVE
                and assignment.paused
                and assignment.member_id in active_set
            ):
                target[partition] = assignment.member_id
        return target

    def _owned_partitions_locked(self, member_id: str) -> list[int]:
        return sorted(
            partition
            for partition, assignment in self.assignments.items()
            if assignment.member_id == member_id and assignment.state == ACTIVE
        )

    def _paused_partitions_locked(self, member_id: str) -> list[int]:
        return sorted(
            partition
            for partition, assignment in self.assignments.items()
            if assignment.member_id == member_id
            and assignment.state == ACTIVE
            and assignment.paused
        )

    def _revoked_for_member_locked(self, member_id: str) -> set[int]:
        if self.in_progress is None:
            return set()
        return set(self.in_progress.revoking.get(member_id, set()))

    def _visible_assignments_locked(self) -> list[dict[str, Any]]:
        return [
            assignment.as_dict()
            for assignment in sorted(
                self.assignments.values(), key=lambda item: item.partition
            )
            if assignment.state == ACTIVE
        ]

    def _involved_locked(self, target: dict[int, str]) -> set[int]:
        involved: set[int] = set()
        for partition in range(self.partition_count):
            assignment = self.assignments.get(partition)
            current_owner = (
                assignment.member_id
                if assignment is not None and assignment.state == ACTIVE
                else None
            )
            desired_owner = target.get(partition)
            if current_owner != desired_owner or partition in self.quarantine:
                involved.add(partition)
        return involved

    def _request_rebalance_locked(
        self,
        reason: str,
        trigger_member: str | None,
        recovered_partitions: Iterable[int],
        now: int,
    ) -> None:
        active = self._active_members_locked()
        if not active:
            if self.pending is not None:
                self._abort_pending_locked("no_active_members", now)
            return
        target = self._compute_target_locked()
        involved = self._involved_locked(target)
        involved.update(recovered_partitions)
        if self.pending is not None:
            self.pending.involved.update(involved)
            if reason not in self.pending.reason.split("+"):
                self.pending.reason = f"{self.pending.reason}+{reason}"
            if trigger_member:
                self.pending.trigger_member = trigger_member
            pending = self.pending
        else:
            pending = PendingRebalance(
                id=uuid.uuid4().hex,
                reason=reason,
                trigger_member=trigger_member,
                involved=set(involved),
                requested_at_ms=now,
                scheduled_at_ms=now,
            )
            self.pending = pending

        pending.involved.update(involved)
        if self.in_progress is not None:
            pending.deferred = True
            pending.status = "queued"
            pending.scheduled_at_ms = now
            pending.suppression_reason = "rebalance_in_progress"
            self._upsert_rebalance_locked(pending, now)
            return

        current_active_assignments = any(
            assignment.state == ACTIVE for assignment in self.assignments.values()
        )
        initial_immediate = bool(
            self.config.get("rebalance", "initial_assignment_immediate", True)
        )
        suppress_reasons: list[str] = []
        deadlines: list[int] = []
        max_partitions = int(
            self.config.get("rebalance", "max_partitions_per_rebalance", 6)
        )
        cooldown_ms = int(self.config.get("rebalance", "cooldown_ms", 5000))
        threshold_delay = int(
            self.config.get("rebalance", "partition_threshold_delay_ms", 1500)
        )
        max_delay = int(
            self.config.get("rebalance", "suppression_max_delay_ms", 8000)
        )

        if not (initial_immediate and not current_active_assignments):
            if len(pending.involved) > max_partitions:
                suppress_reasons.append(
                    f"partition_threshold:{len(pending.involved)}>{max_partitions}"
                )
                threshold_deadline = now + threshold_delay
                cap = pending.requested_at_ms + max_delay
                deadlines.append(min(threshold_deadline, cap))
            if self.last_rebalance_completed_ms is not None:
                elapsed = now - self.last_rebalance_completed_ms
                if elapsed < cooldown_ms:
                    suppress_reasons.append(
                        f"cooldown:{max(0, cooldown_ms - elapsed)}ms_remaining"
                    )
                    deadlines.append(self.last_rebalance_completed_ms + cooldown_ms)

        if suppress_reasons:
            pending.scheduled_at_ms = max(deadlines or [now])
            previous_reason = pending.suppression_reason
            pending.suppression_reason = "|".join(suppress_reasons)
            pending.status = "suppressed"
            self._upsert_rebalance_locked(pending, now)
            for item in suppress_reasons:
                label = item.split(":", 1)[0]
                self._increment(
                    "rebalance", "suppressed_total", {"reason": label}
                )
            if pending.suppression_reason != previous_reason:
                self._event(
                    "rebalance_suppressed",
                    now,
                    trigger_member,
                    details={
                        "rebalance_id": pending.id,
                        "reason": pending.reason,
                        "suppression_reason": pending.suppression_reason,
                        "scheduled_at_ms": pending.scheduled_at_ms,
                        "involved_partitions": sorted(pending.involved),
                    },
                )
        else:
            pending.scheduled_at_ms = now
            pending.status = "scheduled"
            pending.suppression_reason = None
            self._upsert_rebalance_locked(pending, now)

    def _upsert_rebalance_locked(
        self, pending: PendingRebalance, now: int
    ) -> None:
        self.store.execute(
            """
            INSERT INTO rebalances(
                id, reason, trigger_member, involved_json, suppression_reason,
                status, requested_at_ms, scheduled_at_ms
            ) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(id) DO UPDATE SET
                reason = excluded.reason,
                trigger_member = excluded.trigger_member,
                involved_json = excluded.involved_json,
                suppression_reason = excluded.suppression_reason,
                status = excluded.status,
                scheduled_at_ms = excluded.scheduled_at_ms
            """,
            (
                pending.id,
                pending.reason,
                pending.trigger_member,
                _json_dumps(sorted(pending.involved)),
                pending.suppression_reason,
                pending.status,
                pending.requested_at_ms,
                pending.scheduled_at_ms,
            ),
        )

    def _abort_pending_locked(self, reason: str, now: int) -> None:
        if self.pending is None:
            return
        pending = self.pending
        pending.status = "aborted"
        pending.suppression_reason = reason
        self._upsert_rebalance_locked(pending, now)
        self._event(
            "rebalance_aborted",
            now,
            pending.trigger_member,
            details={"rebalance_id": pending.id, "reason": reason},
        )
        self.pending = None

    def _tick_locked(self, now: int) -> None:
        self._expire_members_locked(now)
        if self.in_progress is not None:
            progress = self.in_progress
            revoked_partitions = {
                partition
                for partitions in progress.revoking.values()
                for partition in partitions
            }
            has_outstanding_batch = any(
                partition in self.inflight for partition in revoked_partitions
            )
            if not has_outstanding_batch:
                self._finalize_rebalance_locked(now)
            return
        if self.pending is None:
            return
        if self.pending.status == "queued":
            fresh_involved = self._involved_locked(self._compute_target_locked())
            if not fresh_involved:
                self._abort_pending_locked("merged_into_previous_rebalance", now)
                return
            self.pending.involved = set(fresh_involved)
            # Evaluate the follow-up rebalance against the cooldown that
            # applies after the rebalance which just completed.
            self._request_rebalance_locked(
                self.pending.reason,
                self.pending.trigger_member,
                self.pending.involved,
                now,
            )
        if now < self.pending.scheduled_at_ms:
            return
        self._begin_rebalance_locked(now)

    def _begin_rebalance_locked(self, now: int) -> None:
        pending = self.pending
        if pending is None:
            return
        if not self._active_members_locked():
            self._abort_pending_locked("no_active_members", now)
            return
        target = self._compute_target_locked()
        revoking: dict[str, set[int]] = {}
        for partition, assignment in list(self.assignments.items()):
            desired = target.get(partition)
            if assignment.state == ACTIVE and assignment.member_id != desired:
                assignment.state = "revoking"
                assignment.paused = False
                assignment.paused_at_ms = None
                self._persist_assignment(assignment)
                revoking.setdefault(assignment.member_id, set()).add(partition)
            elif assignment.state == "revoking" and assignment.member_id != desired:
                revoking.setdefault(assignment.member_id, set()).add(partition)
        pending.status = "revoking"
        self._upsert_rebalance_locked(pending, now)
        self.pending = None
        if not revoking:
            self._finalize_rebalance_locked(now, pending=pending, target=target)
            return
        timeout_ms = int(
            self.config.get("rebalance", "revoke_ack_timeout_ms", 2500)
        )
        self.in_progress = InProgressRebalance(
            id=pending.id,
            reason=pending.reason,
            target=target,
            revoking=revoking,
            started_at_ms=now,
            deadline_ms=now + max(0, timeout_ms),
            generation=self.generation + 1,
            involved=set(pending.involved),
            trigger_member=pending.trigger_member,
        )
        self.store.execute(
            """
            UPDATE rebalances SET status = 'revoking', started_at_ms = ?,
                generation = ?, involved_json = ?
            WHERE id = ?
            """,
            (
                now,
                self.generation + 1,
                _json_dumps(sorted(pending.involved)),
                pending.id,
            ),
        )
        self._event(
            "rebalance_revoke_started",
            now,
            pending.trigger_member,
            details={
                "rebalance_id": pending.id,
                "reason": pending.reason,
                "revoking": {
                    member: sorted(partitions)
                    for member, partitions in sorted(revoking.items())
                },
                "ack_deadline_ms": self.in_progress.deadline_ms,
            },
        )
        if not any(
            partition in self.inflight
            for partitions in revoking.values()
            for partition in partitions
        ):
            self._finalize_rebalance_locked(now)

    def _finalize_rebalance_locked(
        self,
        now: int,
        pending: PendingRebalance | None = None,
        target: dict[int, str] | None = None,
    ) -> None:
        progress = self.in_progress
        if pending is None:
            if progress is None:
                return
            pending = PendingRebalance(
                id=progress.id,
                reason=progress.reason,
                trigger_member=progress.trigger_member,
                involved=set(progress.involved),
                requested_at_ms=progress.started_at_ms,
                scheduled_at_ms=progress.started_at_ms,
                status="revoking",
            )
        else:
            progress = self.in_progress
        target = self._compute_target_locked() if target is None else target
        if not target:
            if progress is not None:
                self.store.execute(
                    "UPDATE rebalances SET status = 'aborted', completed_at_ms = ? WHERE id = ?",
                    (now, progress.id),
                )
            self.in_progress = None
            return

        # If membership changed during the revoke stage, any newly changing
        # active partition must still go through revocation before reassignment.
        extra_revoking: dict[str, set[int]] = {}
        for partition, assignment in list(self.assignments.items()):
            desired = target.get(partition)
            if assignment.state == ACTIVE and assignment.member_id != desired:
                assignment.state = "revoking"
                assignment.paused = False
                assignment.paused_at_ms = None
                self._persist_assignment(assignment)
                extra_revoking.setdefault(assignment.member_id, set()).add(partition)
        if extra_revoking:
            if progress is None:
                progress = InProgressRebalance(
                    id=pending.id,
                    reason=pending.reason,
                    target=target,
                    revoking={},
                    started_at_ms=now,
                    deadline_ms=now,
                    generation=self.generation + 1,
                    involved=set(pending.involved),
                    trigger_member=pending.trigger_member,
                )
                self.in_progress = progress
            for member, partitions in extra_revoking.items():
                progress.revoking.setdefault(member, set()).update(partitions)
            timeout_ms = int(
                self.config.get("rebalance", "revoke_ack_timeout_ms", 2500)
            )
            progress.deadline_ms = now + max(0, timeout_ms)
            return

        old_assignments = dict(self.assignments)
        new_generation = self.generation + 1
        new_assignments: dict[int, Assignment] = {}
        previous_owners = {
            partition: assignment.member_id
            for partition, assignment in old_assignments.items()
        }
        for partition in range(self.partition_count):
            owner = target.get(partition)
            if owner is None:
                continue
            old = old_assignments.get(partition)
            preserve_pause = bool(
                old
                and old.member_id == owner
                and old.paused
                and old.state == ACTIVE
                and partition not in self.quarantine
            )
            new_assignments[partition] = Assignment(
                partition=partition,
                member_id=owner,
                generation=new_generation,
                assigned_at_ms=now,
                state=ACTIVE,
                paused=preserve_pause,
                paused_at_ms=old.paused_at_ms if preserve_pause and old else None,
            )

        with self.store.transaction() as connection:
            connection.execute("DELETE FROM assignments")
            for assignment in new_assignments.values():
                connection.execute(
                    """
                    INSERT INTO assignments(
                        partition, member_id, generation, assigned_at_ms,
                        state, paused, paused_at_ms
                    ) VALUES(?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        assignment.partition,
                        assignment.member_id,
                        assignment.generation,
                        assignment.assigned_at_ms,
                        assignment.state,
                        int(assignment.paused),
                        assignment.paused_at_ms,
                    ),
                )
                connection.execute(
                    "DELETE FROM quarantine WHERE partition = ?",
                    (assignment.partition,),
                )
            for partition in range(self.partition_count):
                if partition not in new_assignments:
                    connection.execute(
                        """
                        INSERT INTO quarantine(
                            partition, former_member_id, reason, since_ms
                        ) VALUES(?, ?, ?, ?)
                        ON CONFLICT(partition) DO UPDATE SET
                            former_member_id = excluded.former_member_id,
                            reason = excluded.reason,
                            since_ms = excluded.since_ms
                        """,
                        (
                            partition,
                            previous_owners.get(partition),
                            "unassigned",
                            now,
                        ),
                    )
            connection.execute(
                """
                UPDATE rebalances SET status = 'completed', completed_at_ms = ?,
                    generation = ?, involved_json = ?
                WHERE id = ?
                """,
                (
                    now,
                    new_generation,
                    _json_dumps(sorted(pending.involved)),
                    pending.id,
                ),
            )

        self.assignments = new_assignments
        for partition in new_assignments:
            self.quarantine.pop(partition, None)
        for partition in range(self.partition_count):
            if partition not in new_assignments:
                self.quarantine[partition] = QuarantineRecord(
                    partition=partition,
                    former_member_id=previous_owners.get(partition),
                    reason="unassigned",
                    since_ms=now,
                )
        self.generation = new_generation
        self.last_rebalance_completed_ms = now
        self.in_progress = None
        self.store.set_metadata("generation", str(new_generation))
        self.store.set_metadata("last_rebalance_completed_ms", str(now))
        self._increment("rebalance", "completed_total")
        self._event(
            "rebalance_completed",
            now,
            pending.trigger_member,
            details={
                "rebalance_id": pending.id,
                "reason": pending.reason,
                "generation": new_generation,
                "involved_partitions": sorted(pending.involved),
                "assignments": {
                    str(partition): assignment.member_id
                    for partition, assignment in sorted(new_assignments.items())
                },
            },
        )
        self._round_robin_partition = 0

    # ------------------------------------------------------------------
    # Offset commits, pause/resume and fetches
    # ------------------------------------------------------------------
    def commit(self, member_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        if "commits" in payload:
            commits = payload["commits"]
            if not isinstance(commits, list) or not commits:
                raise api_error(INVALID_REQUEST, "commits must be a non-empty array")
            results = []
            for item in commits:
                if not isinstance(item, dict):
                    raise api_error(INVALID_REQUEST, "each commit must be an object")
                results.append(self._commit_one(member_id, item))
            return {"ok": True, "member_id": member_id, "commits": results}
        result = self._commit_one(member_id, payload)
        return {"ok": True, "member_id": member_id, **result}

    def _commit_one(
        self, member_id: str, payload: dict[str, Any]
    ) -> dict[str, Any]:
        now = _now_ms()
        try:
            partition = int(payload["partition"])
            offset = int(payload.get("offset", payload.get("committed_offset")))
        except (KeyError, TypeError, ValueError):
            raise api_error(
                INVALID_REQUEST, "partition and integer offset are required"
            ) from None
        with self.lock:
            if partition < 0 or partition >= self.partition_count:
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "INVALID_REQUEST"},
                )
                raise api_error(
                    INVALID_REQUEST,
                    f"partition {partition} is outside 0..{self.partition_count - 1}",
                )
            if offset < 0:
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "INVALID_REQUEST"},
                )
                raise api_error(INVALID_REQUEST, "offset must be non-negative")
            member = self.members.get(member_id)
            if member is None or member.status != ACTIVE:
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "MEMBER_NOT_ACTIVE"},
                )
                if member is None:
                    raise api_error(
                        UNKNOWN_MEMBER, f"member {member_id!r} is not known"
                    )
                raise api_error(
                    MEMBER_NOT_ACTIVE, f"member {member_id!r} is not active"
                )
            assignment = self.assignments.get(partition)
            if (
                assignment is None
                or assignment.member_id != member_id
                or assignment.state not in (ACTIVE, "revoking")
            ):
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "PARTITION_NOT_OWNED"},
                )
                raise api_error(
                    PARTITION_NOT_OWNED,
                    f"partition {partition} is not held by {member_id!r}",
                    owner=assignment.member_id if assignment else None,
                )
            lease = self.inflight.get(partition)
            supplied_generation = payload.get("generation")
            if supplied_generation is not None:
                try:
                    supplied_generation = int(supplied_generation)
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "generation must be an integer"
                    ) from None
                # A batch can be delivered immediately before a rebalance
                # increments the assignment generation. If the same member
                # continuously owns the partition, its matching lease proves
                # the batch is still safe to commit.
                matching_lease = bool(
                    lease
                    and lease[0] == member_id
                    and lease[1] == supplied_generation
                )
                if (
                    supplied_generation != assignment.generation
                    and not matching_lease
                ):
                    self._increment(
                        "commit",
                        "rejected_total",
                        {"partition": str(partition), "reason": "STALE_GENERATION"},
                    )
                    raise api_error(
                        STALE_GENERATION,
                        "the supplied generation is stale",
                        current_generation=assignment.generation,
                    )
            # From this point the old owner has finished its delivered batch.
            # Releasing the lease is safe even if the write itself is rejected
            # for a business reason such as OFFSET_REGRESSION.
            self.inflight.pop(partition, None)
            if assignment.paused:
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "PARTITION_PAUSED"},
                )
                raise api_error(
                    PARTITION_PAUSED,
                    f"partition {partition} is paused; committed offset is frozen",
                    committed_offset=self.commits.get(partition).offset
                    if partition in self.commits
                    else 0,
                )
            existing = self.commits.get(partition)
            if existing is not None and offset < existing.offset:
                self._increment(
                    "commit",
                    "rejected_total",
                    {"partition": str(partition), "reason": "OFFSET_REGRESSION"},
                )
                raise api_error(
                    OFFSET_REGRESSION,
                    "offset would move the committed offset backwards",
                    committed_offset=existing.offset,
                    attempted_offset=offset,
                )
            previous = existing.offset if existing is not None else 0
            duplicate = existing is not None and existing.offset == offset
            record = CommitRecord(
                partition=partition,
                offset=offset,
                member_id=member_id,
                generation=assignment.generation,
                updated_at_ms=now,
            )
            self.commits[partition] = record
            self._persist_commit(record)
            self._increment(
                "commit",
                "accepted_total",
                {"partition": str(partition)},
            )
            self._event(
                "offset_committed",
                now,
                member_id,
                partition,
                {
                    "offset": offset,
                    "previous_offset": previous,
                    "generation": assignment.generation,
                    "duplicate": duplicate,
                },
            )
            return {
                "partition": partition,
                "committed_offset": offset,
                "previous_offset": previous,
                "member_id": member_id,
                "generation": assignment.generation,
                "updated_at_ms": now,
                "duplicate": duplicate,
            }

    def pause(self, member_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        return self._set_pause(member_id, payload, True)

    def resume(self, member_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        return self._set_pause(member_id, payload, False)

    def _set_pause(
        self, member_id: str, payload: dict[str, Any], paused: bool
    ) -> dict[str, Any]:
        now = _now_ms()
        try:
            partition = int(payload["partition"])
        except (KeyError, TypeError, ValueError):
            raise api_error(INVALID_REQUEST, "integer partition is required") from None
        with self.lock:
            member = self.members.get(member_id)
            if member is None:
                raise api_error(UNKNOWN_MEMBER, f"member {member_id!r} is not known")
            if member.status != ACTIVE:
                raise api_error(
                    MEMBER_NOT_ACTIVE, f"member {member_id!r} is not active"
                )
            assignment = self.assignments.get(partition)
            if (
                assignment is None
                or assignment.member_id != member_id
                or assignment.state != ACTIVE
            ):
                raise api_error(
                    PARTITION_NOT_OWNED,
                    f"partition {partition} is not actively owned by {member_id!r}",
                )
            if assignment.paused == paused:
                return {
                    "ok": True,
                    "member_id": member_id,
                    "partition": partition,
                    "paused": paused,
                    "idempotent": True,
                    "committed_offset": self.commits.get(partition).offset
                    if partition in self.commits
                    else 0,
                    "operation_at_ms": (
                        assignment.paused_at_ms if paused else now
                    ),
                }
            assignment.paused = paused
            assignment.paused_at_ms = now if paused else None
            self._persist_assignment(assignment)
            operation = "pause" if paused else "resume"
            self._event(
                operation,
                now,
                member_id,
                partition,
                {
                    "operation": operation,
                    "partition": partition,
                    "member_id": member_id,
                    "operation_at_ms": now,
                    "committed_offset": self.commits.get(partition).offset
                    if partition in self.commits
                    else 0,
                },
            )
            return {
                "ok": True,
                "member_id": member_id,
                "partition": partition,
                "paused": paused,
                "idempotent": False,
                "committed_offset": self.commits.get(partition).offset
                if partition in self.commits
                else 0,
                "operation_at_ms": now,
            }

    def fetch(self, member_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        with self.lock:
            member = self.members.get(member_id)
            if member is None:
                raise api_error(UNKNOWN_MEMBER, f"member {member_id!r} is not known")
            if member.status != ACTIVE:
                raise api_error(
                    MEMBER_NOT_ACTIVE, f"member {member_id!r} is not active"
                )
            owned = self._owned_partitions_locked(member_id)
            if not owned:
                return self._empty_fetch_locked(member_id, None, "NO_ASSIGNMENT")
            requested = payload.get("partition")
            if requested is None:
                candidates = [
                    partition
                    for partition in owned
                    if not self.assignments[partition].paused
                ]
                if not candidates:
                    return self._empty_fetch_locked(
                        member_id, owned[0], "ALL_PARTITIONS_PAUSED"
                    )
                self._round_robin_partition = (
                    self._round_robin_partition + 1
                ) % len(candidates)
                partition = candidates[self._round_robin_partition]
            else:
                try:
                    partition = int(requested)
                except (TypeError, ValueError):
                    raise api_error(
                        INVALID_REQUEST, "partition must be an integer"
                    ) from None
            assignment = self.assignments.get(partition)
            if assignment is None or assignment.state != ACTIVE:
                raise api_error(
                    PARTITION_NOT_OWNED,
                    f"partition {partition} is not actively assigned",
                )
            if assignment.member_id != member_id:
                raise api_error(
                    PARTITION_NOT_OWNED,
                    f"partition {partition} is owned by {assignment.member_id!r}",
                )
            if assignment.paused:
                return self._empty_fetch_locked(member_id, partition, "PAUSED")
            try:
                max_records = int(
                    payload.get(
                        "max_records",
                        self.config.get("group", "max_fetch_records", 250),
                    )
                )
            except (TypeError, ValueError):
                raise api_error(INVALID_REQUEST, "max_records must be an integer") from None
            max_records = max(1, min(1000, max_records))
            max_bytes = int(
                payload.get(
                    "max_bytes",
                    self.config.get("group", "max_fetch_bytes", 1048576),
                )
            )
            commit = self.commits.get(partition)
            if commit is not None:
                fetch_offset = commit.offset
            else:
                try:
                    fetch_offset = self.backend.earliest_offsets([partition]).get(
                        partition, 0
                    )
                except BackendError as exc:
                    raise api_error(
                        KAFKA_UNAVAILABLE, f"cannot query earliest offset: {exc}"
                    ) from exc
            try:
                result = self.backend.fetch(
                    partition=partition,
                    offset=fetch_offset,
                    max_bytes=max(1024, max_bytes),
                    max_records=max_records,
                )
            except BackendError as exc:
                raise api_error(
                    KAFKA_UNAVAILABLE, f"cannot fetch partition {partition}: {exc}"
                ) from exc
            records = [record.as_dict(partition) for record in result.records]
            if result.records:
                self.inflight[partition] = (member_id, assignment.generation)
            next_offset = (
                result.records[-1].offset + 1 if result.records else fetch_offset
            )
            return {
                "ok": True,
                "member_id": member_id,
                "partition": partition,
                "generation": assignment.generation,
                "fetch_offset": fetch_offset,
                "next_commit_offset": next_offset,
                "high_watermark": result.high_watermark,
                "paused": False,
                "paused_reason": None,
                "records": records,
            }

    def _empty_fetch_locked(
        self, member_id: str, partition: int | None, reason: str
    ) -> dict[str, Any]:
        return {
            "ok": True,
            "member_id": member_id,
            "partition": partition,
            "generation": self.generation,
            "fetch_offset": None,
            "next_commit_offset": None,
            "high_watermark": None,
            "paused": reason == "PAUSED",
            "paused_reason": reason,
            "records": [],
        }

    # ------------------------------------------------------------------
    # Query, reconciliation and observability
    # ------------------------------------------------------------------
    def assignments_status(self) -> dict[str, Any]:
        with self.lock:
            return {
                "ok": True,
                "group_id": self.group_id,
                "topic": self.topic,
                "generation": self.generation,
                "assignments": self._visible_assignments_locked(),
                "partitions_by_member": {
                    member_id: self._owned_partitions_locked(member_id)
                    for member_id in sorted(self.members)
                    if self.members[member_id].status == ACTIVE
                },
            }

    def offset_status(self, partition: int | None = None) -> dict[str, Any]:
        with self.lock:
            if partition is not None:
                if partition < 0 or partition >= self.partition_count:
                    raise api_error(
                        INVALID_REQUEST,
                        f"partition must be between 0 and {self.partition_count - 1}",
                    )
                commit = self.commits.get(partition)
                assignment = self.assignments.get(partition)
                return {
                    "ok": True,
                    "partition": partition,
                    "committed": commit is not None,
                    "committed_offset": commit.offset if commit else 0,
                    "member_id": commit.member_id if commit else None,
                    "generation": commit.generation if commit else None,
                    "updated_at_ms": commit.updated_at_ms if commit else None,
                    "owner": assignment.member_id if assignment else None,
                    "paused": bool(assignment.paused) if assignment else False,
                }
            records = []
            for number in range(self.partition_count):
                commit = self.commits.get(number)
                assignment = self.assignments.get(number)
                records.append(
                    {
                        "partition": number,
                        "committed": commit is not None,
                        "committed_offset": commit.offset if commit else 0,
                        "member_id": commit.member_id if commit else None,
                        "generation": commit.generation if commit else None,
                        "updated_at_ms": commit.updated_at_ms if commit else None,
                        "owner": assignment.member_id if assignment else None,
                        "paused": bool(assignment.paused) if assignment else False,
                    }
                )
            return {"ok": True, "offsets": records}

    def partition_status(self) -> dict[str, Any]:
        with self.lock:
            result = []
            for partition in range(self.partition_count):
                assignment = self.assignments.get(partition)
                commit = self.commits.get(partition)
                quarantine = self.quarantine.get(partition)
                result.append(
                    {
                        "partition": partition,
                        "owner": assignment.member_id if assignment else None,
                        "generation": assignment.generation if assignment else None,
                        "state": assignment.state if assignment else "quarantined",
                        "paused": bool(assignment.paused) if assignment else False,
                        "paused_at_ms": assignment.paused_at_ms if assignment else None,
                        "committed_offset": commit.offset if commit else 0,
                        "committed": commit is not None,
                        "committed_at_ms": commit.updated_at_ms if commit else None,
                        "quarantined": quarantine.as_dict() if quarantine else None,
                    }
                )
            return {
                "ok": True,
                "topic": self.topic,
                "partitions": result,
            }

    def status(self, event_limit: int = 50) -> dict[str, Any]:
        with self.lock:
            members = []
            for member in sorted(self.members.values(), key=lambda item: item.member_id):
                data = member.as_dict()
                data.update(
                    {
                        "owned_partitions": self._owned_partitions_locked(
                            member.member_id
                        ),
                        "paused_partitions": self._paused_partitions_locked(
                            member.member_id
                        ),
                        "revoked_partitions": sorted(
                            self._revoked_for_member_locked(member.member_id)
                        ),
                    }
                )
                members.append(data)
            last_rebalance = self._last_rebalance_locked()
            return {
                "ok": True,
                "group_id": self.group_id,
                "topic": self.topic,
                "generation": self.generation,
                "phase": self._phase_locked(),
                "member_count": sum(
                    1 for member in self.members.values() if member.status == ACTIVE
                ),
                "members": members,
                "assignments": self._visible_assignments_locked(),
                "partitions_by_member": {
                    member_id: self._owned_partitions_locked(member_id)
                    for member_id in sorted(self.members)
                    if self.members[member_id].status == ACTIVE
                },
                "paused_partitions": {
                    member_id: self._paused_partitions_locked(member_id)
                    for member_id in sorted(self.members)
                    if self.members[member_id].status == ACTIVE
                },
                "quarantined_partitions": [
                    record.as_dict()
                    for _, record in sorted(self.quarantine.items())
                ],
                "pending_rebalance": self.pending.as_dict()
                if self.pending
                else None,
                "in_progress_rebalance": self.in_progress.as_dict()
                if self.in_progress
                else None,
                "last_rebalance": last_rebalance,
                "recent_events": self.store.recent_events(
                    max(1, min(5000, event_limit))
                ),
                "server_time_ms": _now_ms(),
                "uptime_ms": _now_ms() - self._started_at_ms,
            }

    def _phase_locked(self) -> str:
        if self.in_progress is not None:
            return "revoking"
        if self.pending is not None:
            return "suppressed" if self.pending.status == "suppressed" else "pending"
        return "stable"

    def _last_rebalance_locked(self) -> dict[str, Any] | None:
        row = self.store.fetchone(
            "SELECT * FROM rebalances ORDER BY requested_at_ms DESC LIMIT 1"
        )
        if row is None:
            return None
        return {
            "id": row["id"],
            "reason": row["reason"],
            "trigger_member": row["trigger_member"],
            "involved_partitions": json.loads(row["involved_json"]),
            "suppression_reason": row["suppression_reason"],
            "status": row["status"],
            "requested_at_ms": row["requested_at_ms"],
            "scheduled_at_ms": row["scheduled_at_ms"],
            "started_at_ms": row["started_at_ms"],
            "completed_at_ms": row["completed_at_ms"],
            "generation": row["generation"],
        }

    def _refresh_end_offsets(self, force: bool = False) -> dict[int, int]:
        interval_ms = int(self.config.get("metrics", "lag_refresh_ms", 1000))
        now = _now_ms()
        with self.lock:
            stale = now - self._end_offsets_at_ms >= interval_ms
            cached = dict(self._end_offsets_cache)
        if not force and not stale:
            return cached
        try:
            offsets = self.backend.end_offsets(range(self.partition_count))
        except BackendError:
            return cached
        with self.lock:
            self._end_offsets_cache.update(offsets)
            self._end_offsets_at_ms = now
            return dict(self._end_offsets_cache)

    def verify_lag(self) -> dict[str, Any]:
        started = _now_ms()
        verification_id = uuid.uuid4().hex
        with self.lock:
            generation = self.generation
            snapshot = {}
            for partition in range(self.partition_count):
                commit = self.commits.get(partition)
                assignment = self.assignments.get(partition)
                snapshot[partition] = {
                    "committed_offset": commit.offset if commit else 0,
                    "committed": commit is not None,
                    "owner": assignment.member_id if assignment else None,
                    "paused": bool(assignment.paused) if assignment else False,
                }
        try:
            end_offsets = self.backend.end_offsets(range(self.partition_count))
        except BackendError as exc:
            raise api_error(
                KAFKA_UNAVAILABLE, f"cannot query log end offsets: {exc}"
            ) from exc
        completed = _now_ms()
        partitions = []
        total_lag = 0
        for partition in range(self.partition_count):
            item = snapshot[partition]
            end_offset = int(end_offsets.get(partition, 0))
            lag = max(0, end_offset - item["committed_offset"])
            total_lag += lag
            partitions.append(
                {
                    "partition": partition,
                    "committed_offset": item["committed_offset"],
                    "committed": item["committed"],
                    "log_end_offset": end_offset,
                    "lag": lag,
                    "member_id": item["owner"],
                    "paused": item["paused"],
                    "commit_ahead_of_end": item["committed_offset"] > end_offset,
                }
            )
        return {
            "ok": True,
            "verification_id": verification_id,
            "topic": self.topic,
            "started_at_ms": started,
            "completed_at_ms": completed,
            "snapshot_generation": generation,
            "snapshot_semantics": (
                "committed offsets are captured before log-end-offset I/O; "
                "later commits cannot alter this result"
            ),
            "total_lag": total_lag,
            "partitions": partitions,
        }

    def metrics(self) -> str:
        end_offsets = self._refresh_end_offsets(force=False)
        with self.lock:
            lines = [
                "# HELP order_group_committed_offset Last committed offset by partition.",
                "# TYPE order_group_committed_offset gauge",
                "# HELP order_group_offset_commits_total Offset commit attempts by outcome.",
                "# TYPE order_group_offset_commits_total counter",
            ]
            rejected_by_partition: dict[str, int] = {}
            for (scope, name, labels), value in self.counters.items():
                if scope == "commit" and name == "rejected_total":
                    partition_label = dict(labels).get("partition", "")
                    rejected_by_partition[partition_label] = (
                        rejected_by_partition.get(partition_label, 0) + value
                    )
            for partition in range(self.partition_count):
                commit = self.commits.get(partition)
                offset = commit.offset if commit else 0
                lag = max(0, end_offsets.get(partition, 0) - offset)
                lines.append(
                    f'order_group_committed_offset{{topic="{_escape_label(self.topic)}",partition="{partition}"}} {offset}'
                )
                lines.append(
                    f'order_group_lag{{topic="{_escape_label(self.topic)}",partition="{partition}"}} {lag}'
                )
                assignment = self.assignments.get(partition)
                owner = assignment.member_id if assignment and assignment.state == ACTIVE else ""
                lines.append(
                    f'order_group_partition_assigned{{topic="{_escape_label(self.topic)}",partition="{partition}",member="{_escape_label(owner)}"}} {1 if owner else 0}'
                )
                lines.append(
                    f'order_group_partition_paused{{topic="{_escape_label(self.topic)}",partition="{partition}"}} {1 if assignment and assignment.paused else 0}'
                )
                lines.append(
                    f'order_group_offset_commits_total{{partition="{partition}",outcome="accepted"}} {self._counter("commit", "accepted_total", {"partition": str(partition)})}'
                )
                lines.append(
                    f'order_group_offset_commits_total{{partition="{partition}",outcome="rejected"}} {rejected_by_partition.get(str(partition), 0)}'
                )
            lines.extend(
                [
                    "# HELP order_group_offset_rejections_total Rejected offset commit attempts.",
                    "# TYPE order_group_offset_rejections_total counter",
                ]
            )
            rejection_rows = []
            for (scope, name, labels), value in sorted(self.counters.items()):
                if scope == "commit" and name == "rejected_total":
                    labels_dict = dict(labels)
                    rejection_rows.append(
                        (
                            labels_dict.get("partition", ""),
                            labels_dict.get("reason", ""),
                            value,
                        )
                    )
            for partition, reason, value in rejection_rows:
                lines.append(
                    f'order_group_offset_rejections_total{{partition="{partition}",reason="{_escape_label(reason)}"}} {value}'
                )
            lines.extend(
                [
                    "# HELP order_group_member_heartbeat_timeouts_total Members removed after session timeout.",
                    "# TYPE order_group_member_heartbeat_timeouts_total counter",
                ]
            )
            for (scope, name, labels), value in sorted(self.counters.items()):
                if scope == "member" and name == "heartbeat_timeout_total":
                    member = dict(labels).get("member", "")
                    lines.append(
                        f'order_group_member_heartbeat_timeouts_total{{member="{_escape_label(member)}"}} {value}'
                    )
            completed = self._counter("rebalance", "completed_total")
            lines.append("# HELP order_group_rebalance_total Completed rebalances.")
            lines.append("# TYPE order_group_rebalance_total counter")
            lines.append(f"order_group_rebalance_total {completed}")
            lines.append(
                "# HELP order_group_rebalances_suppressed_total Rebalance decisions delayed or merged."
            )
            lines.append(
                "# TYPE order_group_rebalances_suppressed_total counter"
            )
            for (scope, name, labels), value in sorted(self.counters.items()):
                if scope == "rebalance" and name == "suppressed_total":
                    reason = dict(labels).get("reason", "")
                    lines.append(
                        f'order_group_rebalances_suppressed_total{{reason="{_escape_label(reason)}"}} {value}'
                    )
            active_members = [
                member
                for member in sorted(self.members.values(), key=lambda item: item.member_id)
                if member.status == ACTIVE
            ]
            lines.append("# HELP order_group_active_members Active group members.")
            lines.append("# TYPE order_group_active_members gauge")
            lines.append(f"order_group_active_members {len(active_members)}")
            lines.append(
                "# HELP order_group_member_active Active membership by member."
            )
            lines.append("# TYPE order_group_member_active gauge")
            for member in active_members:
                lines.append(
                    f'order_group_member_active{{member="{_escape_label(member.member_id)}"}} 1'
                )
            lines.append(
                "# HELP order_group_generation Current assignment generation."
            )
            lines.append("# TYPE order_group_generation gauge")
            lines.append(f"order_group_generation {self.generation}")
            return "\n".join(lines) + "\n"

    def health(self) -> dict[str, Any]:
        with self.lock:
            return {
                "ok": True,
                "service": "order-group-coordinator",
                "topic": self.topic,
                "backend": self.config.backend_kind,
                "partitions": self.partition_count,
                "phase": self._phase_locked(),
                "generation": self.generation,
                "server_time_ms": _now_ms(),
            }


def _escape_label(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')

