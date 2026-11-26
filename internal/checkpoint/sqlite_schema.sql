-- Logical SQLite schema for the acoustic checkpoint boundary.
--
-- This repository intentionally uses only the Go standard library and therefore
-- ships CheckpointStore: an equivalent append-only, checksummed transaction log
-- plus atomic temp-file/rename snapshots. The DDL below records the supported
-- SQLite deployment shape when a stdlib-compatible database/sql driver is made
-- available by the runtime environment. One chunk commit maps to one row and
-- should be inserted inside BEGIN/COMMIT.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS rebuild_rounds (
    round_id          TEXT PRIMARY KEY,
    mode              TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('pending','rebuilding','committed','failed')),
    param_version     TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    created_seed      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS chunk_commits (
    txn_seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    round_id          TEXT NOT NULL REFERENCES rebuild_rounds(round_id),
    order_index       INTEGER NOT NULL,
    chunk_id          TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('pending','path_screening','energy_transfer','committed','failed')),
    path_digest       TEXT NOT NULL,
    listener_energy_q TEXT NOT NULL, -- JSON array per continuous band id
    absorbed_energy_q TEXT NOT NULL,
    inbound_energy_q  TEXT NOT NULL,
    retained_energy_q TEXT NOT NULL,
    transfer_records  TEXT NOT NULL, -- JSON array with source chunk, boundary position and components
    dependencies      TEXT NOT NULL, -- JSON dependency key set
    content_hash      TEXT NOT NULL,
    local_cache_key   TEXT NOT NULL,
    cache_key         TEXT NOT NULL,
    param_version     TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    result_json       TEXT NOT NULL,
    checksum          TEXT NOT NULL UNIQUE,
    committed_at      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(round_id, chunk_id, txn_seq)
);

CREATE INDEX IF NOT EXISTS idx_chunk_commits_latest
    ON chunk_commits(chunk_id, algorithm_version, param_version, txn_seq DESC);

CREATE TABLE IF NOT EXISTS transfer_versions (
    transfer_id      TEXT PRIMARY KEY,
    from_chunk_id    TEXT NOT NULL,
    to_chunk_id      TEXT NOT NULL,
    boundary_side    TEXT NOT NULL,
    position_x_q     INTEGER NOT NULL,
    position_y_q     INTEGER NOT NULL,
    transfer_version TEXT NOT NULL,
    energy_json      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invalidations (
    round_id       TEXT NOT NULL REFERENCES rebuild_rounds(round_id),
    chunk_id       TEXT NOT NULL,
    reason         TEXT NOT NULL,
    dependency_key TEXT NOT NULL,
    PRIMARY KEY(round_id, chunk_id, dependency_key)
);

-- Example single-transaction commit:
--
-- BEGIN IMMEDIATE;
-- INSERT INTO chunk_commits (...) VALUES (...);
-- INSERT INTO transfer_versions (...) VALUES (...)
--   ON CONFLICT(transfer_id) DO UPDATE SET ...;
-- COMMIT;
--
-- Recovery selects only rows whose algorithm_version and param_version match
-- the snapshot, then takes the greatest txn_seq for each chunk in the round.
