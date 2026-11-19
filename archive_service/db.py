from __future__ import annotations

import sqlite3
from contextlib import contextmanager
from pathlib import Path
from typing import Iterator

from .util import format_time, utc_now


SCHEMA = """
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS policy_publications (
    publication_id INTEGER PRIMARY KEY AUTOINCREMENT,
    effective_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    applied_at TEXT
);

CREATE TABLE IF NOT EXISTS policy_values (
    publication_id INTEGER NOT NULL,
    category TEXT NOT NULL,
    retention_days INTEGER NOT NULL CHECK (retention_days >= 0),
    PRIMARY KEY (publication_id, category),
    FOREIGN KEY (publication_id) REFERENCES policy_publications(publication_id)
);

CREATE TABLE IF NOT EXISTS object_versions (
    version_id TEXT PRIMARY KEY,
    object_key TEXT NOT NULL,
    version_number INTEGER NOT NULL,
    written_at TEXT NOT NULL,
    category TEXT NOT NULL,
    legal_hold INTEGER NOT NULL DEFAULT 0 CHECK (legal_hold IN (0, 1)),
    retention_until TEXT NOT NULL,
    content_length INTEGER NOT NULL,
    content_sha256 TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    deleted_at TEXT,
    UNIQUE (object_key, version_number)
);

CREATE INDEX IF NOT EXISTS idx_versions_key_number
    ON object_versions(object_key, version_number DESC);
CREATE INDEX IF NOT EXISTS idx_versions_retention
    ON object_versions(status, retention_until);

CREATE TABLE IF NOT EXISTS domain_copies (
    version_id TEXT NOT NULL,
    domain TEXT NOT NULL,
    write_status TEXT NOT NULL,
    expected_length INTEGER NOT NULL,
    expected_sha256 TEXT NOT NULL,
    delete_status TEXT NOT NULL,
    last_error TEXT,
    PRIMARY KEY (version_id, domain),
    FOREIGN KEY (version_id) REFERENCES object_versions(version_id)
);

CREATE INDEX IF NOT EXISTS idx_domain_copies_domain
    ON domain_copies(domain, write_status, delete_status);

CREATE TABLE IF NOT EXISTS lock_records (
    version_id TEXT PRIMARY KEY,
    retention_until TEXT NOT NULL,
    lock_pinned INTEGER NOT NULL DEFAULT 0 CHECK (lock_pinned IN (0, 1)),
    updated_at TEXT NOT NULL,
    FOREIGN KEY (version_id) REFERENCES object_versions(version_id)
);

CREATE TABLE IF NOT EXISTS policy_extension_events (
    publication_id INTEGER NOT NULL,
    version_id TEXT NOT NULL,
    old_metadata_until TEXT NOT NULL,
    new_metadata_until TEXT NOT NULL,
    old_lock_until TEXT,
    new_lock_until TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (publication_id, version_id),
    FOREIGN KEY (publication_id) REFERENCES policy_publications(publication_id),
    FOREIGN KEY (version_id) REFERENCES object_versions(version_id)
);

CREATE TABLE IF NOT EXISTS counters (
    name TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS service_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_migrations(version, applied_at)
VALUES (1, '1970-01-01T00:00:00.000000Z');
"""


class Database:
    def __init__(self, path: str | Path):
        self.path = Path(path)

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(
            self.path,
            timeout=30.0,
            isolation_level=None,
            check_same_thread=False,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 30000")
        connection.execute("PRAGMA journal_mode = WAL")
        connection.execute("PRAGMA synchronous = NORMAL")
        return connection

    def initialize(
        self,
        *,
        regular_days: int,
        audit_days: int,
        legal_days: int,
        effective_at: str | None = None,
    ) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.connect() as connection:
            connection.executescript(SCHEMA)
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT publication_id FROM policy_publications "
                "ORDER BY effective_at ASC, publication_id ASC LIMIT 1"
            ).fetchone()
            if row is None:
                now = format_time(utc_now())
                effective = effective_at or "1970-01-01T00:00:00.000000Z"
                cursor = connection.execute(
                    "INSERT INTO policy_publications(effective_at, created_at, applied_at) "
                    "VALUES (?, ?, ?)",
                    (effective, now, now),
                )
                publication_id = int(cursor.lastrowid)
                connection.executemany(
                    "INSERT INTO policy_values(publication_id, category, retention_days) "
                    "VALUES (?, ?, ?)",
                    [
                        (publication_id, "regular", int(regular_days)),
                        (publication_id, "audit", int(audit_days)),
                        (publication_id, "legal", int(legal_days)),
                    ],
                )
            connection.execute(
                "INSERT OR IGNORE INTO counters(name, value) VALUES (?, 0)",
                ("retention_conflict_delete_denials",),
            )

    @contextmanager
    def transaction(self, *, immediate: bool = False) -> Iterator[sqlite3.Connection]:
        connection = self.connect()
        try:
            connection.execute("BEGIN IMMEDIATE" if immediate else "BEGIN")
            yield connection
            connection.execute("COMMIT")
        except BaseException:
            try:
                connection.execute("ROLLBACK")
            except sqlite3.Error:
                pass
            raise
        finally:
            connection.close()
