from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .accum import add_event, new_acc
from .errors import EngineError
from .expr import ExprEvaluator
from .sql_parser import AggregateSpec, WindowSpec
from .storage import open_state, tx
from .util import canonical_json, iso, iter_jsonl_range, parse_time


def execute_filter_task(task: dict[str, Any]) -> dict[str, Any]:
    """Filter one byte range of a batch.  No database write happens here.

    The coordinator performs deduplication and commits the filter stage after
    all parallel ranges have returned, which makes stage progress atomic.
    """
    output_path = Path(task["output_path"])
    output_path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = output_path.with_suffix(output_path.suffix + f".{os.getpid()}.tmp")
    evaluator = ExprEvaluator(task.get("filter_ast"), task.get("sql", ""))
    read_rows = 0
    kept_rows = 0
    with open(tmp_path, "w", encoding="utf-8", newline="\n") as fh:
        for row in iter_jsonl_range(task["input_path"], int(task["start"]), task.get("end")):
            read_rows += 1
            try:
                verdict = evaluator.eval(row)
            except EngineError:
                raise
            except Exception as exc:
                raise EngineError(
                    "IOV_INPUT_INVALID",
                    f"filter expression failed: {exc}",
                    fragment=canonical_json(row)[:300],
                ) from exc
            if verdict is True or (verdict is not None and not isinstance(verdict, bool) and bool(verdict)):
                fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True, default=str))
                fh.write("\n")
                kept_rows += 1
    os.replace(tmp_path, output_path)
    return {
        "ok": True,
        "task": "filter",
        "read_rows": read_rows,
        "kept_rows": kept_rows,
        "output_path": str(output_path),
        "bytes": output_path.stat().st_size if output_path.exists() else 0,
    }


class WindowCache:
    """A tiny write-back cache whose evictions are the state spill mechanism."""

    def __init__(
        self,
        conn,
        job_id: str,
        shard_id: int,
        stage_id: str,
        threshold_bytes: int,
        batch_id: int,
    ) -> None:
        self.conn = conn
        self.job_id = job_id
        self.shard_id = shard_id
        self.stage_id = stage_id
        self.threshold_bytes = max(0, threshold_bytes)
        self.batch_id = batch_id
        self.cache: dict[tuple[str, int], dict[str, Any]] = {}
        self.bytes = 0
        self.spilled_bytes = 0

    def get(self, vehicle_id: str, window_start: int) -> tuple[dict[str, Any], bool, int]:
        key = (vehicle_id, window_start)
        if key in self.cache:
            entry = self.cache[key]
            return entry["acc"], bool(entry["closed"]), int(entry["window_end"])
        row = self.conn.execute(
            """
            SELECT window_end, acc_json, closed FROM window_accum
            WHERE job_id=? AND stage_id=? AND shard_id=? AND vehicle_id=? AND window_start=?
            """,
            (self.job_id, self.stage_id, self.shard_id, vehicle_id, window_start),
        ).fetchone()
        if row is None:
            return new_acc(), False, 0
        acc = json.loads(row["acc_json"])
        self._put(vehicle_id, window_start, int(row["window_end"]), acc, bool(row["closed"]))
        return acc, bool(row["closed"]), int(row["window_end"])

    def put(
        self,
        vehicle_id: str,
        window_start: int,
        window_end: int,
        acc: dict[str, Any],
        closed: bool = False,
    ) -> None:
        self._put(vehicle_id, window_start, window_end, acc, closed)

    def _put(
        self,
        vehicle_id: str,
        window_start: int,
        window_end: int,
        acc: dict[str, Any],
        closed: bool,
    ) -> None:
        key = (vehicle_id, window_start)
        old = self.cache.get(key)
        if old is not None:
            self.bytes -= int(old["bytes"])
        encoded = canonical_json(acc)
        entry = {
            "vehicle_id": vehicle_id,
            "window_start": window_start,
            "window_end": window_end,
            "acc": acc,
            "closed": closed,
            "encoded": encoded,
            "bytes": len(encoded.encode("utf-8")),
        }
        self.cache[key] = entry
        self.bytes += int(entry["bytes"])
        if self.threshold_bytes and self.bytes > self.threshold_bytes and len(self.cache) > 1:
            self.flush(count_as_spill=True)

    def flush(self, count_as_spill: bool = False) -> None:
        if not self.cache:
            return
        for entry in self.cache.values():
            self.conn.execute(
                """
                INSERT INTO window_accum (
                    job_id, stage_id, shard_id, vehicle_id, window_start,
                    window_end, acc_json, closed, event_rows, bytes, updated_batch
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?)
                ON CONFLICT(job_id, stage_id, shard_id, vehicle_id, window_start) DO UPDATE SET
                    window_end=excluded.window_end,
                    acc_json=excluded.acc_json,
                    closed=excluded.closed,
                    event_rows=excluded.event_rows,
                    bytes=excluded.bytes,
                    updated_batch=excluded.updated_batch
                """,
                (
                    self.job_id,
                    self.stage_id,
                    self.shard_id,
                    entry["vehicle_id"],
                    entry["window_start"],
                    entry["window_end"],
                    entry["encoded"],
                    1 if entry["closed"] else 0,
                    int(entry["acc"].get("n", 0)),
                    int(entry["bytes"]),
                    self.batch_id,
                ),
            )
        if count_as_spill:
            self.spilled_bytes += int(self.bytes)
        self.cache.clear()
        self.bytes = 0


