-- inventory_new: target database. deduction_flow has the required shard_no
-- column. Supporting tables make replay and post-cutover validation idempotent.

DROP TABLE IF EXISTS migration_applied_event CASCADE;
DROP TABLE IF EXISTS migration_batch_meta CASCADE;
DROP TABLE IF EXISTS migration_product_baseline CASCADE;
DROP TABLE IF EXISTS deduction_flow CASCADE;
DROP TABLE IF EXISTS inventory CASCADE;
DROP SEQUENCE IF EXISTS flow_id_seq CASCADE;

CREATE SEQUENCE flow_id_seq START WITH 2000000000000;

CREATE TABLE inventory (
    product_id  BIGINT PRIMARY KEY,
    stock_qty   BIGINT NOT NULL CHECK (stock_qty >= 0),
    version     BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE deduction_flow (
    flow_id     BIGINT NOT NULL,
    product_id  BIGINT NOT NULL,
    direction   TEXT NOT NULL CHECK (direction IN ('deduct', 'restock')),
    change_qty  INTEGER NOT NULL CHECK (change_qty <> 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    shard_no    SMALLINT NOT NULL,
    PRIMARY KEY (flow_id, shard_no)
);

CREATE INDEX ix_deduction_flow_product ON deduction_flow(product_id, flow_id);

CREATE TABLE migration_batch_meta (
    batch_id        BIGINT PRIMARY KEY,
    range_start     BIGINT NOT NULL,
    range_end       BIGINT NOT NULL,
    start_snapshot  TEXT NOT NULL,
    start_seq       BIGINT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE migration_product_baseline (
    batch_id       BIGINT NOT NULL,
    product_id     BIGINT NOT NULL,
    old_stock_qty  BIGINT NOT NULL,
    PRIMARY KEY (batch_id, product_id)
);

CREATE TABLE migration_applied_event (
    seq         BIGINT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
