from __future__ import annotations

import json
import sqlite3
import threading
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from .auditor import AuditOutcome
from .common import BusinessError, utc_now


SCHEMA = """
CREATE TABLE IF NOT EXISTS schema_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chain_audits (
    audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
    chain_id TEXT NOT NULL,
    view_version INTEGER NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    created_at TEXT NOT NULL,
    status TEXT NOT NULL,
    increment_count INTEGER NOT NULL,
    usable_restore_points INTEGER NOT NULL,
    conflict_blocks INTEGER NOT NULL,
    missing_blocks INTEGER NOT NULL,
    half_written_blocks INTEGER NOT NULL,
    unconfirmed_blocks INTEGER NOT NULL,
    undeclared_copies INTEGER NOT NULL,
    last_usable_sequence INTEGER,
    breakpoint_json TEXT,
    summary_json TEXT NOT NULL,
    errors_json TEXT NOT NULL,
    UNIQUE(chain_id, view_version)
);

CREATE TABLE IF NOT EXISTS audited_blocks (
    audit_id INTEGER NOT NULL,
    sequence INTEGER NOT NULL,
    backup_id TEXT NOT NULL,
    block_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    expected_length INTEGER NOT NULL,
    expected_sha256 TEXT NOT NULL,
    expected_tail_sha256 TEXT,
    status TEXT NOT NULL,
    missing INTEGER NOT NULL,
    half_written INTEGER NOT NULL,
    conflict INTEGER NOT NULL,
    primary_code TEXT,
    selected_json TEXT,
    candidates_json TEXT NOT NULL,
    codes_json TEXT NOT NULL,
    PRIMARY KEY(audit_id, backup_id, block_id),
    FOREIGN KEY(audit_id) REFERENCES chain_audits(audit_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS restore_points (
    audit_id INTEGER NOT NULL,
    sequence INTEGER NOT NULL,
    backup_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    state_digest TEXT NOT NULL,
    usable INTEGER NOT NULL,
    block_count INTEGER NOT NULL,
    PRIMARY KEY(audit_id, sequence),
    FOREIGN KEY(audit_id) REFERENCES chain_audits(audit_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS restore_point_blocks (
    audit_id INTEGER NOT NULL,
    sequence INTEGER NOT NULL,
    block_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    origin_sequence INTEGER NOT NULL,
    origin_backup_id TEXT NOT NULL,
    length INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    tail_sha256 TEXT,
    path TEXT NOT NULL,
    partition TEXT NOT NULL,
    priority INTEGER NOT NULL,
    PRIMARY KEY(audit_id, sequence, block_id),
    FOREIGN KEY(audit_id, sequence)
        REFERENCES restore_points(audit_id, sequence) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS audit_errors (
    error_row_id INTEGER PRIMARY KEY AUTOINCREMENT,
    audit_id INTEGER NOT NULL,
    ordinal_sort INTEGER NOT NULL,
    code TEXT NOT NULL,
    sequence INTEGER,
    backup_id TEXT,
    block_id TEXT,
    path TEXT,
    severity TEXT NOT NULL,
    message TEXT NOT NULL,
    detail_json TEXT,
    FOREIGN KEY(audit_id) REFERENCES chain_audits(audit_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS injection_impacts (
    audit_id INTEGER PRIMARY KEY,
    event_id INTEGER,
    impact_json TEXT NOT NULL,
    FOREIGN KEY(audit_id) REFERENCES chain_audits(audit_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS drills (
    drill_id TEXT PRIMARY KEY,
    chain_id TEXT NOT NULL,
    audit_id INTEGER NOT NULL,
    sequence INTEGER NOT NULL,
    state_digest TEXT NOT NULL,
    selection_digest TEXT NOT NULL,
    status TEXT NOT NULL,
    block_count INTEGER NOT NULL,
    progress_blocks INTEGER NOT NULL,
    progress_bytes INTEGER NOT NULL,
    blocks_read INTEGER NOT NULL,
    blocks_written INTEGER NOT NULL,
    blocks_skipped INTEGER NOT NULL,
    bytes_read INTEGER NOT NULL,
    bytes_written INTEGER NOT NULL,
    resume_count INTEGER NOT NULL,
    last_progress_json TEXT,
    output_path TEXT,
    staging_path TEXT NOT NULL,
    failure_json TEXT,
    conclusion_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT,
    FOREIGN KEY(audit_id) REFERENCES chain_audits(audit_id)
);

CREATE TABLE IF NOT EXISTS drill_blocks (
    drill_id TEXT NOT NULL,
    block_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    path TEXT NOT NULL,
    expected_sha256 TEXT NOT NULL,
    actual_sha256 TEXT NOT NULL,
    length INTEGER NOT NULL,
    committed_at TEXT NOT NULL,
    PRIMARY KEY(drill_id, block_id),
    FOREIGN KEY(drill_id) REFERENCES drills(drill_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS drill_failures (
    failure_id INTEGER PRIMARY KEY AUTOINCREMENT,
    drill_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    block_id TEXT NOT NULL,
    path TEXT,
    expected_sha256 TEXT,
    actual_sha256 TEXT,
    code TEXT NOT NULL,
    detail_json TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(drill_id) REFERENCES drills(drill_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS drill_events (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    drill_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    from_progress_blocks INTEGER NOT NULL,
    progress_marker_json TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY(drill_id) REFERENCES drills(drill_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_audits_chain_version
    ON chain_audits(chain_id, view_version DESC);
CREATE INDEX IF NOT EXISTS idx_restore_blocks_order
    ON restore_point_blocks(audit_id, sequence, ordinal);
CREATE INDEX IF NOT EXISTS idx_drill_blocks_order
    ON drill_blocks(drill_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_drills_chain_status
    ON drills(chain_id, status);
"""


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _loads(value: str | None, default: Any = None) -> Any:
    if value is None:
        return default
    try:
        return json.loads(value)
    except (TypeError, json.JSONDecodeError):
        return default