def execute_window_task(task: dict[str, Any]) -> dict[str, Any]:
    """Update one logical window shard and commit its state atomically."""
    job_id = str(task["job_id"])
    batch_id = int(task["batch_id"])
    shard_id = int(task["shard_id"])
    stage_id = "window_aggregate"
    watermark = int(task["watermark"])
    window_size = int(task["window_size"])
    vehicle_column = str(task["vehicle_column"])
    aggregates = [AggregateSpec(**a) for a in task["aggregates"]]
    window_spec = WindowSpec(
        alias=task["window"]["alias"],
        size_seconds=window_size,
        expr_ast=task["window"]["expr_ast"],
        expr_sql=task["window"]["expr_sql"],
    )
    evaluator = ExprEvaluator(window_spec.expr_ast)
    threshold = int(float(task.get("state_memory_mb", 32.0)) * 1024 * 1024)
    conn = open_state(task["state_db"])
    try:
        current = conn.execute(
            """
            SELECT consumed_batch, committed_batch FROM stage_shards
            WHERE job_id=? AND stage_id=? AND shard_id=?
            """,
            (job_id, stage_id, shard_id),
        ).fetchone()
        if current and int(current["committed_batch"]) >= batch_id:
            return {
                "ok": True,
                "task": "window",
                "job_id": job_id,
                "batch_id": batch_id,
                "shard_id": shard_id,
                "noop": True,
                "committed_batch": int(current["committed_batch"]),
            }
        route_path = task.get("route_path")
        cache = WindowCache(conn, job_id, shard_id, stage_id, threshold, batch_id)
        events = 0
        late = 0
        touched = 0
        closed_count = 0
        with tx(conn):
            # Re-check inside the transaction; another attempt may have
            # committed while this worker was starting.
            current = conn.execute(
                """
                SELECT committed_batch FROM stage_shards
                WHERE job_id=? AND stage_id=? AND shard_id=?
                """,
                (job_id, stage_id, shard_id),
            ).fetchone()
            if current and int(current["committed_batch"]) >= batch_id:
                return {
                    "ok": True,
                    "task": "window",
                    "job_id": job_id,
                    "batch_id": batch_id,
                    "shard_id": shard_id,
                    "noop": True,
                    "committed_batch": int(current["committed_batch"]),
                }
            if route_path:
                for row in _iter_rows(route_path):
                    events += 1
                    vid = row.get(vehicle_column)
                    if vid is None:
                        raise EngineError(
                            "IOV_INPUT_INVALID",
                            f"routed event missing {vehicle_column}",
                            fragment=canonical_json(row)[:300],
                        )
                    vid = str(vid)
                    try:
                        wstart = int(parse_time(evaluator.eval(row)))
                    except EngineError:
                        raise
                    except Exception as exc:
                        raise EngineError(
                            "IOV_INPUT_INVALID",
                            f"window expression failed: {exc}",
                            fragment=canonical_json(row)[:300],
                        ) from exc
                    wend = wstart + window_size
                    if wend <= watermark:
                        _increment_late(conn, job_id, vid, wstart, wend, batch_id)
                        late += 1
                        continue
                    acc, is_closed, existing_end = cache.get(vid, wstart)
                    if is_closed:
                        _increment_late(conn, job_id, vid, wstart, wend, batch_id)
                        late += 1
                        continue
                    add_event(acc, aggregates, row)
                    cache.put(vid, wstart, wend, acc, False)
                    conn.execute(
                        """
                        INSERT OR IGNORE INTO touched_windows (
                            job_id, batch_id, vehicle_id, window_start, window_end
                        ) VALUES (?,?,?,?,?)
                        """,
                        (job_id, batch_id, vid, wstart, wend),
                    )
                    touched += 1
            cache.flush()
            rows = conn.execute(
                """
                SELECT vehicle_id, window_start, window_end, acc_json
                FROM window_accum
                WHERE job_id=? AND stage_id=? AND shard_id=?
                  AND window_end<=? AND closed=0
                ORDER BY vehicle_id, window_start
                """,
                (job_id, stage_id, shard_id, watermark),
            ).fetchall()
            for row in rows:
                vid = str(row["vehicle_id"])
                wstart = int(row["window_start"])
                wend = int(row["window_end"])
                acc_json = row["acc_json"]
                conn.execute(
                    """
                    UPDATE window_accum SET closed=1, updated_batch=?
                    WHERE job_id=? AND stage_id=? AND shard_id=? AND vehicle_id=? AND window_start=?
                    """,
                    (batch_id, job_id, stage_id, shard_id, vid, wstart),
                )
                conn.execute(
                    """
                    INSERT INTO window_partials (
                        job_id, shard_id, vehicle_id, window_start, window_end,
                        acc_json, closed, batch_id
                    ) VALUES (?,?,?,?,?,?,?,?)
                    ON CONFLICT(job_id, shard_id, vehicle_id, window_start) DO UPDATE SET
                        window_end=excluded.window_end,
                        acc_json=excluded.acc_json,
                        closed=excluded.closed,
                        batch_id=excluded.batch_id
                    """,
                    (job_id, shard_id, vid, wstart, wend, acc_json, 1, batch_id),
                )
                conn.execute(
                    """
                    INSERT OR IGNORE INTO closed_windows (
                        job_id, batch_id, vehicle_id, window_start, window_end
                    ) VALUES (?,?,?,?,?)
                    """,
                    (job_id, batch_id, vid, wstart, wend),
                )
                closed_count += 1
            now = iso()
            conn.execute(
                """
                UPDATE stage_shards SET status='committed', consumed_batch=?,
                    committed_batch=?, watermark=?, last_error=NULL, updated_at=?,
                    owner=?
                WHERE job_id=? AND stage_id=? AND shard_id=?
                """,
                (
                    batch_id,
                    batch_id,
                    watermark,
                    now,
                    task.get("worker_id"),
                    job_id,
                    stage_id,
                    shard_id,
                ),
            )
            if cache.spilled_bytes:
                conn.execute(
                    """
                    UPDATE spill_stats SET spill_bytes=spill_bytes+?, updated_at=?
                    WHERE job_id=?
                    """,
                    (int(cache.spilled_bytes), now, job_id),
                )
        return {
            "ok": True,
            "task": "window",
            "job_id": job_id,
            "batch_id": batch_id,
            "shard_id": shard_id,
            "events": events,
            "late": late,
            "touched": touched,
            "closed": closed_count,
            "spilled_bytes": int(cache.spilled_bytes),
            "committed_batch": batch_id,
        }
    finally:
        conn.close()


def _increment_late(
    conn,
    job_id: str,
    vehicle_id: str,
    window_start: int,
    window_end: int,
    batch_id: int,
) -> None:
    conn.execute(
        """
        INSERT INTO late_windows (
            job_id, vehicle_id, window_start, window_end, late_count,
            first_batch, last_batch
        ) VALUES (?,?,?,?,1,?,?)
        ON CONFLICT(job_id, vehicle_id, window_start) DO UPDATE SET
            late_count=late_windows.late_count+1,
            window_end=excluded.window_end,
            last_batch=excluded.last_batch
        """,
        (job_id, vehicle_id, window_start, window_end, batch_id, batch_id),
    )


def _iter_rows(path: str):
    from .util import iter_jsonl

    yield from iter_jsonl(path)

