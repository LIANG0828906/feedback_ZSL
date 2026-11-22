from __future__ import annotations

import json
import sqlite3
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping

from .errors import ErrorCode, HapticError, Issue
from .models import (
    EncodedPacket,
    PacketState,
    ScheduleDecision,
    canonical_json,
    sha256_hex,
)


@dataclass(frozen=True)
class CheckpointWindow:
    order_index: int
    packet: EncodedPacket
    decisions: tuple[ScheduleDecision, ...]
    consumed_event_offset: int
    capability_version: int
    capability_fingerprint: str
    event_stream_hash: str
    kernel_version: str

    def payload(self, previous_hash: str) -> dict[str, Any]:
        return {
            "order_index": self.order_index,
            "previous_hash": previous_hash,
            "packet": self.packet.to_dict(),
            "decisions": [decision.to_dict() for decision in self.decisions],
            "consumed_event_offset": self.consumed_event_offset,
            "capability_version": self.capability_version,
            "capability_fingerprint": self.capability_fingerprint,
            "event_stream_hash": self.event_stream_hash,
            "kernel_version": self.kernel_version,
        }


@dataclass(frozen=True)
class StoredCheckpoint:
    order_index: int
    content_hash: str
    packet: EncodedPacket
    decisions: tuple[ScheduleDecision, ...]
    consumed_event_offset: int
    capability_version: int
    capability_fingerprint: str
    event_stream_hash: str
    kernel_version: str


@dataclass(frozen=True)
class LoadResult:
    valid: bool
    checkpoints: tuple[StoredCheckpoint, ...]
    issue: Issue | None = None
    invalidated: bool = False


def _decision_from_dict(data: Mapping[str, Any]) -> ScheduleDecision:
    from .models import DecisionType

    return ScheduleDecision(
        decision_id=int(data["decision_id"]),
        event_id=str(data["event_id"]),
        event_order=(int(data["event_order"][0]), int(data["event_order"][1])),
        channel=int(data["channel"]),
        decision_type=DecisionType(str(data["decision_type"])),
        basis_event_ids=tuple(str(item) for item in data["basis_event_ids"]),
        target_packet_seq=int(data["target_packet_seq"]),
        reason=str(data["reason"]),
        intensity=float(data["intensity"]),
        priority=int(data["priority"]),
        details=dict(data.get("details", {})),
    )


def _packet_from_dict(data: Mapping[str, Any]) -> EncodedPacket:
    return EncodedPacket(
        packet_seq=int(data["packet_seq"]),
        channel=int(data["channel"]),
        data=bytes.fromhex(str(data["data_hex"])),
        timing_mark_ms=float(data["timing_mark_ms"]),
        event_count=int(data["event_count"]),
        local_index=int(data["local_index"]),
        state=PacketState(str(data["state"])),
        reused=bool(data["reused"]),
        invalidated=bool(data["invalidated"]),
        sent_at_ms=float(data["sent_at_ms"]) if data["sent_at_ms"] is not None else None,
    )


