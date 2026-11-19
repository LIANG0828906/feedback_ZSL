-- Deterministic source data: 50,000 inventory rows and 120,000 flow rows.

TRUNCATE migration_outbox RESTART IDENTITY;
TRUNCATE deduction_flow, inventory;

INSERT INTO inventory(product_id, stock_qty, version, updated_at)
SELECT
    product_id,
    1000 + ((product_id * 7919) % 500),
    0,
    TIMESTAMPTZ '2026-01-01 00:00:00+00'
FROM generate_series(1, 50000) AS product_id;

INSERT INTO deduction_flow(flow_id, product_id, direction, change_qty, created_at)
SELECT
    i,
    1 + ((i * 37) % 50000),
    CASE WHEN i % 3 = 0 THEN 'restock' ELSE 'deduct' END,
    CASE WHEN i % 3 = 0 THEN (1 + (i % 5)) ELSE -(1 + (i % 5)) END,
    TIMESTAMPTZ '2026-01-01 00:00:00+00' + (i * INTERVAL '1 second')
FROM generate_series(1, 120000) AS i;

SELECT setval('flow_id_seq', 1000000000000, false);
TRUNCATE migration_outbox RESTART IDENTITY;
