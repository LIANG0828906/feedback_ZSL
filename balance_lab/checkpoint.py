from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass, field
from pathlib import Path
from threading import RLock
from typing import Any

from .budget import SimulationRequest
from .errors import BalanceError, ErrorCode
from .models import CandidateSet, ComboResult, Project
from .util import content_hash


ALGORITHM_VERSION = "fixedpoint-combat-v1"


@dataclass(frozen=True)
class SnapshotContext:
    project_hash: str
    budget_version: str
    algorithm_version: str
    candidate_set_hash: str
    schema_version: str

    @classmethod
    def create(
        cls,
        project: Project,
        candidate_set: CandidateSet,
        request: SimulationRequest,
    ) -> "SnapshotContext":
        budget_version = content_hash(
            {
                "budget": project.validation_budget.to_dict(),
                "request": {
                    "target_samples": request.target_samples,
                    "min_samples": request.min_samples,
                    "total_sample_budget": request.total_sample_budget,
                },
            }
        )
        return cls(
            project_hash=project.content_hash(),
            budget_version=budget_version,
            algorithm_version=ALGORITHM_VERSION,
            candidate_set_hash=candidate_set.content_hash(),
            schema_version=str(project.schema_version),
        )

    def to_dict(self) -> dict[str, str]:
        return {
            "project_hash": self.project_hash,
            "budget_version": self.budget_version,
            "algorithm_version": self.algorithm_version,
            "candidate_set_hash": self.candidate_set_hash,
            "schema_version": self.schema_version,
        }

    def fingerprint(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class SnapshotLoad:
    valid: bool
    invalidated: bool
    reason: str
    combo_results: tuple[ComboResult, ...] = ()
    candidate_commits: frozenset[str] = field(default_factory=frozenset)
    last_sequence: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "invalidated": self.invalidated,
            "reason": self.reason,
            "combo_count": len(self.combo_results),
            "candidate_commit_count": len(self.candidate_commits),
            "last_sequence": self.last_sequence,
        }


