"""Batch-side reference computation over the complete data via DuckDB.

Uses the exact same dedup criterion as the streaming path (first
occurrence of an ``event_id`` wins, both join sides alike) and the same
minute-window join/aggregation, so on identical complete input the two
paths must agree field by field.
"""

from __future__ import annotations

import duckdb

from .config import JobConfig

_QUERY = """
WITH all_events AS (
    SELECT event_id, event_type, order_id, event_time, amount_cents
    FROM read_parquet(?, union_by_name = true)
    UNION ALL
    SELECT event_id, event_type, order_id, event_time, amount_cents
    FROM read_json_auto(?, columns = {
        event_id: 'VARCHAR', event_type: 'VARCHAR', order_id: 'VARCHAR',
        event_time: 'BIGINT', amount_cents: 'BIGINT'})
),
dedup AS (
    SELECT *, row_number() OVER (PARTITION BY event_id) AS rn FROM all_events
),
per_key AS (
    SELECT
        CAST(FLOOR(event_time / ?) AS BIGINT) * ? AS window_start,
        order_id,
        SUM(CASE WHEN event_type = 'order' THEN amount_cents ELSE 0 END) AS o_amt,
        SUM(CASE WHEN event_type = 'order' THEN 1 ELSE 0 END) AS o_cnt,
        SUM(CASE WHEN event_type = 'refund' THEN amount_cents ELSE 0 END) AS r_amt,
        SUM(CASE WHEN event_type = 'refund' THEN 1 ELSE 0 END) AS r_cnt
    FROM dedup WHERE rn = 1
    GROUP BY 1, 2
)
SELECT
    window_start,
    SUM(o_amt) AS o_amt, SUM(o_cnt) AS o_cnt,
    SUM(r_amt) AS r_amt, SUM(r_cnt) AS r_cnt,
    SUM(CASE WHEN o_cnt > 0 AND r_cnt > 0 THEN 1 ELSE 0 END) AS matched
FROM per_key
GROUP BY window_start
ORDER BY window_start
"""


def baseline_windows(cfg: JobConfig) -> list[dict]:
    data_dir = cfg.abs_path(cfg.data_dir)
    parquet_glob = str(data_dir / "historical" / "**" / "*.parquet")
    jsonl_glob = str(data_dir / "incremental" / "batch_*.jsonl")
    con = duckdb.connect()
    try:
        rows = con.execute(
            _QUERY,
            [parquet_glob, jsonl_glob, cfg.window_seconds, cfg.window_seconds],
        ).fetchall()
    finally:
        con.close()
    return [
        {
            "window_start": int(r[0]),
            "order_amount_cents": int(r[1]),
            "order_count": int(r[2]),
            "refund_amount_cents": int(r[3]),
            "refund_count": int(r[4]),
            "matched_keys": int(r[5]),
        }
        for r in rows
    ]