class CheckpointStore:
    """Serialized, hash-chained SQLite checkpoint store.

    Every window is committed in its own `BEGIN IMMEDIATE` transaction.  A
    process-wide lock and the SQLite write lock ensure that bytes and decisions
    from different windows can never interleave.
    """

    def __init__(self, directory: str | Path, db_name: str = "checkpoints.sqlite3"):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.path = self.directory / db_name
        self._lock = threading.RLock()
        self._connection = sqlite3.connect(
            self.path,
            check_same_thread=False,
            isolation_level=None,
        )
        self._connection.row_factory = sqlite3.Row
        with self._lock:
            self._connection.execute("PRAGMA journal_mode=WAL")
            self._connection.execute("PRAGMA synchronous=FULL")
            self._connection.execute("PRAGMA busy_timeout=5000")
            self._connection.execute(
                """
                CREATE TABLE IF NOT EXISTS checkpoints (
                    order_index INTEGER PRIMARY KEY,
                    content_hash TEXT NOT NULL,
                    payload_json TEXT NOT NULL,
                    packet_seq INTEGER NOT NULL,
                    channel INTEGER NOT NULL,
                    capability_version INTEGER NOT NULL,
                    kernel_version TEXT NOT NULL
                )
                """
            )
            self._connection.execute(
                """
                CREATE TABLE IF NOT EXISTS metadata (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                )
                """
            )

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def __enter__(self) -> "CheckpointStore":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()

    def clear(self) -> None:
        with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                self._connection.execute("DELETE FROM checkpoints")
                self._connection.execute("DELETE FROM metadata")
                self._connection.execute("COMMIT")
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

    def commit_window(self, checkpoint: CheckpointWindow) -> str:
        with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                row = self._connection.execute(
                    "SELECT order_index, content_hash FROM checkpoints ORDER BY order_index DESC LIMIT 1"
                ).fetchone()
                expected_order = 0 if row is None else int(row["order_index"]) + 1
                if checkpoint.order_index != expected_order:
                    raise HapticError(
                        Issue(
                            ErrorCode.E_IO,
                            "checkpoint window order is not stable and sequential",
                            packet_seq=checkpoint.packet.packet_seq,
                            channel=checkpoint.packet.channel,
                            field="order_index",
                            details={
                                "expected": expected_order,
                                "actual": checkpoint.order_index,
                            },
                        )
                    )
                previous_hash = "" if row is None else str(row["content_hash"])
                payload = checkpoint.payload(previous_hash)
                payload_json = canonical_json(payload)
                content_hash = sha256_hex(previous_hash + payload_json)
                self._connection.execute(
                    """
                    INSERT INTO checkpoints(
                        order_index, content_hash, payload_json, packet_seq, channel,
                        capability_version, kernel_version
                    ) VALUES (?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        checkpoint.order_index,
                        content_hash,
                        payload_json,
                        checkpoint.packet.packet_seq,
                        checkpoint.packet.channel,
                        checkpoint.capability_version,
                        checkpoint.kernel_version,
                    ),
                )
                self._connection.execute(
                    "INSERT OR REPLACE INTO metadata(key, value) VALUES ('last_hash', ?)",
                    (content_hash,),
                )
                self._connection.execute("COMMIT")
                return content_hash
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

    def count(self) -> int:
        with self._lock:
            return int(
                self._connection.execute("SELECT COUNT(*) FROM checkpoints").fetchone()[0]
            )

    def load_valid(
        self,
        *,
        expected_capability_version: int,
        expected_kernel_version: str,
        invalidate_on_error: bool = True,
    ) -> LoadResult:
        with self._lock:
            rows = self._connection.execute(
                "SELECT * FROM checkpoints ORDER BY order_index ASC"
            ).fetchall()
            if not rows:
                return LoadResult(valid=True, checkpoints=())
            previous_hash = ""
            checkpoints: list[StoredCheckpoint] = []
            failure: Issue | None = None
            for expected_order, row in enumerate(rows):
                try:
                    if int(row["order_index"]) != expected_order:
                        raise ValueError(
                            f"non-contiguous checkpoint order {row['order_index']} at {expected_order}"
                        )
                    if int(row["capability_version"]) != expected_capability_version:
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint capability version does not match the active manifest",
                            field="capability_version",
                            details={
                                "stored": int(row["capability_version"]),
                                "active": expected_capability_version,
                            },
                        )
                        break
                    if str(row["kernel_version"]) != expected_kernel_version:
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint kernel version does not match the active kernel",
                            field="kernel_version",
                            details={
                                "stored": str(row["kernel_version"]),
                                "active": expected_kernel_version,
                            },
                        )
                        break
                    payload_json = str(row["payload_json"])
                    actual_hash = sha256_hex(previous_hash + payload_json)
                    if actual_hash != str(row["content_hash"]):
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint content hash mismatch",
                            field="content_hash",
                            details={
                                "order_index": expected_order,
                                "stored": str(row["content_hash"]),
                                "computed": actual_hash,
                            },
                        )
                        break
                    payload = json.loads(payload_json)
                    if canonical_json(payload) != payload_json:
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint payload is not canonical JSON",
                            field="payload_json",
                            details={"order_index": expected_order},
                        )
                        break
                    if str(payload.get("previous_hash", "")) != previous_hash:
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint hash chain predecessor mismatch",
                            field="previous_hash",
                            details={"order_index": expected_order},
                        )
                        break
                    packet = _packet_from_dict(payload["packet"])
                    if (
                        packet.packet_seq != int(row["packet_seq"])
                        or packet.channel != int(row["channel"])
                        or int(payload["capability_version"])
                        != int(row["capability_version"])
                        or str(payload["kernel_version"]) != str(row["kernel_version"])
                    ):
                        failure = Issue(
                            ErrorCode.E_HASH,
                            "checkpoint metadata columns disagree with the hashed payload",
                            field="packet_seq",
                            details={"order_index": expected_order},
                        )
                        break
                    decisions = tuple(
                        _decision_from_dict(item) for item in payload.get("decisions", [])
                    )
                    checkpoints.append(
                        StoredCheckpoint(
                            order_index=expected_order,
                            content_hash=actual_hash,
                            packet=packet,
                            decisions=decisions,
                            consumed_event_offset=int(payload["consumed_event_offset"]),
                            capability_version=int(payload["capability_version"]),
                            capability_fingerprint=str(payload["capability_fingerprint"]),
                            event_stream_hash=str(payload["event_stream_hash"]),
                            kernel_version=str(payload["kernel_version"]),
                        )
                    )
                    previous_hash = actual_hash
                except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
                    failure = Issue(
                        ErrorCode.E_HASH,
                        f"checkpoint payload is corrupt: {exc}",
                        field="payload_json",
                        details={"order_index": expected_order},
                    )
                    break
            if failure is not None:
                if invalidate_on_error:
                    self._connection.execute("BEGIN IMMEDIATE")
                    try:
                        self._connection.execute("DELETE FROM checkpoints")
                        self._connection.execute("DELETE FROM metadata")
                        self._connection.execute("COMMIT")
                    except Exception:
                        self._connection.execute("ROLLBACK")
                        raise
                return LoadResult(
                    valid=False,
                    checkpoints=(),
                    issue=failure,
                    invalidated=invalidate_on_error,
                )
            return LoadResult(valid=True, checkpoints=tuple(checkpoints))

    def inspect_raw(self) -> tuple[dict[str, Any], ...]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT * FROM checkpoints ORDER BY order_index ASC"
            ).fetchall()
            return tuple(
                {
                    "order_index": int(row["order_index"]),
                    "content_hash": str(row["content_hash"]),
                    "payload_json": str(row["payload_json"]),
                    "packet_seq": int(row["packet_seq"]),
                    "channel": int(row["channel"]),
                }
                for row in rows
            )

    def latest_hash(self) -> str:
        with self._lock:
            row = self._connection.execute(
                "SELECT value FROM metadata WHERE key='last_hash'"
            ).fetchone()
            return "" if row is None else str(row["value"])