class CheckpointStore:
    """SQLite-backed, atomic, append-only checkpoint stream.

    A process-wide lock plus ``BEGIN IMMEDIATE`` prevents interleaved writes
    from scheduler workers sharing one store.  Every committed row carries the
    complete version envelope; loading never accepts a mixed snapshot.
    """

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = RLock()
        self._connection = sqlite3.connect(
            self.path,
            timeout=30.0,
            isolation_level=None,
            check_same_thread=False,
        )
        self._connection.row_factory = sqlite3.Row
        self._initialize()

    def _initialize(self) -> None:
        with self._lock:
            self._connection.executescript(
                """
                PRAGMA journal_mode=WAL;
                PRAGMA synchronous=FULL;
                CREATE TABLE IF NOT EXISTS commits (
                    seq INTEGER PRIMARY KEY AUTOINCREMENT,
                    kind TEXT NOT NULL,
                    candidate_id TEXT NOT NULL,
                    scenario_id TEXT,
                    payload TEXT NOT NULL,
                    content_hash TEXT NOT NULL,
                    project_hash TEXT NOT NULL,
                    budget_version TEXT NOT NULL,
                    algorithm_version TEXT NOT NULL,
                    candidate_set_hash TEXT NOT NULL,
                    schema_version TEXT NOT NULL,
                    UNIQUE(kind, candidate_id, scenario_id)
                );
                CREATE INDEX IF NOT EXISTS idx_commits_order ON commits(seq);
                """
            )

    def close(self) -> None:
        with self._lock:
            self._connection.close()

    def _invalidate(self) -> None:
        with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                self._connection.execute("DELETE FROM commits")
                self._connection.execute("DELETE FROM sqlite_sequence WHERE name='commits'")
                self._connection.execute("COMMIT")
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

    def clear(self) -> None:
        self._invalidate()

    def load(self, context: SnapshotContext) -> SnapshotLoad:
        with self._lock:
            rows = list(self._connection.execute("SELECT * FROM commits ORDER BY seq ASC"))
            if not rows:
                return SnapshotLoad(valid=True, invalidated=False, reason="empty")
            expected = context.to_dict()
            reason = ""
            for row in rows:
                for key, value in expected.items():
                    if row[key] != value:
                        reason = f"{key} mismatch at sequence {row['seq']}"
                        break
                if reason:
                    break
                payload = json.loads(row["payload"])
                unsigned = dict(payload)
                stored_hash = unsigned.pop("content_hash", "")
                hash_mismatch = stored_hash and stored_hash != row["content_hash"]
                if hash_mismatch or content_hash(unsigned) != row["content_hash"]:
                    reason = f"payload hash mismatch at sequence {row['seq']}"
                    break
            if reason:
                self._invalidate()
                return SnapshotLoad(valid=False, invalidated=True, reason=reason)

            combo_results: list[ComboResult] = []
            candidate_commits: set[str] = set()
            expected_seq = 1
            try:
                for row in rows:
                    if row["seq"] != expected_seq:
                        reason = f"non-contiguous sequence at {row['seq']}"
                        break
                    expected_seq += 1
                    payload = json.loads(row["payload"])
                    if row["kind"] == "combo":
                        result = ComboResult.from_dict(payload)
                        if (
                            result.candidate_id != row["candidate_id"]
                            or result.scenario_id != row["scenario_id"]
                        ):
                            reason = f"combo identity mismatch at sequence {row['seq']}"
                            break
                        combo_results.append(result)
                    elif row["kind"] == "candidate":
                        candidate_commits.add(row["candidate_id"])
                    else:
                        reason = f"unknown commit kind at sequence {row['seq']}"
                        break
            except (KeyError, TypeError, ValueError) as exc:
                reason = f"malformed payload at sequence {row['seq']}: {exc}"
            if reason:
                self._invalidate()
                return SnapshotLoad(valid=False, invalidated=True, reason=reason)
            return SnapshotLoad(
                valid=True,
                invalidated=False,
                reason="ok",
                combo_results=tuple(combo_results),
                candidate_commits=frozenset(candidate_commits),
                last_sequence=rows[-1]["seq"],
            )

    def _commit(
        self,
        context: SnapshotContext,
        *,
        kind: str,
        candidate_id: str,
        scenario_id: str | None,
        payload: dict[str, Any],
    ) -> int:
        unsigned = dict(payload)
        unsigned.pop("content_hash", None)
        digest = content_hash(unsigned)
        with self._lock:
            self._connection.execute("BEGIN IMMEDIATE")
            try:
                cursor = self._connection.execute(
                    """
                    INSERT INTO commits(
                        kind, candidate_id, scenario_id, payload, content_hash,
                        project_hash, budget_version, algorithm_version,
                        candidate_set_hash, schema_version
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        kind,
                        candidate_id,
                        scenario_id,
                        json.dumps(unsigned, ensure_ascii=False, sort_keys=True, separators=(",", ":")),
                        digest,
                        context.project_hash,
                        context.budget_version,
                        context.algorithm_version,
                        context.candidate_set_hash,
                        context.schema_version,
                    ),
                )
                seq = int(cursor.lastrowid)
                self._connection.execute("COMMIT")
                return seq
            except sqlite3.IntegrityError as exc:
                self._connection.execute("ROLLBACK")
                raise BalanceError(
                    ErrorCode.STATE,
                    f"checkpoint entry already committed: {exc}",
                    f"{candidate_id}::{scenario_id or '-'}",
                    "checkpoint",
                ) from exc
            except Exception:
                self._connection.execute("ROLLBACK")
                raise

    def commit_combo(
        self,
        context: SnapshotContext,
        result: ComboResult,
    ) -> int:
        return self._commit(
            context,
            kind="combo",
            candidate_id=result.candidate_id,
            scenario_id=result.scenario_id,
            payload=result.to_dict(),
        )

    def commit_candidate(
        self,
        context: SnapshotContext,
        candidate_id: str,
        payload: dict[str, Any],
    ) -> int:
        body = dict(payload)
        body["candidate_id"] = candidate_id
        body["content_hash"] = content_hash(body)
        return self._commit(
            context,
            kind="candidate",
            candidate_id=candidate_id,
            scenario_id=None,
            payload=body,
        )

    def count(self) -> int:
        with self._lock:
            row = self._connection.execute("SELECT COUNT(*) AS n FROM commits").fetchone()
            return int(row["n"])