class Store:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self.connection = sqlite3.connect(
            str(path), timeout=30.0, check_same_thread=False
        )
        self.connection.row_factory = sqlite3.Row
        self.connection.execute("PRAGMA journal_mode=WAL")
        self.connection.execute("PRAGMA synchronous=FULL")
        self.connection.execute("PRAGMA foreign_keys=ON")
        self.connection.execute("PRAGMA busy_timeout=30000")
        self.connection.executescript(SCHEMA)
        now = utc_now()
        self.connection.execute(
            "INSERT OR IGNORE INTO schema_meta(key, value) VALUES('schema_version', '1')"
        )
        self.connection.execute(
            "INSERT OR IGNORE INTO schema_meta(key, value) VALUES('created_at', ?)",
            (now,),
        )
        self.connection.commit()

    def close(self) -> None:
        with self._lock:
            self.connection.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                yield self.connection
            except BaseException:
                self.connection.rollback()
                raise
            else:
                self.connection.commit()

    def commit_audit(self, outcome: AuditOutcome, repo_path: Path) -> dict[str, Any]:
        catalog = outcome.catalog
        chain_id = str(catalog["chain_id"])
        now = utc_now()
        summary = outcome.summary()
        errors_json = _json(outcome.errors)
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT COALESCE(MAX(view_version), 0) AS version "
                "FROM chain_audits WHERE chain_id=?",
                (chain_id,),
            ).fetchone()
            version = int(row["version"]) + 1
            cursor = conn.execute(
                """
                INSERT INTO chain_audits(
                    chain_id, view_version, manifest_sha256, repo_path,
                    created_at, status, increment_count, usable_restore_points,
                    conflict_blocks, missing_blocks, half_written_blocks,
                    unconfirmed_blocks, undeclared_copies,
                    last_usable_sequence, breakpoint_json, summary_json,
                    errors_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    chain_id,
                    version,
                    catalog["manifest_sha256"],
                    str(repo_path),
                    now,
                    catalog["status"],
                    catalog["increment_count"],
                    len(catalog["usable_restore_points"]),
                    catalog["conflict_blocks"],
                    catalog["missing_blocks"],
                    catalog["half_written_blocks"],
                    catalog["unconfirmed_blocks"],
                    catalog["undeclared_copies"],
                    catalog["last_usable_sequence"],
                    _json(outcome.breakpoint) if outcome.breakpoint else None,
                    _json(summary),
                    errors_json,
                ),
            )
            audit_id = int(cursor.lastrowid)

            block_rows = []
            for block in outcome.blocks:
                block_rows.append(
                    (
                        audit_id,
                        block["sequence"],
                        block["backup_id"],
                        block["block_id"],
                        block["ordinal"],
                        block["expected"]["length"],
                        block["expected"]["sha256"],
                        block["expected"]["tail_sha256"],
                        block["status"],
                        1 if block["missing"] else 0,
                        1 if block["half_written"] else 0,
                        1 if block["conflict"] else 0,
                        block["primary_code"],
                        _json(block["selected"]) if block["selected"] else None,
                        _json(block["candidates"]),
                        _json(block["codes"]),
                    )
                )
            conn.executemany(
                """
                INSERT INTO audited_blocks(
                    audit_id, sequence, backup_id, block_id, ordinal,
                    expected_length, expected_sha256, expected_tail_sha256,
                    status, missing, half_written, conflict, primary_code,
                    selected_json, candidates_json, codes_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                block_rows,
            )

            point_rows = []
            point_block_rows = []
            for point in outcome.restore_points:
                point_rows.append(
                    (
                        audit_id,
                        point["sequence"],
                        point["backup_id"],
                        point["kind"],
                        point["state_digest"],
                        1 if point["usable"] else 0,
                        point["block_count"],
                    )
                )
                for block in outcome.restore_point_blocks[point["sequence"]]:
                    point_block_rows.append(
                        (
                            audit_id,
                            point["sequence"],
                            block["block_id"],
                            block["ordinal"],
                            block["origin_sequence"],
                            block["origin_backup_id"],
                            block["length"],
                            block["sha256"],
                            block["tail_sha256"],
                            block["path"],
                            block["partition"],
                            block["priority"],
                        )
                    )
            conn.executemany(
                """
                INSERT INTO restore_points(
                    audit_id, sequence, backup_id, kind, state_digest,
                    usable, block_count
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                point_rows,
            )
            conn.executemany(
                """
                INSERT INTO restore_point_blocks(
                    audit_id, sequence, block_id, ordinal, origin_sequence,
                    origin_backup_id, length, sha256, tail_sha256, path,
                    partition, priority
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                point_block_rows,
            )

            error_rows = []
            for sort_index, error in enumerate(outcome.errors):
                error_rows.append(
                    (
                        audit_id,
                        sort_index,
                        error["code"],
                        error.get("sequence"),
                        error.get("backup_id"),
                        error.get("block_id"),
                        error.get("path"),
                        error.get("severity", "error"),
                        error["message"],
                        _json(error.get("detail")),
                    )
                )
            conn.executemany(
                """
                INSERT INTO audit_errors(
                    audit_id, ordinal_sort, code, sequence, backup_id,
                    block_id, path, severity, message, detail_json
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                error_rows,
            )

            if outcome.injection_impact is not None:
                event = outcome.injection_impact.get("event") or {}
                event_id = event.get("id")
                conn.execute(
                    """
                    INSERT INTO injection_impacts(audit_id, event_id, impact_json)
                    VALUES (?, ?, ?)
                    """,
                    (
                        audit_id,
                        int(event_id) if event_id is not None else None,
                        _json(outcome.injection_impact),
                    ),
                )

        return {
            "audit_id": audit_id,
            "chain_id": chain_id,
            "view_version": version,
        }

    def _audit_row(self, row: sqlite3.Row) -> dict[str, Any]:
        return {
            "audit_id": int(row["audit_id"]),
            "breakpoint": _loads(row["breakpoint_json"]),
            "chain_id": row["chain_id"],
            "conflict_blocks": int(row["conflict_blocks"]),
            "created_at": row["created_at"],
            "errors": _loads(row["errors_json"], []),
            "half_written_blocks": int(row["half_written_blocks"]),
            "increment_count": int(row["increment_count"]),
            "last_usable_sequence": row["last_usable_sequence"],
            "manifest_sha256": row["manifest_sha256"],
            "missing_blocks": int(row["missing_blocks"]),
            "repo_path": row["repo_path"],
            "status": row["status"],
            "summary": _loads(row["summary_json"], {}),
            "undeclared_copies": int(row["undeclared_copies"]),
            "unconfirmed_blocks": int(row["unconfirmed_blocks"]),
            "usable_restore_points": int(row["usable_restore_points"]),
            "view_version": int(row["view_version"]),
        }

    def get_audit(
        self, *, audit_id: int | None = None, chain_id: str | None = None
    ) -> dict[str, Any] | None:
        with self._lock:
            if audit_id is not None:
                row = self.connection.execute(
                    "SELECT * FROM chain_audits WHERE audit_id=?", (audit_id,)
                ).fetchone()
            elif chain_id is not None:
                row = self.connection.execute(
                    "SELECT * FROM chain_audits WHERE chain_id=? "
                    "ORDER BY view_version DESC LIMIT 1",
                    (chain_id,),
                ).fetchone()
            else:
                row = self.connection.execute(
                    "SELECT * FROM chain_audits ORDER BY audit_id DESC LIMIT 1"
                ).fetchone()
        if row is None:
            return None
        result = self._audit_row(row)
        summary = result["summary"]
        points = summary.get("usable_restore_points", [])
        if isinstance(points, list):
            casts = [int(value) for value in points]
        else:
            casts = []
        result["summary"]["usable_restore_points"] = casts
        return result

    def list_chains(self) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT a.* FROM chain_audits a
                JOIN (
                    SELECT chain_id, MAX(view_version) AS version
                    FROM chain_audits GROUP BY chain_id
                ) latest
                ON a.chain_id=latest.chain_id AND a.view_version=latest.version
                ORDER BY a.chain_id
                """
            ).fetchall()
        return [self._audit_row(row) for row in rows]

    def restore_point(
        self, audit_id: int, sequence: int
    ) -> dict[str, Any] | None:
        with self._lock:
            row = self.connection.execute(
                """
                SELECT * FROM restore_points
                WHERE audit_id=? AND sequence=?
                """,
                (audit_id, sequence),
            ).fetchone()
        if row is None:
            return None
        return {
            "audit_id": int(row["audit_id"]),
            "backup_id": row["backup_id"],
            "block_count": int(row["block_count"]),
            "kind": row["kind"],
            "sequence": int(row["sequence"]),
            "state_digest": row["state_digest"],
            "usable": bool(row["usable"]),
        }

    def restore_blocks(
        self, audit_id: int, sequence: int
    ) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT * FROM restore_point_blocks
                WHERE audit_id=? AND sequence=?
                ORDER BY ordinal, block_id
                """,
                (audit_id, sequence),
            ).fetchall()
        return [
            {
                "block_id": row["block_id"],
                "length": int(row["length"]),
                "ordinal": int(row["ordinal"]),
                "origin_backup_id": row["origin_backup_id"],
                "origin_sequence": int(row["origin_sequence"]),
                "partition": row["partition"],
                "path": row["path"],
                "priority": int(row["priority"]),
                "sha256": row["sha256"],
                "tail_sha256": row["tail_sha256"],
            }
            for row in rows
        ]

    def audit_blocks(self, audit_id: int) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT * FROM audited_blocks WHERE audit_id=?
                ORDER BY sequence, ordinal, block_id
                """,
                (audit_id,),
            ).fetchall()
        return [
            {
                "backup_id": row["backup_id"],
                "block_id": row["block_id"],
                "candidates": _loads(row["candidates_json"], []),
                "codes": _loads(row["codes_json"], []),
                "conflict": bool(row["conflict"]),
                "expected": {
                    "length": int(row["expected_length"]),
                    "sha256": row["expected_sha256"],
                    "tail_sha256": row["expected_tail_sha256"],
                },
                "half_written": bool(row["half_written"]),
                "missing": bool(row["missing"]),
                "ordinal": int(row["ordinal"]),
                "primary_code": row["primary_code"],
                "selected": _loads(row["selected_json"]),
                "sequence": int(row["sequence"]),
                "status": row["status"],
            }
            for row in rows
        ]

    def latest_injection_impact(self, audit_id: int) -> dict[str, Any] | None:
        with self._lock:
            row = self.connection.execute(
                "SELECT impact_json FROM injection_impacts WHERE audit_id=?",
                (audit_id,),
            ).fetchone()
        return _loads(row["impact_json"]) if row else None

    def _drill_row(self, row: sqlite3.Row) -> dict[str, Any]:
        return {
            "audit_id": int(row["audit_id"]),
            "block_count": int(row["block_count"]),
            "blocks_read": int(row["blocks_read"]),
            "blocks_skipped": int(row["blocks_skipped"]),
            "blocks_written": int(row["blocks_written"]),
            "bytes_read": int(row["bytes_read"]),
            "bytes_written": int(row["bytes_written"]),
            "chain_id": row["chain_id"],
            "completed_at": row["completed_at"],
            "conclusion": _loads(row["conclusion_json"]),
            "created_at": row["created_at"],
            "drill_id": row["drill_id"],
            "failure": _loads(row["failure_json"]),
            "last_progress": _loads(row["last_progress_json"]),
            "output_path": row["output_path"],
            "progress_blocks": int(row["progress_blocks"]),
            "progress_bytes": int(row["progress_bytes"]),
            "resume_count": int(row["resume_count"]),
            "selection_digest": row["selection_digest"],
            "sequence": int(row["sequence"]),
            "state_digest": row["state_digest"],
            "status": row["status"],
            "staging_path": row["staging_path"],
            "updated_at": row["updated_at"],
        }

    def get_drill(self, drill_id: str) -> dict[str, Any] | None:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
        return self._drill_row(row) if row else None

    def latest_drill_for_point(
        self, audit_id: int, sequence: int
    ) -> dict[str, Any] | None:
        with self._lock:
            row = self.connection.execute(
                """
                SELECT * FROM drills
                WHERE audit_id=? AND sequence=?
                ORDER BY
                    CASE WHEN status='completed' THEN 0 ELSE 1 END,
                    updated_at DESC
                LIMIT 1
                """,
                (audit_id, sequence),
            ).fetchone()
        return self._drill_row(row) if row else None

    def committed_drill_blocks(self, drill_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT * FROM drill_blocks WHERE drill_id=?
                ORDER BY ordinal, block_id
                """,
                (drill_id,),
            ).fetchall()
        return [
            {
                "actual_sha256": row["actual_sha256"],
                "block_id": row["block_id"],
                "committed_at": row["committed_at"],
                "expected_sha256": row["expected_sha256"],
                "length": int(row["length"]),
                "ordinal": int(row["ordinal"]),
                "path": row["path"],
            }
            for row in rows
        ]

    def drill_failures(self, drill_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT * FROM drill_failures WHERE drill_id=?
                ORDER BY failure_id
                """,
                (drill_id,),
            ).fetchall()
        return [
            {
                "actual_sha256": row["actual_sha256"],
                "block_id": row["block_id"],
                "code": row["code"],
                "created_at": row["created_at"],
                "detail": _loads(row["detail_json"]),
                "expected_sha256": row["expected_sha256"],
                "ordinal": int(row["ordinal"]),
                "path": row["path"],
            }
            for row in rows
        ]

    def drill_events(self, drill_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT * FROM drill_events WHERE drill_id=?
                ORDER BY event_id
                """,
                (drill_id,),
            ).fetchall()
        return [
            {
                "created_at": row["created_at"],
                "from_progress_blocks": int(row["from_progress_blocks"]),
                "kind": row["kind"],
                "progress_marker": _loads(row["progress_marker_json"]),
            }
            for row in rows
        ]

    def begin_drill(
        self,
        *,
        drill_id: str,
        chain_id: str,
        audit_id: int,
        sequence: int,
        state_digest: str,
        selection_digest: str,
        block_count: int,
        staging_path: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]], bool]:
        now = utc_now()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
            if row is not None:
                if row["status"] == "completed":
                    committed = conn.execute(
                        "SELECT * FROM drill_blocks WHERE drill_id=? "
                        "ORDER BY ordinal, block_id",
                        (drill_id,),
                    ).fetchall()
                    return (
                        self._drill_row(row),
                        [dict(item) for item in committed],
                        True,
                    )
                if (
                    row["chain_id"] != chain_id
                    or int(row["audit_id"]) != audit_id
                    or int(row["sequence"]) != sequence
                    or row["state_digest"] != state_digest
                    or row["selection_digest"] != selection_digest
                    or int(row["block_count"]) != block_count
                ):
                    raise BusinessError(
                        "BC_DRILL_ID_CONFLICT",
                        "the deterministic drill identifier maps to different input",
                        drill_id=drill_id,
                    )
                from_progress = int(row["progress_blocks"])
                marker = _loads(row["last_progress_json"])
                conn.execute(
                    """
                    UPDATE drills SET
                        status='running',
                        resume_count=resume_count+1,
                        blocks_skipped=blocks_skipped+progress_blocks,
                        updated_at=?,
                        failure_json=NULL
                    WHERE drill_id=?
                    """,
                    (now, drill_id),
                )
                conn.execute(
                    """
                    INSERT INTO drill_events(
                        drill_id, kind, from_progress_blocks,
                        progress_marker_json, created_at
                    ) VALUES (?, 'resume', ?, ?, ?)
                    """,
                    (drill_id, from_progress, _json(marker), now),
                )
                updated = conn.execute(
                    "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
                ).fetchone()
                committed = conn.execute(
                    "SELECT * FROM drill_blocks WHERE drill_id=? "
                    "ORDER BY ordinal, block_id",
                    (drill_id,),
                ).fetchall()
                return self._drill_row(updated), [dict(item) for item in committed], False

            conn.execute(
                """
                INSERT INTO drills(
                    drill_id, chain_id, audit_id, sequence, state_digest,
                    selection_digest, status, block_count, progress_blocks,
                    progress_bytes, blocks_read, blocks_written,
                    blocks_skipped, bytes_read, bytes_written, resume_count,
                    last_progress_json, output_path, staging_path,
                    failure_json, conclusion_json, created_at, updated_at,
                    completed_at
                ) VALUES (
                    ?, ?, ?, ?, ?, ?, 'running', ?, 0, 0, 0, 0,
                    0, 0, 0, 0, NULL, NULL, ?, NULL, NULL, ?, ?, NULL
                )
                """,
                (
                    drill_id,
                    chain_id,
                    audit_id,
                    sequence,
                    state_digest,
                    selection_digest,
                    block_count,
                    staging_path,
                    now,
                    now,
                ),
            )
            conn.execute(
                """
                INSERT INTO drill_events(
                    drill_id, kind, from_progress_blocks,
                    progress_marker_json, created_at
                ) VALUES (?, 'start', 0, NULL, ?)
                """,
                (drill_id, now),
            )
            row = conn.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
        return self._drill_row(row), [], False

    def commit_drill_block(
        self,
        *,
        drill_id: str,
        block: dict[str, Any],
        actual_sha256: str,
        read_bytes: int,
        write_bytes: int,
        complete: bool = False,
        output_path: str | None = None,
        conclusion: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        now = utc_now()
        marker = {
            "block_id": block["block_id"],
            "ordinal": block["ordinal"],
            "sha256": actual_sha256,
        }
        with self.transaction() as conn:
            existing = conn.execute(
                "SELECT status FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
            if existing is None:
                raise BusinessError(
                    "BC_DRILL_NOT_FOUND", "drill does not exist", drill_id=drill_id
                )
            if existing["status"] == "completed":
                raise BusinessError(
                    "BC_DRILL_ALREADY_COMPLETED",
                    "completed drill progress cannot be overwritten",
                    drill_id=drill_id,
                )
            duplicate = conn.execute(
                "SELECT 1 FROM drill_blocks WHERE drill_id=? AND block_id=?",
                (drill_id, block["block_id"]),
            ).fetchone()
            if duplicate:
                raise BusinessError(
                    "BC_DRILL_BLOCK_ALREADY_COMMITTED",
                    "committed block cannot be written or committed twice",
                    drill_id=drill_id,
                    block_id=block["block_id"],
                )
            conn.execute(
                """
                INSERT INTO drill_blocks(
                    drill_id, block_id, ordinal, path, expected_sha256,
                    actual_sha256, length, committed_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    drill_id,
                    block["block_id"],
                    block["ordinal"],
                    block["path"],
                    block["sha256"],
                    actual_sha256,
                    block["length"],
                    now,
                ),
            )
            progress_blocks = conn.execute(
                "SELECT COUNT(*) AS count FROM drill_blocks WHERE drill_id=?",
                (drill_id,),
            ).fetchone()["count"]
            progress_bytes = conn.execute(
                "SELECT COALESCE(SUM(length), 0) AS total FROM drill_blocks "
                "WHERE drill_id=?",
                (drill_id,),
            ).fetchone()["total"]
            marker["progress_blocks"] = int(progress_blocks)
            marker["progress_bytes"] = int(progress_bytes)
            if complete:
                conn.execute(
                    """
                    UPDATE drills SET
                        status='completed',
                        progress_blocks=?,
                        progress_bytes=?,
                        blocks_read=blocks_read+1,
                        blocks_written=blocks_written+1,
                        bytes_read=bytes_read+?,
                        bytes_written=bytes_written+?,
                        last_progress_json=?,
                        output_path=?,
                        conclusion_json=?,
                        completed_at=?,
                        updated_at=?,
                        failure_json=NULL
                    WHERE drill_id=?
                    """,
                    (
                        progress_blocks,
                        progress_bytes,
                        read_bytes,
                        write_bytes,
                        _json(marker),
                        output_path,
                        _json(conclusion),
                        now,
                        now,
                        drill_id,
                    ),
                )
                conn.execute(
                    """
                    INSERT INTO drill_events(
                        drill_id, kind, from_progress_blocks,
                        progress_marker_json, created_at
                    ) VALUES (?, 'complete', ?, ?, ?)
                    """,
                    (drill_id, int(progress_blocks) - 1, _json(marker), now),
                )
            else:
                conn.execute(
                    """
                    UPDATE drills SET
                        progress_blocks=?,
                        progress_bytes=?,
                        blocks_read=blocks_read+1,
                        blocks_written=blocks_written+1,
                        bytes_read=bytes_read+?,
                        bytes_written=bytes_written+?,
                        last_progress_json=?,
                        updated_at=?
                    WHERE drill_id=?
                    """,
                    (
                        progress_blocks,
                        progress_bytes,
                        read_bytes,
                        write_bytes,
                        _json(marker),
                        now,
                        drill_id,
                    ),
                )
            row = conn.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
        return self._drill_row(row)

    def mark_drill_interrupted(
        self,
        drill_id: str,
        *,
        reason: str,
        read_bytes: int = 0,
        write_bytes: int = 0,
    ) -> dict[str, Any]:
        now = utc_now()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT status FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
            if row is None:
                raise BusinessError(
                    "BC_DRILL_NOT_FOUND", "drill does not exist", drill_id=drill_id
                )
            if row["status"] == "completed":
                return dict(row)
            conn.execute(
                """
                UPDATE drills SET
                    status='interrupted',
                    blocks_read=blocks_read+(? > 0),
                    blocks_written=blocks_written+(? > 0),
                    bytes_read=bytes_read+?,
                    bytes_written=bytes_written+?,
                    updated_at=?
                WHERE drill_id=?
                """,
                (read_bytes, write_bytes, read_bytes, write_bytes, now, drill_id),
            )
            conn.execute(
                """
                INSERT INTO drill_events(
                    drill_id, kind, from_progress_blocks,
                    progress_marker_json, created_at
                ) VALUES (?, 'interrupted', 0, ?, ?)
                """,
                (
                    drill_id,
                    _json({"reason": reason}),
                    now,
                ),
            )
            updated = conn.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
        return self._drill_row(updated)

    def fail_drill_block(
        self,
        *,
        drill_id: str,
        block: dict[str, Any],
        code: str,
        actual_sha256: str | None,
        detail: Any,
        read_bytes: int,
        write_bytes: int,
    ) -> dict[str, Any]:
        now = utc_now()
        with self.transaction() as conn:
            row = conn.execute(
                "SELECT status FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
            if row is None:
                raise BusinessError(
                    "BC_DRILL_NOT_FOUND", "drill does not exist", drill_id=drill_id
                )
            if row["status"] == "completed":
                raise BusinessError(
                    "BC_DRILL_ALREADY_COMPLETED",
                    "completed drill conclusion cannot be overwritten",
                    drill_id=drill_id,
                )
            conn.execute(
                """
                INSERT INTO drill_failures(
                    drill_id, ordinal, block_id, path, expected_sha256,
                    actual_sha256, code, detail_json, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    drill_id,
                    block["ordinal"],
                    block["block_id"],
                    block["path"],
                    block["sha256"],
                    actual_sha256,
                    code,
                    _json(detail),
                    now,
                ),
            )
            failure = {
                "actual_sha256": actual_sha256,
                "block_id": block["block_id"],
                "code": code,
                "detail": detail,
                "expected_sha256": block["sha256"],
                "ordinal": block["ordinal"],
                "path": block["path"],
            }
            conn.execute(
                """
                UPDATE drills SET
                    status='failed',
                    blocks_read=blocks_read+1,
                    blocks_written=blocks_written+(? > 0),
                    bytes_read=bytes_read+?,
                    bytes_written=bytes_written+?,
                    failure_json=?,
                    updated_at=?
                WHERE drill_id=?
                """,
                (
                    write_bytes,
                    read_bytes,
                    write_bytes,
                    _json(failure),
                    now,
                    drill_id,
                ),
            )
            conn.execute(
                """
                INSERT INTO drill_events(
                    drill_id, kind, from_progress_blocks,
                    progress_marker_json, created_at
                ) VALUES (?, 'failed', 0, ?, ?)
                """,
                (drill_id, _json(failure), now),
            )
            updated = conn.execute(
                "SELECT * FROM drills WHERE drill_id=?", (drill_id,)
            ).fetchone()
        return self._drill_row(updated)

    def metrics(self) -> list[dict[str, Any]]:
        with self._lock:
            audits = self.connection.execute(
                """
                SELECT a.* FROM chain_audits a
                JOIN (
                    SELECT chain_id, MAX(view_version) AS version
                    FROM chain_audits GROUP BY chain_id
                ) latest
                ON a.chain_id=latest.chain_id AND a.view_version=latest.version
                ORDER BY a.chain_id
                """
            ).fetchall()
            rows = []
            for audit in audits:
                completed = self.connection.execute(
                    "SELECT COUNT(*) AS count FROM drills "
                    "WHERE chain_id=? AND status='completed'",
                    (audit["chain_id"],),
                ).fetchone()["count"]
                rows.append(
                    {
                        "audit_id": int(audit["audit_id"]),
                        "chain_id": audit["chain_id"],
                        "committed_drills": int(completed),
                        "conflict_blocks": int(audit["conflict_blocks"]),
                        "half_written_blocks": int(audit["half_written_blocks"]),
                        "increment_count": int(audit["increment_count"]),
                        "missing_blocks": int(audit["missing_blocks"]),
                        "usable_restore_points": int(
                            audit["usable_restore_points"]
                        ),
                        "view_version": int(audit["view_version"]),
                    }
                )
        return rows
