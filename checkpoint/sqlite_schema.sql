-- Optional SQLite mapping for checkpoint.Store.
-- The default offline kernel uses the equivalent atomic JSON file store;
-- this schema shows the same commit ordering for a pure-Go database/sql driver.

CREATE TABLE IF NOT EXISTS commit_pointer (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    format_version INTEGER NOT NULL,
    project_fingerprint TEXT NOT NULL,
    committed_tick INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tenant_state (
    tenant_id TEXT PRIMARY KEY,
    committed_tick INTEGER NOT NULL,
    state_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS node_state (
    node_id TEXT PRIMARY KEY,
    committed_tick INTEGER NOT NULL,
    used_bytes INTEGER NOT NULL,
    pre_deducted_bytes INTEGER NOT NULL,
    state_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS queue_item (
    event_id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    enqueued_tick INTEGER NOT NULL,
    item_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reserve_state (
    decision_id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL,
    tick INTEGER NOT NULL,
    reserve_json TEXT NOT NULL
);

-- Commit order:
--   BEGIN IMMEDIATE;
--   UPSERT tenant_state for every tenant;
--   UPSERT node_state (including occupancy and pre_deducted_bytes);
--   REPLACE queue_item and reserve_state rows;
--   UPSERT commit_pointer.committed_tick;
--   COMMIT;
--
-- A process killed before COMMIT leaves the old commit_pointer and SQLite
-- rollback state. The JSON store implements the same boundary by writing and
-- fsyncing entities/ticks/<tick>.entities.json first and atomically renaming
-- committed.json only afterwards.
