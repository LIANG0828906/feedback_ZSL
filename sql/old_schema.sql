-- inventory_old: legacy source database.
-- The migration service adds trigger-based CDC objects without changing the
-- application tables used by the legacy service.

DROP TABLE IF EXISTS migration_outbox CASCADE;
DROP TABLE IF EXISTS deduction_flow CASCADE;
DROP TABLE IF EXISTS inventory CASCADE;
DROP SEQUENCE IF EXISTS flow_id_seq CASCADE;

CREATE SEQUENCE flow_id_seq START WITH 1000000000000;

CREATE TABLE inventory (
    product_id  BIGINT PRIMARY KEY,
    stock_qty   BIGINT NOT NULL CHECK (stock_qty >= 0),
    version     BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE deduction_flow (
    flow_id     BIGINT PRIMARY KEY,
    product_id  BIGINT NOT NULL REFERENCES inventory(product_id),
    direction   TEXT NOT NULL CHECK (direction IN ('deduct', 'restock')),
    change_qty  INTEGER NOT NULL CHECK (change_qty <> 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX ix_deduction_flow_product ON deduction_flow(product_id, flow_id);

CREATE TABLE migration_outbox (
    seq          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_id   BIGINT NOT NULL,
    flow_id      BIGINT NOT NULL,
    direction    TEXT NOT NULL,
    change_qty   INTEGER NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    captured_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX ix_migration_outbox_product_seq
    ON migration_outbox(product_id, seq);

CREATE OR REPLACE FUNCTION capture_inventory_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO migration_outbox(product_id, flow_id, direction, change_qty, occurred_at)
    VALUES (NEW.product_id, NEW.flow_id, NEW.direction, NEW.change_qty, NEW.created_at);
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_capture_inventory_change
AFTER INSERT ON deduction_flow
FOR EACH ROW
EXECUTE FUNCTION capture_inventory_change();
