"""Pure batch path: one DuckDB computation with the SAME semantics.

Dedup key, window assignment, watermark advancement and late-event handling
mirror ``engine.Engine`` exactly; only the execution engine differs. Used to
prove that one complete dataset yields row-identical results whether it is
submitted through the pure batch or the batch/stream mixed path.
"""

from __future__ import annotations

import os

import duckdb

from .config import JobConfig
from .semantics import SplitPlan, window_start
from .util import rss_bytes


def _all_rows(data_dir: str, manifest: dict) -> list[dict]:
    """Materialize every source row with a global deterministic order."""

    import pyarrow.parquet as pq

    from .io_sources import read_jsonl_batch

    rows: list[dict] = []
    for shard_index, shard in enumerate(manifest["history"]):
        table = pq.read_table(os.path.join(data_dir, shard["path"]))
        for row in table.to_pylist():
            row["_source_id"] = -1 - shard_index
            rows.append(row)
    for batch in manifest["stream_batches"]:
        for row in read_jsonl_batch(data_dir, batch):
            row["_source_id"] = batch["batch_id"]
            rows.append(row)
    return rows


def run_batch(cfg: JobConfig, manifest: dict, split_plan: SplitPlan) -> dict:
    rows = _all_rows(cfg.data_dir, manifest)
    size = cfg.window_size_ms

    # Source units in consumption order and their *own* declared event times,
    # exactly as the streaming engine observes them unit by unit.
    unit_order: list[int] = []
    declared: dict[int, int] = {}
    for shard in manifest["history"]:
        unit = -1 - shard["hour"]
        unit_order.append(unit)
        declared[unit] = shard_max = 0
        for r in rows:
            if r["_source_id"] == unit and r["event_time_ms"] > shard_max:
                shard_max = r["event_time_ms"]
        declared[unit] = shard_max
    for batch in manifest["stream_batches"]:
        unit = batch["batch_id"]
        unit_order.append(unit)
        declared[unit] = batch["declared_event_time_ms"]

    # Watermark visible *while* each unit is processed. Like the streaming
    # engine, events in unit N are judged against the watermark produced by
    # unit N-1; the watermark only advances after the unit is consumed.
    watermark_during: dict[int, int] = {}
    max_declared = 0
    watermark = 0
    for unit in unit_order:
        watermark_during[unit] = watermark
        max_declared = max(max_declared, declared[unit])
        watermark = max(
            watermark, max_declared - cfg.allowed_lateness_ms
        )

    seen: set[str] = set()
    windows: dict[int, list[int]] = {}
    late: dict[int, int] = {}
    dedup_dropped = 0

    # Mirror the streaming engine exactly: source units are consumed in order,
    # the watermark is advanced once per unit, and every row in a unit is
    # judged against the watermark observed when that unit is committed.
    rows_by_unit: dict[int, list[dict]] = {}
    for row in rows:
        rows_by_unit.setdefault(row["_source_id"], []).append(row)
    for unit in unit_order:
        watermark = watermark_during[unit]
        for row in rows_by_unit.get(unit, []):
            w_start = window_start(row["event_time_ms"], size)
            key = f"{row['payment_id']}|{w_start}|{row['event_id']}"
            if key in seen:
                dedup_dropped += 1
                continue
            seen.add(key)
            if watermark >= w_start + size:
                late[w_start] = late.get(w_start, 0) + 1
                continue
            agg = windows.setdefault(w_start, [0, 0])
            agg[0] += 1
            agg[1] += int(row["amount_cents"])

    output_rows = []
    for w in sorted(windows):
        count, amount = windows[w]
        output_rows.append(
            {
                "window_start": w,
                "event_count": count,
                "amount_cents": amount,
                "late_count": 0,
                "completed": True,
            }
        )
    # Late events never touch emitted window rows; windows that received only
    # late events appear as separate zero-amount rows.
    for w in sorted(late):
        output_rows.append(
            {
                "window_start": w,
                "event_count": 0,
                "amount_cents": 0,
                "late_count": late[w],
                "completed": True,
            }
        )
    output_rows.sort(key=lambda r: r["window_start"])

    # DuckDB cross-check: the same window / dedup definition expressed in SQL,
    # run over an in-memory Arrow table (no external database needed).
    con = duckdb.connect()
    import pyarrow as pa

    table = pa.table(
        {
            "event_id": pa.array([r["event_id"] for r in rows], pa.string()),
            "payment_id": pa.array([r["payment_id"] for r in rows], pa.string()),
            "event_time_ms": pa.array(
                [r["event_time_ms"] for r in rows], pa.int64()
            ),
            "amount_cents": pa.array(
                [r["amount_cents"] for r in rows], pa.int64()
            ),
            "event_type": pa.array([r["event_type"] for r in rows], pa.string()),
            "_source_id": pa.array([r["_source_id"] for r in rows], pa.int64()),
            "arrival_ms": pa.array(
                [r.get("arrival_ms", 0) for r in rows], pa.int64()
            ),
        }
    )
    con.register("events", table)
    wm_table = pa.table(
        {
            "_source_id": pa.array(unit_order, pa.int64()),
            "watermark": pa.array(
                [watermark_during[u] for u in unit_order], pa.int64()
            ),
        }
    )
    con.register("watermarks", wm_table)
    sql_rows = con.execute(
        """
        WITH deduped AS (
            SELECT *,
                   (event_time_ms // ?) * ? AS window_start,
                   row_number() OVER (
                       PARTITION BY payment_id,
                                    (event_time_ms // ?) * ?,
                                    event_id
                       ORDER BY _source_id, arrival_ms, event_id
                   ) AS rn
            FROM events
        )
        SELECT d.window_start,
               count(*) AS event_count,
               sum(d.amount_cents) AS amount_cents
        FROM deduped d
        JOIN watermarks w ON w._source_id = d._source_id
        WHERE d.rn = 1
          AND w.watermark < d.window_start + ?
        GROUP BY window_start
        ORDER BY window_start
        """,
        [size, size, size, size, size],
    ).fetchall()
    con.close()

    sql_map = {int(w): (int(c), int(a)) for w, c, a in sql_rows}
    for row in output_rows:
        if row["event_count"] == 0:
            continue  # late-only rows are not part of the SQL aggregation
        count, amount = sql_map.get(row["window_start"], (0, 0))
        if (count, amount) != (row["event_count"], row["amount_cents"]):
            raise AssertionError(
                "duckdb cross-check diverged from the semantic implementation"
            )
    sql_windows = set(sql_map)
    semantic_windows = {
        r["window_start"] for r in output_rows if r["event_count"] > 0
    }
    if sql_windows != semantic_windows:
        raise AssertionError(
            "duckdb cross-check window set diverged"
        )

    return {
        "mode": "batch",
        "allowed_lateness_ms": cfg.allowed_lateness_ms,
        "window_size_ms": cfg.window_size_ms,
        "partitions": cfg.partitions,
        "workers": cfg.workers,
        "input": {
            "history_shards": len(manifest["history"]),
            "stream_batches": len(manifest["stream_batches"]),
            "history_rows": manifest["history_row_count"],
            "stream_rows": manifest["stream_row_count"],
        },
        "dedup_dropped": dedup_dropped,
        "late_events": sum(late.values()),
        "window_count": len(output_rows),
        "windows": output_rows,
        "split_plan": split_plan.to_dict(),
        "recovery": {
            "recoveries": 0,
            "recovery_commit_points": [],
            "commit_points": [],
            "consumed_batches": sorted(declared),
        },
        "spill_bytes": 0,
        "peak_rss_bytes": rss_bytes(),
        "faults": [],
    }
