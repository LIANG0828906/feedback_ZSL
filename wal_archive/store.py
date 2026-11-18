"""SQLite catalog for archive versions, validations and recovery points."""

from __future__ import annotations

import json
import os
import sqlite3
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Sequence

from .archive import (
    InProgressSegment,
    Quarantine,
    ScanError,
    ScanSnapshot,
    ScanVersion,
    TimelineMarker,
)
from .errors import E_DATABASE_BUSY, E_DATABASE_ERROR, err


SCHEMA_VERSION = 1


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace(
        "+00:00", "Z"
    )


def _json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _loads(value: str | None, default: Any) -> Any:
    if not value:
        return default
    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return default


@dataclass
class ScanRecord:
    id: int
    started_at: str
    finished_at: str
    duration_ms: int
    archive_id: str
    root_label: str
    segment_files: int
    config: dict[str, Any]


class Store:
    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        if self.path.parent and not self.path.parent.exists():
            self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        try:
            self._conn = sqlite3.connect(
                str(self.path),
                timeout=30.0,
                isolation_level=None,
                check_same_thread=False,
            )
        except sqlite3.Error as exc:
            code, name = E_DATABASE_ERROR
            raise err(code, name, f"unable to open SQLite database: {exc}") from exc
        self._conn.row_factory = sqlite3.Row
        try:
            self._conn.execute("PRAGMA busy_timeout = 30000")
            self._conn.execute("PRAGMA foreign_keys = ON")
            self._conn.execute("PRAGMA journal_mode = WAL")
            self._conn.execute("PRAGMA synchronous = FULL")
            self._initialize()
        except sqlite3.Error as exc:
            code, name = E_DATABASE_ERROR
            raise err(code, name, f"unable to initialize SQLite database: {exc}") from exc

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def _initialize(self) -> None:
        with self._lock:
            self._conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS scans (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    started_at TEXT NOT NULL,
                    finished_at TEXT NOT NULL,
                    duration_ms INTEGER NOT NULL,
                    archive_id TEXT NOT NULL,
                    root_label TEXT NOT NULL,
                    segment_files INTEGER NOT NULL,
                    config_json TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS scan_errors (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    scan_id INTEGER NOT NULL,
                    code TEXT NOT NULL,
                    name TEXT NOT NULL,
                    message TEXT NOT NULL,
                    rel_path TEXT NOT NULL,
                    details_json TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_scan_errors_scan
                    ON scan_errors(scan_id, code, rel_path);

                CREATE TABLE IF NOT EXISTS timeline_markers (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    timeline INTEGER NOT NULL,
                    parent_timeline INTEGER,
                    switch_segment INTEGER,
                    switch_lsn TEXT,
                    switched_at TEXT NOT NULL,
                    authoritative_instance TEXT,
                    source_instance TEXT,
                    reason TEXT,
                    rel_path TEXT NOT NULL,
                    digest TEXT NOT NULL,
                    raw_json TEXT NOT NULL,
                    first_scan_id INTEGER NOT NULL,
                    last_scan_id INTEGER NOT NULL,
                    UNIQUE(rel_path, digest)
                );
                CREATE INDEX IF NOT EXISTS idx_markers_scan
                    ON timeline_markers(last_scan_id, timeline);

                CREATE TABLE IF NOT EXISTS wal_versions (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    instance TEXT NOT NULL,
                    timeline INTEGER NOT NULL,
                    segno INTEGER NOT NULL,
                    filename TEXT NOT NULL,
                    rel_path TEXT NOT NULL,
                    digest TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    mtime_ns INTEGER NOT NULL,
                    archived_at TEXT NOT NULL,
                    classification TEXT NOT NULL,
                    usable INTEGER NOT NULL,
                    conflict INTEGER NOT NULL DEFAULT 0,
                    header_ok INTEGER NOT NULL,
                    length_ok INTEGER NOT NULL,
                    pages_total INTEGER NOT NULL,
                    pages_parsed INTEGER NOT NULL,
                    pages_checked INTEGER NOT NULL,
                    pages_ok INTEGER NOT NULL,
                    pages_bad INTEGER NOT NULL,
                    pages_unchecked INTEGER NOT NULL,
                    checksum_pass_rate REAL NOT NULL,
                    reason TEXT NOT NULL,
                    details_json TEXT NOT NULL,
                    first_scan_id INTEGER NOT NULL,
                    last_scan_id INTEGER NOT NULL,
                    UNIQUE(instance, timeline, segno, digest)
                );
                CREATE INDEX IF NOT EXISTS idx_versions_scan
                    ON wal_versions(last_scan_id, instance, timeline, segno);
                CREATE INDEX IF NOT EXISTS idx_versions_slot
                    ON wal_versions(timeline, segno, classification);

                CREATE TABLE IF NOT EXISTS in_progress (
                    scan_id INTEGER NOT NULL,
                    instance TEXT NOT NULL,
                    filename TEXT NOT NULL,
                    rel_path TEXT NOT NULL,
                    reason TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    mtime_ns INTEGER NOT NULL,
                    timeline INTEGER,
                    segno INTEGER,
                    details_json TEXT NOT NULL,
                    PRIMARY KEY(scan_id, rel_path)
                );
                CREATE INDEX IF NOT EXISTS idx_in_progress_scan
                    ON in_progress(scan_id, instance);

                CREATE TABLE IF NOT EXISTS quarantine (
                    instance TEXT NOT NULL,
                    filename TEXT NOT NULL,
                    rel_path TEXT NOT NULL,
                    digest TEXT NOT NULL,
                    classification TEXT NOT NULL,
                    first_scan_id INTEGER NOT NULL,
                    created_at TEXT NOT NULL,
                    PRIMARY KEY(instance, filename)
                );

                CREATE TABLE IF NOT EXISTS recovery_points (
                    seq INTEGER PRIMARY KEY AUTOINCREMENT,
                    id TEXT NOT NULL UNIQUE,
                    request_key TEXT NOT NULL UNIQUE,
                    status TEXT NOT NULL,
                    instance TEXT NOT NULL,
                    timeline INTEGER NOT NULL,
                    target_time TEXT NOT NULL,
                    source_instance TEXT,
                    head_segno INTEGER NOT NULL,
                    chain_length INTEGER NOT NULL,
                    chain_json TEXT NOT NULL,
                    gaps_json TEXT NOT NULL,
                    validation_json TEXT NOT NULL,
                    selection_json TEXT NOT NULL,
                    archive_id TEXT NOT NULL,
                    created_at TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_recovery_instance
                    ON recovery_points(instance, timeline, target_time);

                CREATE TABLE IF NOT EXISTS validations (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    scan_id INTEGER,
                    recovery_seq INTEGER,
                    instance TEXT NOT NULL,
                    timeline INTEGER NOT NULL,
                    segno INTEGER NOT NULL,
                    digest TEXT NOT NULL,
                    classification TEXT NOT NULL,
                    pages_total INTEGER NOT NULL,
                    pages_checked INTEGER NOT NULL,
                    pages_ok INTEGER NOT NULL,
                    pages_bad INTEGER NOT NULL,
                    checksum_pass_rate REAL NOT NULL,
                    source TEXT NOT NULL,
                    details_json TEXT NOT NULL,
                    created_at TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_validations_instance
                    ON validations(instance, timeline, id);
                CREATE INDEX IF NOT EXISTS idx_validations_scan
                    ON validations(scan_id, instance, timeline);
                """
            )
            self._conn.execute(
                "INSERT INTO meta(key, value) VALUES('schema_version', ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (str(SCHEMA_VERSION),),
            )

    def _begin(self) -> None:
        try:
            self._conn.execute("BEGIN IMMEDIATE")
        except sqlite3.OperationalError as exc:
            if "locked" in str(exc).lower() or "busy" in str(exc).lower():
                code, name = E_DATABASE_BUSY
                raise err(code, name, "SQLite catalog is busy", http_status=503) from exc
            code, name = E_DATABASE_ERROR
            raise err(code, name, f"unable to begin transaction: {exc}") from exc

    def _rollback_quietly(self) -> None:
        try:
            self._conn.execute("ROLLBACK")
        except sqlite3.Error:
            pass

    def _meta(self, key: str) -> str | None:
        row = self._conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row["value"] if row else None

    def _set_meta(self, key: str, value: str) -> None:
        self._conn.execute(
            "INSERT INTO meta(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )

    def latest_scan_id(self) -> int | None:
        with self._lock:
            value = self._meta("latest_scan_id")
            if value is None:
                return None
            try:
                return int(value)
            except ValueError:
                return None

    def latest_scan(self) -> ScanRecord | None:
        with self._lock:
            scan_id = self.latest_scan_id()
            if scan_id is None:
                return None
            row = self._conn.execute("SELECT * FROM scans WHERE id=?", (scan_id,)).fetchone()
            if row is None:
                return None
            return ScanRecord(
                id=row["id"],
                started_at=row["started_at"],
                finished_at=row["finished_at"],
                duration_ms=row["duration_ms"],
                archive_id=row["archive_id"],
                root_label=row["root_label"],
                segment_files=row["segment_files"],
                config=_loads(row["config_json"], {}),
            )

    def get_quarantines(self) -> list[Quarantine]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT instance, filename, rel_path, digest, classification "
                "FROM quarantine ORDER BY instance, filename"
            ).fetchall()
            return [
                Quarantine(
                    instance=row["instance"],
                    filename=row["filename"],
                    rel_path=row["rel_path"],
                    digest=row["digest"],
                    classification=row["classification"],
                )
                for row in rows
            ]

    def commit_scan(
        self,
        snapshot: ScanSnapshot,
        *,
        started_at: str,
        duration_ms: int,
    ) -> int:
        with self._lock:
            self._begin()
            try:
                cursor = self._conn.execute(
                    "INSERT INTO scans("
                    "started_at, finished_at, duration_ms, archive_id, root_label, "
                    "segment_files, config_json"
                    ") VALUES(?, ?, ?, ?, ?, ?, ?)",
                    (
                        started_at,
                        _now(),
                        duration_ms,
                        snapshot.archive_id,
                        snapshot.root_label,
                        snapshot.files_seen,
                        _json(snapshot.config.as_dict()),
                    ),
                )
                scan_id = int(cursor.lastrowid)

                for item in snapshot.errors:
                    self._conn.execute(
                        "INSERT INTO scan_errors("
                        "scan_id, code, name, message, rel_path, details_json"
                        ") VALUES(?, ?, ?, ?, ?, ?)",
                        (
                            scan_id,
                            item.code,
                            item.name,
                            item.message,
                            item.rel_path,
                            _json(item.details),
                        ),
                    )

                for marker in snapshot.markers:
                    self._conn.execute(
                        "INSERT INTO timeline_markers("
                        "timeline, parent_timeline, switch_segment, switch_lsn, "
                        "switched_at, authoritative_instance, source_instance, reason, "
                        "rel_path, digest, raw_json, first_scan_id, last_scan_id"
                        ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
                        "ON CONFLICT(rel_path, digest) DO UPDATE SET "
                        "timeline=excluded.timeline, "
                        "parent_timeline=excluded.parent_timeline, "
                        "switch_segment=excluded.switch_segment, "
                        "switch_lsn=excluded.switch_lsn, "
                        "switched_at=excluded.switched_at, "
                        "authoritative_instance=excluded.authoritative_instance, "
                        "source_instance=excluded.source_instance, "
                        "reason=excluded.reason, "
                        "raw_json=excluded.raw_json, "
                        "last_scan_id=excluded.last_scan_id",
                        (
                            marker.timeline,
                            marker.parent_timeline,
                            marker.switch_segment,
                            marker.switch_lsn,
                            marker.switched_at,
                            marker.authoritative_instance,
                            marker.source_instance,
                            marker.reason,
                            marker.rel_path,
                            marker.digest,
                            _json(marker.raw),
                            scan_id,
                            scan_id,
                        ),
                    )

                for version in snapshot.versions:
                    self._upsert_version(scan_id, version)
                self._recompute_conflicts(scan_id)

                for item in snapshot.in_progress:
                    self._conn.execute(
                        "INSERT INTO in_progress("
                        "scan_id, instance, filename, rel_path, reason, size, mtime_ns, "
                        "timeline, segno, details_json"
                        ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            scan_id,
                            item.instance,
                            item.filename,
                            item.rel_path,
                            item.reason,
                            item.size,
                            item.mtime_ns,
                            item.timeline,
                            item.segno,
                            _json(item.details),
                        ),
                    )

                for item in snapshot.quarantines:
                    self._conn.execute(
                        "INSERT INTO quarantine("
                        "instance, filename, rel_path, digest, classification, "
                        "first_scan_id, created_at"
                        ") VALUES(?, ?, ?, ?, ?, ?, ?) "
                        "ON CONFLICT(instance, filename) DO NOTHING",
                        (
                            item.instance,
                            item.filename,
                            item.rel_path,
                            item.digest,
                            item.classification,
                            scan_id,
                            _now(),
                        ),
                    )

                self._set_meta("latest_scan_id", str(scan_id))
                self._conn.execute("COMMIT")
                return scan_id
            except Exception:
                self._rollback_quietly()
                raise

    def _recompute_conflicts(self, scan_id: int) -> None:
        """Apply first-completed-valid semantics across history and instances.

        A version is a conflict when it differs from the first complete valid
        version for the same physical instance/filename, or from the first
        complete valid version in the same timeline/segment group.  The first
        rule keeps sequential same-path overwrites auditable; the second
        records divergent copies from other instances.
        """

        rows = self._conn.execute(
            "SELECT * FROM wal_versions"
        ).fetchall()
        path_first: dict[tuple[str, int, int, str], sqlite3.Row] = {}
        group_first: dict[tuple[int, int], sqlite3.Row] = {}
        for row in rows:
            if row["classification"] != "complete":
                continue
            path_key = (
                row["instance"],
                row["timeline"],
                row["segno"],
                row["filename"],
            )
            current_path = path_first.get(path_key)
            if current_path is None or (
                row["archived_at"],
                row["digest"],
            ) < (
                current_path["archived_at"],
                current_path["digest"],
            ):
                path_first[path_key] = row
            group_key = (row["timeline"], row["segno"])
            current_group = group_first.get(group_key)
            if current_group is None or (
                row["archived_at"],
                row["instance"],
                row["digest"],
            ) < (
                current_group["archived_at"],
                current_group["instance"],
                current_group["digest"],
            ):
                group_first[group_key] = row

        current_rows = self._conn.execute(
            "SELECT * FROM wal_versions WHERE last_scan_id=?", (scan_id,)
        ).fetchall()
        for row in current_rows:
            conflict = 0
            if row["classification"] == "complete":
                path_key = (
                    row["instance"],
                    row["timeline"],
                    row["segno"],
                    row["filename"],
                )
                path_winner = path_first.get(path_key)
                group_winner = group_first.get((row["timeline"], row["segno"]))
                if path_winner is not None and path_winner["digest"] != row["digest"]:
                    conflict = 1
                if group_winner is not None and group_winner["digest"] != row["digest"]:
                    conflict = 1
            self._conn.execute(
                "UPDATE wal_versions SET conflict=? WHERE id=?",
                (conflict, row["id"]),
            )

    def _upsert_version(self, scan_id: int, version: ScanVersion) -> None:
        existing = self._conn.execute(
            "SELECT * FROM wal_versions "
            "WHERE instance=? AND timeline=? AND segno=? AND digest=?",
            (version.instance, version.timeline, version.segno, version.digest),
        ).fetchone()
        classification = version.classification
        usable = int(version.usable)
        reason = version.reason
        if existing is not None and existing["classification"] in (
            "truncated",
            "checksum_failed",
        ):
            if classification == "complete":
                classification = existing["classification"]
                usable = 0
                reason = (
                    "sticky classification: a content version already classified "
                    f"as {existing['classification']} cannot become complete"
                )
        if existing is None:
            cursor = self._conn.execute(
                "INSERT INTO wal_versions("
                "instance, timeline, segno, filename, rel_path, digest, size, mtime_ns, "
                "archived_at, classification, usable, conflict, header_ok, length_ok, "
                "pages_total, pages_parsed, pages_checked, pages_ok, pages_bad, "
                "pages_unchecked, checksum_pass_rate, reason, details_json, "
                "first_scan_id, last_scan_id"
                ") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    version.instance,
                    version.timeline,
                    version.segno,
                    version.filename,
                    version.rel_path,
                    version.digest,
                    version.size,
                    version.mtime_ns,
                    version.archived_at,
                    classification,
                    usable,
                    int(version.conflict),
                    int(version.header_ok),
                    int(version.length_ok),
                    version.pages_total,
                    version.pages_parsed,
                    version.pages_checked,
                    version.pages_ok,
                    version.pages_bad,
                    version.pages_unchecked,
                    version.checksum_pass_rate,
                    reason,
                    _json(version.details),
                    scan_id,
                    scan_id,
                ),
            )
            version_id = int(cursor.lastrowid)
        else:
            version_id = int(existing["id"])
            self._conn.execute(
                "UPDATE wal_versions SET "
                "filename=?, rel_path=?, size=?, mtime_ns=?, archived_at=?, "
                "classification=?, usable=?, conflict=?, header_ok=?, length_ok=?, "
                "pages_total=?, pages_parsed=?, pages_checked=?, pages_ok=?, "
                "pages_bad=?, pages_unchecked=?, checksum_pass_rate=?, reason=?, "
                "details_json=?, last_scan_id=? "
                "WHERE id=?",
                (
                    version.filename,
                    version.rel_path,
                    version.size,
                    version.mtime_ns,
                    version.archived_at,
                    classification,
                    usable,
                    int(version.conflict),
                    int(version.header_ok),
                    int(version.length_ok),
                    version.pages_total,
                    version.pages_parsed,
                    version.pages_checked,
                    version.pages_ok,
                    version.pages_bad,
                    version.pages_unchecked,
                    version.checksum_pass_rate,
                    reason,
                    _json(version.details),
                    scan_id,
                    version_id,
                ),
            )

        self._conn.execute(
            "INSERT INTO validations("
            "scan_id, recovery_seq, instance, timeline, segno, digest, classification, "
            "pages_total, pages_checked, pages_ok, pages_bad, checksum_pass_rate, "
            "source, details_json, created_at"
            ") VALUES(?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'scan', ?, ?)",
            (
                scan_id,
                version.instance,
                version.timeline,
                version.segno,
                version.digest,
                classification,
                version.pages_total,
                version.pages_checked,
                version.pages_ok,
                version.pages_bad,
                version.checksum_pass_rate,
                _json(
                    {
                        "reason": reason,
                        "version_id": version_id,
                        "violations": version.details.get("violations", []),
                    }
                ),
                _now(),
            ),
        )

    def current_versions(
        self,
        *,
        scan_id: int | None = None,
        instance: str | None = None,
        timeline: int | None = None,
    ) -> list[sqlite3.Row]:
        with self._lock:
            active = scan_id if scan_id is not None else self.latest_scan_id()
            if active is None:
                return []
            clauses = ["last_scan_id=?"]
            params: list[Any] = [active]
            if instance is not None:
                clauses.append("instance=?")
                params.append(instance)
            if timeline is not None:
                clauses.append("timeline=?")
                params.append(timeline)
            sql = (
                "SELECT * FROM wal_versions WHERE "
                + " AND ".join(clauses)
                + " ORDER BY instance, timeline, segno, digest"
            )
            return self._conn.execute(sql, params).fetchall()

    def all_versions(
        self,
        *,
        timeline: int | None = None,
        segno: int | None = None,
        instance: str | None = None,
    ) -> list[sqlite3.Row]:
        """Return current and historical content versions for audit queries."""

        with self._lock:
            clauses: list[str] = []
            params: list[Any] = []
            if timeline is not None:
                clauses.append("timeline=?")
                params.append(timeline)
            if segno is not None:
                clauses.append("segno=?")
                params.append(segno)
            if instance is not None:
                clauses.append("instance=?")
                params.append(instance)
            sql = "SELECT * FROM wal_versions"
            if clauses:
                sql += " WHERE " + " AND ".join(clauses)
            sql += " ORDER BY instance, timeline, segno, archived_at, digest"
            return self._conn.execute(sql, params).fetchall()

    def current_markers(self, *, scan_id: int | None = None) -> list[TimelineMarker]:
        with self._lock:
            active = scan_id if scan_id is not None else self.latest_scan_id()
            if active is None:
                return []
            rows = self._conn.execute(
                "SELECT * FROM timeline_markers WHERE last_scan_id=? "
                "ORDER BY timeline, digest",
                (active,),
            ).fetchall()
            return [
                TimelineMarker(
                    timeline=row["timeline"],
                    parent_timeline=row["parent_timeline"],
                    switch_segment=row["switch_segment"],
                    switch_lsn=row["switch_lsn"],
                    switched_at=row["switched_at"],
                    authoritative_instance=row["authoritative_instance"],
                    source_instance=row["source_instance"],
                    reason=row["reason"],
                    rel_path=row["rel_path"],
                    digest=row["digest"],
                    raw=_loads(row["raw_json"], {}),
                )
                for row in rows
            ]

    def current_in_progress(self, *, scan_id: int | None = None) -> list[InProgressSegment]:
        with self._lock:
            active = scan_id if scan_id is not None else self.latest_scan_id()
            if active is None:
                return []
            rows = self._conn.execute(
                "SELECT * FROM in_progress WHERE scan_id=? ORDER BY instance, rel_path",
                (active,),
            ).fetchall()
            return [
                InProgressSegment(
                    instance=row["instance"],
                    filename=row["filename"],
                    rel_path=row["rel_path"],
                    reason=row["reason"],
                    size=row["size"],
                    mtime_ns=row["mtime_ns"],
                    timeline=row["timeline"],
                    segno=row["segno"],
                    details=_loads(row["details_json"], {}),
                )
                for row in rows
            ]

    def current_errors(self, *, scan_id: int | None = None) -> list[ScanError]:
        with self._lock:
            active = scan_id if scan_id is not None else self.latest_scan_id()
            if active is None:
                return []
            rows = self._conn.execute(
                "SELECT * FROM scan_errors WHERE scan_id=? "
                "ORDER BY code, rel_path, message",
                (active,),
            ).fetchall()
            return [
                ScanError(
                    code=row["code"],
                    name=row["name"],
                    message=row["message"],
                    rel_path=row["rel_path"],
                    details=_loads(row["details_json"], {}),
                )
                for row in rows
            ]

    def current_markers_effective(self) -> dict[int, TimelineMarker]:
        markers = self.current_markers()
        effective: dict[int, TimelineMarker] = {}
        for marker in markers:
            existing = effective.get(marker.timeline)
            if existing is None or (
                marker.switched_at,
                marker.digest,
                marker.source_instance or "",
            ) > (
                existing.switched_at,
                existing.digest,
                existing.source_instance or "",
            ):
                effective[marker.timeline] = marker
        return effective

    def recovery_by_request(self, request_key: str) -> sqlite3.Row | None:
        with self._lock:
            return self._conn.execute(
                "SELECT * FROM recovery_points WHERE request_key=?",
                (request_key,),
            ).fetchone()

    def recovery_by_id(self, recovery_id: str) -> sqlite3.Row | None:
        with self._lock:
            return self._conn.execute(
                "SELECT * FROM recovery_points WHERE id=?",
                (recovery_id,),
            ).fetchone()

    def list_recoveries(
        self, *, instance: str | None = None, timeline: int | None = None
    ) -> list[sqlite3.Row]:
        with self._lock:
            clauses: list[str] = []
            params: list[Any] = []
            if instance is not None:
                clauses.append("instance=?")
                params.append(instance)
            if timeline is not None:
                clauses.append("timeline=?")
                params.append(timeline)
            sql = "SELECT * FROM recovery_points"
            if clauses:
                sql += " WHERE " + " AND ".join(clauses)
            sql += " ORDER BY instance, timeline, target_time, id"
            return self._conn.execute(sql, params).fetchall()

    def latest_recovery(
        self, *, instance: str, timeline: int | None = None
    ) -> sqlite3.Row | None:
        with self._lock:
            if timeline is None:
                return self._conn.execute(
                    "SELECT * FROM recovery_points WHERE instance=? "
                    "ORDER BY seq DESC LIMIT 1",
                    (instance,),
                ).fetchone()
            return self._conn.execute(
                "SELECT * FROM recovery_points WHERE instance=? AND timeline=? "
                "ORDER BY seq DESC LIMIT 1",
                (instance, timeline),
            ).fetchone()

    def latest_validation(
        self, *, instance: str, timeline: int | None = None
    ) -> sqlite3.Row | None:
        with self._lock:
            if timeline is None:
                return self._conn.execute(
                    "SELECT * FROM validations WHERE instance=? "
                    "ORDER BY id DESC LIMIT 1",
                    (instance,),
                ).fetchone()
            return self._conn.execute(
                "SELECT * FROM validations WHERE instance=? AND timeline=? "
                "ORDER BY id DESC LIMIT 1",
                (instance, timeline),
            ).fetchone()

    def recovery_counts(self) -> dict[tuple[str, int], int]:
        with self._lock:
            rows = self._conn.execute(
                "SELECT instance, timeline, COUNT(*) AS count "
                "FROM recovery_points GROUP BY instance, timeline"
            ).fetchall()
            return {(row["instance"], row["timeline"]): int(row["count"]) for row in rows}

    def commit_recovery(
        self,
        *,
        recovery_id: str,
        request_key: str,
        instance: str,
        timeline: int,
        target_time: str,
        source_instance: str | None,
        head_segno: int,
        chain: Sequence[dict[str, Any]],
        gaps: Sequence[int],
        validation: dict[str, Any],
        selection: Sequence[dict[str, Any]],
        archive_id: str,
        validation_rows: Sequence[dict[str, Any]],
        created_at: str | None = None,
    ) -> tuple[sqlite3.Row, bool]:
        """Commit a complete recovery point and all validation rows atomically.

        Existing request keys are never overwritten.  There is no "draft"
        recovery-point row: callers either see a committed row or nothing.
        """

        with self._lock:
            self._begin()
            try:
                existing = self._conn.execute(
                    "SELECT * FROM recovery_points WHERE request_key=?",
                    (request_key,),
                ).fetchone()
                if existing is not None:
                    self._conn.execute("COMMIT")
                    return existing, False
                cursor = self._conn.execute(
                    "INSERT INTO recovery_points("
                    "id, request_key, status, instance, timeline, target_time, "
                    "source_instance, head_segno, chain_length, chain_json, gaps_json, "
                    "validation_json, selection_json, archive_id, created_at"
                    ") VALUES(?, ?, 'committed', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        recovery_id,
                        request_key,
                        instance,
                        timeline,
                        target_time,
                        source_instance,
                        head_segno,
                        len(chain),
                        _json(list(chain)),
                        _json(list(gaps)),
                        _json(validation),
                        _json(list(selection)),
                        archive_id,
                        created_at or _now(),
                    ),
                )
                recovery_seq = int(cursor.lastrowid)
                for row in validation_rows:
                    self._conn.execute(
                        "INSERT INTO validations("
                        "scan_id, recovery_seq, instance, timeline, segno, digest, "
                        "classification, pages_total, pages_checked, pages_ok, pages_bad, "
                        "checksum_pass_rate, source, details_json, created_at"
                        ") VALUES(NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'recovery', ?, ?)",
                        (
                            recovery_seq,
                            row["instance"],
                            row["timeline"],
                            row["segno"],
                            row["digest"],
                            row["classification"],
                            row["pages_total"],
                            row["pages_checked"],
                            row["pages_ok"],
                            row["pages_bad"],
                            row["checksum_pass_rate"],
                            _json(row.get("details", {})),
                            created_at or _now(),
                        ),
                    )
                self._conn.execute("COMMIT")
                saved = self._conn.execute(
                    "SELECT * FROM recovery_points WHERE seq=?",
                    (recovery_seq,),
                ).fetchone()
                if saved is None:
                    raise RuntimeError("committed recovery point disappeared")
                return saved, True
            except Exception:
                self._rollback_quietly()
                raise

    def clear_quarantine(self, instance: str, filename: str) -> bool:
        with self._lock:
            self._begin()
            try:
                cursor = self._conn.execute(
                    "DELETE FROM quarantine WHERE instance=? AND filename=?",
                    (instance, filename),
                )
                self._conn.execute("COMMIT")
                return cursor.rowcount > 0
            except Exception:
                self._rollback_quietly()
                raise

    def record_validation(
        self,
        *,
        instance: str,
        timeline: int,
        segno: int,
        digest: str,
        classification: str,
        pages_total: int,
        pages_checked: int,
        pages_ok: int,
        pages_bad: int,
        checksum_pass_rate: float,
        source: str,
        details: dict[str, Any] | None = None,
    ) -> int:
        with self._lock:
            self._begin()
            try:
                cursor = self._conn.execute(
                    "INSERT INTO validations("
                    "scan_id, recovery_seq, instance, timeline, segno, digest, "
                    "classification, pages_total, pages_checked, pages_ok, pages_bad, "
                    "checksum_pass_rate, source, details_json, created_at"
                    ") VALUES(NULL, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        instance,
                        timeline,
                        segno,
                        digest,
                        classification,
                        pages_total,
                        pages_checked,
                        pages_ok,
                        pages_bad,
                        checksum_pass_rate,
                        source,
                        _json(details or {}),
                        _now(),
                    ),
                )
                self._conn.execute("COMMIT")
                return int(cursor.lastrowid)
            except Exception:
                self._rollback_quietly()
                raise
