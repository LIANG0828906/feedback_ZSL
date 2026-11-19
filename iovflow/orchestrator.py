from __future__ import annotations

import json
import math
import os
import re
import shutil
import time
from pathlib import Path
from typing import Any

from .accum import merge_accs, project_result
from .config import RunConfig
from .errors import EngineError
from .expr import ExprEvaluator
from .input import InputDataset, load_input, scan_counts
from .metrics import MetricsServer
from .resources import MemoryMonitor
from .routing import RoutingPlan, build_routing_plan
from .sql_parser import LogicalPlan, parse_sql
from .storage import (
    get_job,
    mark_stage_batch_tx,
    open_results,
    open_state,
    stage_batch_done,
    tx,
    update_job,
)
from .util import (
    INF_TS,
    append_jsonl,
    canonical_json,
    fingerprint,
    iso,
    iter_jsonl,
    percentile,
    read_json,
    stable_hash_hex,
    tmp_size,
    ts_iso,
    utc_now,
    write_json,
)
from .worker import WorkerPool


class Orchestrator:
    def __init__(
        self,
        config: RunConfig,
        plan: LogicalPlan,
        dataset: InputDataset,
        routing: RoutingPlan,
        scan: dict[str, Any],
    ) -> None:
        self.config = config
        self.plan = plan
        self.dataset = dataset
        self.routing = routing
        self.scan = scan
        self.state_conn = open_state(config.state_db)
        self.results_conn = open_results(config.results_db)
        self.agg_columns = self._ensure_result_columns()
        self.tmp_root = Path(config.temp_dir) / config.job_id
        self.tmp_root.mkdir(parents=True, exist_ok=True)
        self.pool: WorkerPool | None = None
        self.monitor: MemoryMonitor | None = None
        self.metrics: MetricsServer | None = None
        self.start_time = time.perf_counter()
        self.batch_durations: list[float] = []
        self.temp_peak = 0
        self.resumed_from_batch = -1
        self._last_error: EngineError | None = None

    def _ensure_result_columns(self) -> list[str]:
        existing = {
            str(r["name"])
            for r in self.results_conn.execute("PRAGMA table_info(window_results)").fetchall()
        }
        added: list[str] = []
        for agg in self.plan.aggregates:
            name = agg.alias
            if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name):
                continue
            if name in existing:
                added.append(name)
                continue
            typ = "INTEGER" if agg.func in ("count_star", "count") else "REAL"
            try:
                self.results_conn.execute(f'ALTER TABLE window_results ADD COLUMN "{name}" {typ}')
                self.results_conn.commit()
                existing.add(name)
                added.append(name)
            except Exception:
                try:
                    self.results_conn.rollback()
                except Exception:
                    pass
        # Preserve the SQL-visible vehicle/window aliases as convenience
        # columns in addition to the canonical vehicle_id/window_start.
        for name, typ in (
            (self.plan.vehicle_column, "TEXT"),
            (self.plan.window.alias, "TEXT"),
        ):
            if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name):
                continue
            if name in existing:
                if name not in added:
                    added.append(name)
                continue
            try:
                self.results_conn.execute(f'ALTER TABLE window_results ADD COLUMN "{name}" {typ}')
                self.results_conn.commit()
                existing.add(name)
                added.append(name)
            except Exception:
                try:
                    self.results_conn.rollback()
                except Exception:
                    pass
        return added

    # ------------------------------------------------------------------
    # Public entry point
    # ------------------------------------------------------------------
    def run(self) -> dict[str, Any]:
        existing = get_job(self.state_conn, self.config.job_id)
        if existing:
            stored_config = json.loads(existing["config_json"])
            self._validate_resume(existing, stored_config)
            self.plan = LogicalPlan.from_dict(json.loads(existing["plan_json"]))
            self.routing = RoutingPlan.from_dict(json.loads(existing["routing_json"]))
            stored_input = json.loads(existing["input_json"])
            current_input = self.dataset.to_dict()
            if stored_input.get("batch_signature") and current_input.get("batch_signature"):
                if stored_input["batch_signature"] != current_input["batch_signature"]:
                    raise EngineError(
                        "IOV_STATE_INCOMPATIBLE",
                        "input batch signature changed since the job was committed",
                    )
            if existing["status"] == "complete":
                report = json.loads(existing["report_json"]) if existing["report_json"] else {}
                self.state_conn.close()
                self.results_conn.close()
                return report
            self.resumed_from_batch = self._last_committed_batch()
            update_job(
                self.state_conn,
                self.config.job_id,
                status="running",
                started_at=existing.get("started_at") or iso(),
            )
        else:
            if self.config.resume:
                raise EngineError(
                    "IOV_STATE_INCOMPATIBLE",
                    f"job {self.config.job_id!r} does not exist; cannot resume",
                )
            stages = self._stages()
            self._create_job(stages)
            update_job(
                self.state_conn,
                self.config.job_id,
                status="running",
                started_at=iso(),
            )
        self._load_existing_durations()
        try:
            self.pool = WorkerPool(
                self.config.state_db,
                self.config.job_id,
                self.config.parallelism,
                self.config.no_progress_timeout,
                self.config.max_takeovers,
                self.state_conn,
                inject=self.config.inject_kill,
                inject_delay_ms=self.config.inject_delay_ms,
            )
            self.pool.start()
            if self.config.metrics_port:
                self.metrics = MetricsServer(
                    self.config.job_id, self.config.mode, self.config.metrics_port
                )
            self.monitor = MemoryMonitor(
                self._pids,
                self.config.memory_limit_mb,
            )
            self.monitor.start()
            self._run_batches()
            self._run_flush()
            self._finish_success()
            report = self._build_report(status="complete")
            self._write_report(report)
            if self.metrics is not None:
                self.metrics.update(
                    "complete",
                    report,
                    [
                        {
                            "stage_id": s["stage_id"],
                            "committed_batch": s["committed_batch"],
                        }
                        for s in report["stages"]
                    ],
                )
            self._cleanup_success()
            return report
        except EngineError as exc:
            self._last_error = exc
            self._finish_failure(exc)
            raise
        except BaseException as exc:  # pragma: no cover - defensive
            err = EngineError("IOV_INTERNAL", f"{type(exc).__name__}: {exc}")
            self._last_error = err
            self._finish_failure(err)
            raise err from exc
        finally:
            if self.monitor is not None:
                try:
                    self.monitor.stop()
                except Exception:
                    pass
            if self.pool is not None:
                try:
                    self.pool.stop()
                except Exception:
                    pass
            self.state_conn.close()
            self.results_conn.close()

    # ------------------------------------------------------------------
    # Job initialization / metadata
    # ------------------------------------------------------------------
    def _stages(self) -> list[dict[str, Any]]:
        stages = self.plan.default_stages(self.config.parallelism, self.config.max_shards)
        for stage in stages:
            if stage["stage_type"] in ("filter", "group_by_vehicle", "window_aggregate"):
                stage["parallelism"] = self.routing.shard_count
                stage["max_concurrent"] = self.config.parallelism
        return stages

    def _create_job(self, stages: list[dict[str, Any]]) -> None:
        from .storage import create_or_resume_job

        config_dict = self.config.to_dict()
        config_dict["allowed_lateness"] = self.config.allowed_lateness
        create_or_resume_job(
            self.state_conn,
            self.config.job_id,
            config_dict,
            self.plan.to_dict(),
            self.dataset.to_dict(),
            self.routing.to_dict(),
            stages,
        )

    def _validate_resume(self, existing: dict[str, Any], stored: dict[str, Any]) -> None:
        if existing["sql_hash"] != self.plan.sql_hash:
            raise EngineError(
                "IOV_STATE_INCOMPATIBLE",
                "resume SQL hash differs from the committed job",
            )
        checks = {
            "mode": self.config.mode,
            "allowed_lateness": self.config.allowed_lateness,
            "vehicle_column": self.config.vehicle_column,
            "parallelism": self.config.parallelism,
            "max_shards": self.config.max_shards,
            "shard_skew_cap": self.config.shard_skew_cap,
            "dedupe_keys": list(self.config.dedupe_keys),
        }
        for key, expected in checks.items():
            got = stored.get(key)
            if isinstance(got, tuple):
                got = list(got)
            if got != expected:
                raise EngineError(
                    "IOV_STATE_INCOMPATIBLE",
                    f"resume changes {key}: existing={stored.get(key)!r} requested={expected!r}",
                )

    def _pids(self) -> list[int]:
        pids = [os.getpid()]
        if self.pool is not None:
            for proc in self.pool.procs.values():
                if proc.is_alive() and proc.pid:
                    pids.append(int(proc.pid))
        return pids

    # ------------------------------------------------------------------
    # Batch pipeline
    # ------------------------------------------------------------------
    def _run_batches(self) -> None:
        watermarks = self._watermark_map()
        for batch in self.dataset.batches:
            bid = batch.batch_id
            watermark = watermarks[bid]
            # Re-assert the watermark for committed batches too.  This keeps
            # the reporting history complete if a previous process was killed
            # between the stage commit and the history write.
            self._record_watermark(batch, watermark)
            if self._batch_complete(bid):
                self._mark_job_batch(bid)
                continue
            t0 = time.perf_counter()
            if not self._filter_done(bid):
                self._run_filter(batch)
            # One-stage look-ahead: filtering is independent of window state,
            # so the upstream stage may keep progressing while a window shard
            # is being taken over.  Merge still waits for every window shard.
            next_index = bid + 1
            if next_index < len(self.dataset.batches) and not self._filter_done(next_index):
                try:
                    self._run_filter(self.dataset.batches[next_index])
                except EngineError:
                    raise
            self._check_resources()
            if not self._window_done(bid):
                self._run_window(batch, watermark)
            self._check_resources()
            if not self._merge_done(bid):
                self._run_merge(bid, watermark)
            self._check_resources()
            if self.config.mode == "stream" and not self._sink_done(bid):
                self._publish()
                self._mark_sink_batch(bid)
            self._mark_job_batch(bid)
            duration = time.perf_counter() - t0
            self.batch_durations.append(duration)
            self._store_durations()
            self._store_peak()
            self._check_resources()
            self._update_metrics()
        # At the end of the last minute batch, no more input exists.  The
        # synthetic flush closes every remaining open window so batch and
        # stream return the same final set.

    def _run_flush(self) -> None:
        flush_id = len(self.dataset.batches)
        flush_dir = self.tmp_root / "routes" / f"b{flush_id:06d}"
        flush_dir.mkdir(parents=True, exist_ok=True)
        if not stage_batch_done(self.state_conn, self.config.job_id, "window_aggregate", flush_id):
            tasks = []
            for shard in range(self.routing.shard_count):
                empty = flush_dir / f"shard_{shard:04d}.ndjson"
                if not empty.exists():
                    empty.write_text("", encoding="utf-8")
                tasks.append(self._window_task(flush_id, shard, str(empty), INF_TS))
            assert self.pool is not None
            self.pool.run_tasks(tasks, self.config.job_id, "window_aggregate", flush_id)
            self._verify_window_shards(flush_id)
            self._mark_window_batch(flush_id, INF_TS, synthetic=True)
        if not stage_batch_done(self.state_conn, self.config.job_id, "merge_skew", flush_id):
            self._run_merge(flush_id, INF_TS, flush=True)
        if self.config.mode == "batch":
            self._enqueue_all_completed(flush_id)
        self._publish()
        self._mark_sink_batch(flush_id)
        self._record_flush_watermark(flush_id)
        self._store_peak()

    # ------------------------------------------------------------------
    # Filter / route stage
    # ------------------------------------------------------------------
    def _run_filter(self, batch) -> None:
        bid = batch.batch_id
        out_dir = self.tmp_root / "filter" / f"b{bid:06d}"
        out_dir.mkdir(parents=True, exist_ok=True)
        size = os.path.getsize(batch.path)
        chunks = max(1, self.routing.shard_count)
        tasks = []
        for chunk in range(chunks):
            start = (size * chunk) // chunks
            end = size if chunk == chunks - 1 else (size * (chunk + 1)) // chunks
            tasks.append(
                {
                    "kind": "filter",
                    "stage_id": "filter",
                    "batch_id": bid,
                    "shard_id": chunk,
                    "input_path": batch.path,
                    "start": start,
                    "end": end,
                    "output_path": str(out_dir / f"chunk_{chunk:04d}.ndjson"),
                    "filter_ast": self.plan.filter_ast,
                    "sql": self.plan.sql,
                }
            )
        assert self.pool is not None
        self.pool.run_tasks(tasks, self.config.job_id, "filter", bid)

        routes_dir = self.tmp_root / "routes" / f"b{bid:06d}"
        routes_dir.mkdir(parents=True, exist_ok=True)
        route_tmp = {
            shard: routes_dir / f"shard_{shard:04d}.ndjson.{os.getpid()}.tmp"
            for shard in range(self.routing.shard_count)
        }
        route_final = {
            shard: routes_dir / f"shard_{shard:04d}.ndjson"
            for shard in range(self.routing.shard_count)
        }
        handles = {
            shard: open(path, "w", encoding="utf-8", newline="\n")
            for shard, path in route_tmp.items()
        }
        counts = {shard: 0 for shard in range(self.routing.shard_count)}
        kept = 0
        handles_closed = False
        dedupe_keys = tuple(
            self.plan.vehicle_column if k == "vehicle_id" else k
            for k in self.config.dedupe_keys
        )
        try:
            with tx(self.state_conn):
                if stage_batch_done(self.state_conn, self.config.job_id, "filter", bid):
                    return
                for chunk in range(chunks):
                    path = out_dir / f"chunk_{chunk:04d}.ndjson"
                    for row in iter_jsonl(path):
                        fp = fingerprint(row, dedupe_keys)
                        if self.state_conn.execute(
                            "SELECT 1 FROM dedupe_keys WHERE job_id=? AND fingerprint=?",
                            (self.config.job_id, fp),
                        ).fetchone():
                            continue
                        vid = row.get(self.plan.vehicle_column)
                        if vid is None:
                            raise EngineError(
                                "IOV_INPUT_INVALID",
                                f"event missing vehicle column {self.plan.vehicle_column!r}",
                                fragment=canonical_json(row)[:300],
                            )
                        shard = self.routing.shard_for(str(vid), row.get("trip_id"), fp)
                        self.state_conn.execute(
                            "INSERT INTO dedupe_keys (job_id, fingerprint, batch_id) VALUES (?,?,?)",
                            (self.config.job_id, fp, bid),
                        )
                        handles[shard].write(
                            json.dumps(row, ensure_ascii=False, sort_keys=True, default=str) + "\n"
                        )
                        counts[shard] += 1
                        kept += 1
                # Route files are published before the SQLite commit.  If the
                # process dies between the two, the transaction rolls back and
                # the next attempt safely overwrites these final files.
                for fh in handles.values():
                    fh.close()
                handles_closed = True
                for shard in range(self.routing.shard_count):
                    os.replace(route_tmp[shard], route_final[shard])
                self.state_conn.execute(
                    "DELETE FROM batch_routes WHERE job_id=? AND batch_id=?",
                    (self.config.job_id, bid),
                )
                for shard in range(self.routing.shard_count):
                    p = route_final[shard]
                    self.state_conn.execute(
                        """
                        INSERT INTO batch_routes (
                            job_id, batch_id, shard_id, path, rows, bytes
                        ) VALUES (?,?,?,?,?,?)
                        """,
                        (
                            self.config.job_id,
                            bid,
                            shard,
                            str(p),
                            counts[shard],
                            p.stat().st_size,
                        ),
                    )
                for shard in range(self.routing.shard_count):
                    for stage_id in ("filter", "group_by_vehicle"):
                        self.state_conn.execute(
                            """
                            UPDATE stage_shards SET status='committed', consumed_batch=?,
                                committed_batch=?, updated_at=?
                            WHERE job_id=? AND stage_id=? AND shard_id=?
                            """,
                            (bid, bid, iso(), self.config.job_id, stage_id, shard),
                        )
                mark_stage_batch_tx(
                    self.state_conn,
                    self.config.job_id,
                    "filter",
                    bid,
                    None,
                    details={"kept": kept, "routed": counts},
                )
                mark_stage_batch_tx(
                    self.state_conn,
                    self.config.job_id,
                    "group_by_vehicle",
                    bid,
                    None,
                    details={"shard_rows": counts},
                )
        finally:
            if not handles_closed:
                for fh in handles.values():
                    try:
                        fh.close()
                    except Exception:
                        pass

    # ------------------------------------------------------------------
    # Window stage
    # ------------------------------------------------------------------
    def _run_window(self, batch, watermark: int) -> None:
        bid = batch.batch_id
        route_rows = {
            int(r["shard_id"]): str(r["path"])
            for r in self.state_conn.execute(
                "SELECT shard_id, path FROM batch_routes WHERE job_id=? AND batch_id=?",
                (self.config.job_id, bid),
            ).fetchall()
        }
        tasks = []
        for shard in range(self.routing.shard_count):
            committed = self.state_conn.execute(
                """
                SELECT committed_batch FROM stage_shards
                WHERE job_id=? AND stage_id='window_aggregate' AND shard_id=?
                """,
                (self.config.job_id, shard),
            ).fetchone()
            if committed and int(committed["committed_batch"]) >= bid:
                continue
            path = route_rows.get(shard)
            if path is None or not Path(path).exists():
                raise EngineError(
                    "IOV_STATE_CORRUPT",
                    f"route file missing for batch {bid} shard {shard}",
                )
            tasks.append(self._window_task(bid, shard, path, watermark))
        if tasks:
            assert self.pool is not None
            self.pool.run_tasks(tasks, self.config.job_id, "window_aggregate", bid)
        self._verify_window_shards(bid)
        self._mark_window_batch(bid, watermark)

    def _window_task(self, bid: int, shard: int, route_path: str, watermark: int) -> dict[str, Any]:
        return {
            "kind": "window",
            "job_id": self.config.job_id,
            "stage_id": "window_aggregate",
            "batch_id": bid,
            "shard_id": shard,
            "route_path": route_path,
            "watermark": watermark,
            "window_size": self.plan.window.size_seconds,
            "vehicle_column": self.plan.vehicle_column,
            "aggregates": [a.to_dict() for a in self.plan.aggregates],
            "window": self.plan.window.to_dict(),
            "state_db": self.config.state_db,
            "state_memory_mb": self.config.state_memory_mb,
        }

    def _verify_window_shards(self, bid: int) -> None:
        missing = [
            int(r["shard_id"])
            for r in self.state_conn.execute(
                """
                SELECT shard_id FROM stage_shards
                WHERE job_id=? AND stage_id='window_aggregate' AND committed_batch<?
                ORDER BY shard_id
                """,
                (self.config.job_id, bid),
            ).fetchall()
        ]
        if missing:
            raise EngineError(
                "IOV_STATE_CORRUPT",
                f"window stage has uncommitted shards for batch {bid}: {missing}",
            )

    def _mark_window_batch(self, bid: int, watermark: int, synthetic: bool = False) -> None:
        with tx(self.state_conn):
            mark_stage_batch_tx(
                self.state_conn,
                self.config.job_id,
                "window_aggregate",
                bid,
                watermark,
                synthetic=synthetic,
            )

    # ------------------------------------------------------------------
    # Merge stage
    # ------------------------------------------------------------------
    def _run_merge(self, bid: int, watermark: int, flush: bool = False) -> None:
        with tx(self.state_conn):
            if stage_batch_done(self.state_conn, self.config.job_id, "merge_skew", bid):
                return
            keys = self.state_conn.execute(
                """
                SELECT vehicle_id, window_start, window_end
                FROM touched_windows WHERE job_id=? AND batch_id=?
                UNION
                SELECT vehicle_id, window_start, window_end
                FROM closed_windows WHERE job_id=? AND batch_id=?
                ORDER BY vehicle_id, window_start
                """,
                (self.config.job_id, bid, self.config.job_id, bid),
            ).fetchall()
            completed = 0
            provisional = 0
            now = iso()
            for key in keys:
                vid = str(key["vehicle_id"])
                wstart = int(key["window_start"])
                wend = int(key["window_end"])
                acc_rows = self.state_conn.execute(
                    """
                    SELECT shard_id, acc_json FROM window_accum
                    WHERE job_id=? AND vehicle_id=? AND window_start=?
                    ORDER BY shard_id
                    """,
                    (self.config.job_id, vid, wstart),
                ).fetchall()
                if not acc_rows:
                    continue
                acc = merge_accs(
                    [json.loads(r["acc_json"]) for r in acc_rows],
                    self.plan.aggregates,
                )
                result = project_result(acc, self.plan.aggregates, vid, wstart, wend)
                if self.plan.window.alias != "window_start":
                    result[self.plan.window.alias] = result["window_start"]
                if self.plan.vehicle_column != "vehicle_id":
                    result[self.plan.vehicle_column] = vid
                result_json = canonical_json(result)
                is_closed = wend <= watermark
                existing = self.state_conn.execute(
                    """
                    SELECT result_json FROM completed_windows
                    WHERE job_id=? AND vehicle_id=? AND window_start=?
                    """,
                    (self.config.job_id, vid, wstart),
                ).fetchone()
                if is_closed:
                    if existing is None:
                        self.state_conn.execute(
                            """
                            INSERT INTO completed_windows (
                                job_id, vehicle_id, window_start, window_end,
                                result_json, closed_batch, watermark, emitted_at
                            ) VALUES (?,?,?,?,?,?,?,?)
                            """,
                            (
                                self.config.job_id,
                                vid,
                                wstart,
                                wend,
                                result_json,
                                bid,
                                watermark,
                                now,
                            ),
                        )
                        completed += 1
                        if self.config.mode == "stream":
                            self._insert_outbox(bid, vid, wstart, wend, result_json, 1, now)
                    # Completed windows are immutable.  Replays only verify
                    # that the outbox entry exists (for a crash between stages).
                    elif self.config.mode == "stream":
                        self._insert_outbox(
                            bid, vid, wstart, wend, result_json, 1, now, ignore_only=True
                        )
                elif self.config.mode == "stream" and existing is None:
                    self._insert_outbox(bid, vid, wstart, wend, result_json, 0, now, ignore_only=True)
                    provisional += 1
            mark_stage_batch_tx(
                self.state_conn,
                self.config.job_id,
                "merge_skew",
                bid,
                watermark,
                synthetic=flush,
                details={"keys": len(keys), "completed": completed, "provisional": provisional},
            )

    def _insert_outbox(
        self,
        bid: int,
        vid: str,
        wstart: int,
        wend: int,
        result_json: str,
        is_final: int,
        now: str,
        ignore_only: bool = False,
    ) -> None:
        self.state_conn.execute(
            f"""
            INSERT OR IGNORE INTO outbox (
                job_id, batch_id, vehicle_id, window_start, window_end,
                result_json, is_final, created_at
            ) VALUES (?,?,?,?,?,?,?,?)
            """,
            (self.config.job_id, bid, vid, wstart, wend, result_json, is_final, now),
        )

    def _enqueue_all_completed(self, bid: int) -> None:
        now = iso()
        with tx(self.state_conn):
            rows = self.state_conn.execute(
                """
                SELECT c.vehicle_id, c.window_start, c.window_end, c.result_json
                FROM completed_windows c
                LEFT JOIN outbox o
                  ON o.job_id=c.job_id AND o.vehicle_id=c.vehicle_id
                 AND o.window_start=c.window_start
                WHERE c.job_id=? AND o.id IS NULL
                ORDER BY c.vehicle_id, c.window_start
                """,
                (self.config.job_id,),
            ).fetchall()
            for row in rows:
                self._insert_outbox(
                    bid,
                    str(row["vehicle_id"]),
                    int(row["window_start"]),
                    int(row["window_end"]),
                    str(row["result_json"]),
                    1,
                    now,
                )

    # ------------------------------------------------------------------
    # Sink stage
    # ------------------------------------------------------------------
    def _publish(self) -> int:
        state = self.state_conn
        results = self.results_conn
        progress = state.execute(
            "SELECT last_outbox_id FROM sink_progress WHERE job_id=?",
            (self.config.job_id,),
        ).fetchone()
        last_id = int(progress["last_outbox_id"]) if progress else 0
        rows = state.execute(
            "SELECT * FROM outbox WHERE job_id=? AND id>? ORDER BY id",
            (self.config.job_id, last_id),
        ).fetchall()
        if not rows:
            return 0
        now = iso()
        published = 0
        batches: set[int] = set()
        for row in rows:
            result = json.loads(row["result_json"])
            wstart = int(row["window_start"])
            wend = int(row["window_end"])
            pub = int(row["id"])
            extra_cols = [
                c
                for c in self.agg_columns
                if c in result and c not in ("vehicle_id", "window_start", "window_end")
            ]
            extra_sql = "".join(f', "{c}"' for c in extra_cols)
            extra_ph = "".join(",?" for _ in extra_cols)
            extra_update = "".join(
                f', "{c}"=excluded."{c}"' for c in extra_cols
            )
            results.execute(
                f"""
                INSERT INTO window_results (
                    job_id, vehicle_id, window_start, window_start_epoch,
                    window_end, window_end_epoch, result_json, is_final,
                    version_batch, publish_seq, updated_at{extra_sql}
                ) VALUES (?,?,?,?,?,?,?,?,?,?,?{extra_ph})
                ON CONFLICT(job_id, vehicle_id, window_start_epoch) DO UPDATE SET
                    window_start=excluded.window_start,
                    window_end=excluded.window_end,
                    window_end_epoch=excluded.window_end_epoch,
                    result_json=excluded.result_json,
                    is_final=excluded.is_final,
                    version_batch=excluded.version_batch,
                    publish_seq=excluded.publish_seq,
                    updated_at=excluded.updated_at{extra_update}
                WHERE excluded.publish_seq >= window_results.publish_seq
                  AND window_results.is_final=0
                """,
                (
                    self.config.job_id,
                    str(row["vehicle_id"]),
                    result.get("window_start", ts_iso(wstart)),
                    wstart,
                    result.get("window_end", ts_iso(wend)),
                    wend,
                    row["result_json"],
                    int(row["is_final"]),
                    int(row["batch_id"]),
                    pub,
                    now,
                    *[result.get(c) for c in extra_cols],
                ),
            )
            results.execute(
                """
                INSERT OR IGNORE INTO result_versions (
                    job_id, vehicle_id, window_start_epoch, publish_seq,
                    result_json, is_final, version_batch, created_at
                ) VALUES (?,?,?,?,?,?,?,?)
                """,
                (
                    self.config.job_id,
                    str(row["vehicle_id"]),
                    wstart,
                    pub,
                    row["result_json"],
                    int(row["is_final"]),
                    int(row["batch_id"]),
                    now,
                ),
            )
            published += 1
            batches.add(int(row["batch_id"]))
        # Late-count side table is separate and immutable with respect to the
        # emitted aggregate rows.
        for late in state.execute(
            "SELECT * FROM late_windows WHERE job_id=?", (self.config.job_id,)
        ).fetchall():
            results.execute(
                """
                INSERT INTO late_windows (
                    job_id, vehicle_id, window_start, window_end,
                    late_count, first_batch, last_batch, updated_at
                ) VALUES (?,?,?,?,?,?,?,?)
                ON CONFLICT(job_id, vehicle_id, window_start) DO UPDATE SET
                    late_count=excluded.late_count,
                    last_batch=excluded.last_batch,
                    updated_at=excluded.updated_at
                """,
                (
                    self.config.job_id,
                    str(late["vehicle_id"]),
                    int(late["window_start"]),
                    int(late["window_end"]),
                    int(late["late_count"]),
                    int(late["first_batch"]),
                    int(late["last_batch"]),
                    now,
                ),
            )
        results.commit()
        new_last = int(rows[-1]["id"])
        with tx(state):
            state.execute(
                """
                INSERT INTO sink_progress (
                    job_id, last_outbox_id, published_batches, updated_at
                ) VALUES (?,?,?,?)
                ON CONFLICT(job_id) DO UPDATE SET
                    last_outbox_id=excluded.last_outbox_id,
                    published_batches=sink_progress.published_batches+1,
                    updated_at=excluded.updated_at
                """,
                (self.config.job_id, new_last, len(batches), now),
            )
        return published

    def _mark_sink_batch(self, bid: int) -> None:
        with tx(self.state_conn):
            mark_stage_batch_tx(
                self.state_conn,
                self.config.job_id,
                "sink",
                bid,
                None,
            )

    # ------------------------------------------------------------------
    # Progress helpers
    # ------------------------------------------------------------------
    def _watermark_map(self) -> dict[int, int]:
        rows = self.state_conn.execute(
            "SELECT batch_id, declared_epoch FROM job_batches WHERE job_id=? ORDER BY batch_id",
            (self.config.job_id,),
        ).fetchall()
        out: dict[int, int] = {}
        maximum: int | None = None
        for row in rows:
            declared = int(row["declared_epoch"])
            maximum = declared if maximum is None else max(maximum, declared)
            out[int(row["batch_id"])] = int(maximum) - self.config.allowed_lateness
        return out

    def _record_watermark(self, batch, watermark: int) -> None:
        with tx(self.state_conn):
            self.state_conn.execute(
                """
                INSERT OR REPLACE INTO watermark_history (
                    job_id, batch_id, declared_epoch, declared_event_time,
                    watermark, consumed_at
                ) VALUES (?,?,?,?,?,?)
                """,
                (
                    self.config.job_id,
                    batch.batch_id,
                    batch.declared_epoch,
                    ts_iso(batch.declared_epoch),
                    watermark,
                    iso(),
                ),
            )

    def _record_flush_watermark(self, flush_id: int) -> None:
        with tx(self.state_conn):
            self.state_conn.execute(
                """
                INSERT OR REPLACE INTO watermark_history (
                    job_id, batch_id, declared_epoch, declared_event_time,
                    watermark, consumed_at
                ) VALUES (?,?,?,?,?,?)
                """,
                (
                    self.config.job_id,
                    flush_id,
                    max((b.declared_epoch for b in self.dataset.batches), default=0),
                    "INF",
                    INF_TS,
                    iso(),
                ),
            )

    def _filter_done(self, bid: int) -> bool:
        return stage_batch_done(self.state_conn, self.config.job_id, "filter", bid)

    def _window_done(self, bid: int) -> bool:
        return stage_batch_done(self.state_conn, self.config.job_id, "window_aggregate", bid)

    def _merge_done(self, bid: int) -> bool:
        return stage_batch_done(self.state_conn, self.config.job_id, "merge_skew", bid)

    def _sink_done(self, bid: int) -> bool:
        return stage_batch_done(self.state_conn, self.config.job_id, "sink", bid)

    def _batch_complete(self, bid: int) -> bool:
        row = self.state_conn.execute(
            "SELECT filter_committed, window_committed, merge_committed, sink_committed "
            "FROM job_batches WHERE job_id=? AND batch_id=?",
            (self.config.job_id, bid),
        ).fetchone()
        if not row:
            return False
        if self.config.mode == "stream":
            return all(int(row[k]) for k in row.keys())
        return bool(row["filter_committed"] and row["window_committed"] and row["merge_committed"])

    def _mark_job_batch(self, bid: int) -> None:
        with tx(self.state_conn):
            self.state_conn.execute(
                "UPDATE job_batches SET committed_at=COALESCE(committed_at, ?) "
                "WHERE job_id=? AND batch_id=?",
                (iso(), self.config.job_id, bid),
            )

    def _last_committed_batch(self) -> int:
        rows = self.state_conn.execute(
            """
            SELECT batch_id, filter_committed, window_committed, merge_committed, sink_committed
            FROM job_batches WHERE job_id=? ORDER BY batch_id
            """,
            (self.config.job_id,),
        ).fetchall()
        last = -1
        for row in rows:
            if self.config.mode == "stream":
                ok = all(int(row[k]) for k in row.keys() if k != "batch_id")
            else:
                ok = bool(
                    row["filter_committed"]
                    and row["window_committed"]
                    and row["merge_committed"]
                )
            if not ok:
                break
            last = int(row["batch_id"])
        return last

    # ------------------------------------------------------------------
    # Resources / durations
    # ------------------------------------------------------------------
    def _check_resources(self) -> None:
        if self.monitor is not None:
            self.monitor.check()
        size = tmp_size(self.tmp_root)
        self.temp_peak = max(self.temp_peak, size)
        if size > self.config.temp_limit_mb * 1024 * 1024:
            raise EngineError(
                "IOV_RESOURCE_TEMP",
                f"temporary data exceeded {self.config.temp_limit_mb} MiB",
                context={"temp_bytes": size, "limit_mb": self.config.temp_limit_mb},
            )

    def _load_existing_durations(self) -> None:
        row = self.state_conn.execute(
            "SELECT batch_durations_json FROM spill_stats WHERE job_id=?",
            (self.config.job_id,),
        ).fetchone()
        if row and row["batch_durations_json"]:
            try:
                self.batch_durations = [float(x) for x in json.loads(row["batch_durations_json"])]
            except Exception:
                self.batch_durations = []

    def _store_durations(self) -> None:
        with tx(self.state_conn):
            self.state_conn.execute(
                "UPDATE spill_stats SET batch_durations_json=?, updated_at=? WHERE job_id=?",
                (
                    json.dumps(self.batch_durations[-512:]),
                    iso(),
                    self.config.job_id,
                ),
            )

    def _store_peak(self) -> None:
        peak = self.monitor.peak_mb if self.monitor is not None else 0.0
        with tx(self.state_conn):
            self.state_conn.execute(
                """
                UPDATE spill_stats SET peak_memory_mb=MAX(peak_memory_mb, ?),
                    temp_bytes_peak=MAX(temp_bytes_peak, ?), updated_at=?
                WHERE job_id=?
                """,
                (peak, self.temp_peak, iso(), self.config.job_id),
            )

    def _update_metrics(self) -> None:
        if self.metrics is None:
            return
        stages = [
            {
                "stage_id": r["stage_id"],
                "committed_batch": r["committed_batch"],
            }
            for r in self.state_conn.execute(
                "SELECT stage_id, committed_batch FROM stages WHERE job_id=?",
                (self.config.job_id,),
            ).fetchall()
        ]
        self.metrics.update("running", None, stages)

    # ------------------------------------------------------------------
    # Completion / report
    # ------------------------------------------------------------------
    def _finish_success(self) -> None:
        self._store_peak()
        with tx(self.state_conn):
            for stage_id in ("filter", "group_by_vehicle", "window_aggregate", "merge_skew", "sink"):
                self.state_conn.execute(
                    "UPDATE stages SET status='complete', updated_at=? "
                    "WHERE job_id=? AND stage_id=?",
                    (iso(), self.config.job_id, stage_id),
                )
        update_job(
            self.state_conn,
            self.config.job_id,
            status="complete",
            finished_at=iso(),
            error_code=None,
            error_message=None,
        )

    def _finish_failure(self, exc: EngineError) -> None:
        try:
            self._store_peak()
        except Exception:
            pass
        try:
            report = self._build_report(status="failed", error=exc.to_dict()["error"])
            self._write_report(report)
            if self.metrics is not None:
                self.metrics.update("failed", report)
        except Exception:
            pass
        try:
            update_job(
                self.state_conn,
                self.config.job_id,
                status="failed",
                finished_at=iso(),
                error_code=exc.code,
                error_message=exc.message,
                report_json=canonical_json(report) if "report" in locals() else None,
            )
        except Exception:
            pass

    def _build_report(
        self,
        status: str,
        error: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        state = self.state_conn
        job = get_job(state, self.config.job_id) or {}
        stages = [
            {
                "stage_id": r["stage_id"],
                "stage_type": r["stage_type"],
                "ordinal": r["ordinal"],
                "parallelism": r["parallelism"],
                "max_concurrent": r["max_concurrent"],
                "input_from": r["input_from"],
                "output_to": r["output_to"],
                "status": r["status"],
                "consumed_batch": r["consumed_batch"],
                "committed_batch": r["committed_batch"],
                "watermark": r["watermark"],
            }
            for r in state.execute(
                "SELECT * FROM stages WHERE job_id=? ORDER BY ordinal", (self.config.job_id,)
            ).fetchall()
        ]
        shards = [
            {
                "stage_id": r["stage_id"],
                "shard_id": r["shard_id"],
                "status": r["status"],
                "committed_batch": r["committed_batch"],
                "watermark": r["watermark"],
                "takeover_count": r["takeover_count"],
            }
            for r in state.execute(
                "SELECT * FROM stage_shards WHERE job_id=? ORDER BY stage_id, shard_id",
                (self.config.job_id,),
            ).fetchall()
        ]
        recoveries = [
            dict(r)
            for r in state.execute(
                "SELECT * FROM recoveries WHERE job_id=? ORDER BY id", (self.config.job_id,)
            ).fetchall()
        ]
        completed = int(
            state.execute(
                "SELECT COUNT(*) FROM completed_windows WHERE job_id=?", (self.config.job_id,)
            ).fetchone()[0]
        )
        late_rows = int(
            state.execute(
                "SELECT COUNT(*) FROM late_windows WHERE job_id=?", (self.config.job_id,)
            ).fetchone()[0]
        )
        late_events = int(
            state.execute(
                "SELECT COALESCE(SUM(late_count),0) FROM late_windows WHERE job_id=?",
                (self.config.job_id,),
            ).fetchone()[0]
        )
        final_outputs = int(
            self.results_conn.execute(
                "SELECT COUNT(*) FROM window_results WHERE job_id=? AND is_final=1",
                (self.config.job_id,),
            ).fetchone()[0]
        )
        versions = int(
            self.results_conn.execute(
                "SELECT COUNT(*) FROM result_versions WHERE job_id=?",
                (self.config.job_id,),
            ).fetchone()[0]
        )
        spill = state.execute(
            "SELECT * FROM spill_stats WHERE job_id=?", (self.config.job_id,)
        ).fetchone()
        watermark_rows = state.execute(
            "SELECT * FROM watermark_history WHERE job_id=? ORDER BY batch_id",
            (self.config.job_id,),
        ).fetchall()
        real_watermarks = [
            r for r in watermark_rows if int(r["batch_id"]) < len(self.dataset.batches)
        ]
        flush_watermark = next(
            (
                int(r["watermark"])
                for r in watermark_rows
                if int(r["batch_id"]) >= len(self.dataset.batches)
            ),
            None,
        )
        last_batch = self._last_committed_batch()
        takeovers = sum(int(r["takeover_count"]) for r in shards)
        mode = self.config.mode
        duration = time.perf_counter() - self.start_time
        p95 = percentile(self.batch_durations, 95) if self.batch_durations else 0.0
        routing = self.routing.to_dict()
        max_ratio = max(routing["shard_ratios"]) if routing.get("shard_ratios") else 0.0
        report = {
            "job_id": self.config.job_id,
            "mode": mode,
            "status": status,
            "sql_hash": job.get("sql_hash") or self.plan.sql_hash,
            "started_at": job.get("started_at"),
            "finished_at": iso(),
            "duration_seconds": round(duration, 6),
            "input": {
                "root": self.dataset.root,
                "manifest": self.dataset.manifest_path,
                "batch_count": len(self.dataset.batches),
                "rows": self.dataset.total_rows,
                "stats": self.dataset.stats,
                "processed_unique_rows": self.scan["total_unique"],
                "processed_duplicate_rows": self.scan["duplicate_rows"],
                "filtered_out_rows": self.scan.get("filtered_out_rows", 0),
                "input_duplicate_rows": self.dataset.stats.get("duplicate_rows"),
            },
            "watermark": {
                "allowed_lateness_seconds": self.config.allowed_lateness,
                "final": int(real_watermarks[-1]["watermark"]) if real_watermarks else None,
                "flush": flush_watermark,
                "history_count": len(watermark_rows),
                "semantics": "max(declared_event_time of consumed batches) - allowed_lateness",
            },
            "stages": stages,
            "shards": shards,
            "routing": {
                "shard_count": routing["shard_count"],
                "shard_counts": routing["shard_counts"],
                "shard_ratios": routing["shard_ratios"],
                "max_shard_ratio": max_ratio,
                "skew_cap": routing["skew_cap"],
                "split_vehicle_count": len(routing["split_vehicles"]),
                "split_vehicles_sample": routing["split_vehicles"][:20],
            },
            "checkpoint": {
                "last_committed_batch": last_batch,
                "resumed_from_batch": self.resumed_from_batch,
                "consumed_batches": last_batch + 1,
                "resume_safe": True,
            },
            "fault_recovery": {
                "takeover_count": takeovers,
                "events": recoveries,
            },
            "results": {
                "completed_windows": completed,
                "final_output_rows": final_outputs,
                "published_versions": versions,
                "late_windows": late_rows,
                "late_events": late_events,
            },
            "resources": {
                "cpu_limit": 2,
                "parallelism": self.config.parallelism,
                "memory_limit_mb": self.config.memory_limit_mb,
                "peak_memory_mb": round(
                    float(spill["peak_memory_mb"]) if spill else 0.0, 2
                ),
                "spill_bytes": int(spill["spill_bytes"]) if spill else 0,
                "temp_bytes_peak": int(spill["temp_bytes_peak"]) if spill else self.temp_peak,
                "temp_limit_mb": self.config.temp_limit_mb,
            },
            "performance": {
                "batch_count": len(self.batch_durations),
                "batch_p95_seconds": round(p95, 6),
                "batch_max_seconds": round(max(self.batch_durations), 6)
                if self.batch_durations
                else 0.0,
                "batch_mode_end_to_end_limit_seconds": 180,
                "stream_batch_p95_limit_seconds": 2.0,
            },
            "error": error,
        }
        return report

    def _write_report(self, report: dict[str, Any]) -> None:
        path = Path(self.config.report_dir)
        path.mkdir(parents=True, exist_ok=True)
        write_json(path / f"{self.config.job_id}.json", report)
        write_json(path / "latest.json", report)
        try:
            self.results_conn.execute(
                """
                INSERT INTO job_reports (job_id, report_json, updated_at)
                VALUES (?,?,?)
                ON CONFLICT(job_id) DO UPDATE SET
                    report_json=excluded.report_json,
                    updated_at=excluded.updated_at
                """,
                (self.config.job_id, canonical_json(report), iso()),
            )
            self.results_conn.commit()
        except Exception:
            try:
                self.results_conn.rollback()
            except Exception:
                pass
        update_job(
            self.state_conn,
            self.config.job_id,
            report_json=canonical_json(report),
        )

    def _cleanup_success(self) -> None:
        if not self.config.keep_temp:
            try:
                shutil.rmtree(self.tmp_root, ignore_errors=True)
            except Exception:
                pass
        # Keep audit-friendly completed rows and reports; drop transient route
        # and partial state so the state database does not grow without bound.
        try:
            with tx(self.state_conn):
                self.state_conn.execute(
                    "DELETE FROM window_partials WHERE job_id=?", (self.config.job_id,)
                )
                self.state_conn.execute(
                    "DELETE FROM touched_windows WHERE job_id=?", (self.config.job_id,)
                )
                self.state_conn.execute(
                    "DELETE FROM closed_windows WHERE job_id=?", (self.config.job_id,)
                )
                self.state_conn.execute(
                    "DELETE FROM batch_routes WHERE job_id=?", (self.config.job_id,)
                )
                self.state_conn.execute(
                    "DELETE FROM window_accum WHERE job_id=?", (self.config.job_id,)
                )
        except Exception:
            pass


def run_job(config: RunConfig) -> dict[str, Any]:
    plan = parse_sql(config.sql_text, config.vehicle_column)
    dataset = load_input(config.input_dir, plan.vehicle_column)
    evaluator = ExprEvaluator(plan.window.expr_ast, plan.sql)

    def window_start(row: dict[str, Any]) -> int:
        from .util import parse_time

        return int(parse_time(evaluator.eval(row)))

    scan = scan_counts(
        dataset,
        plan.vehicle_column,
        tuple(plan.vehicle_column if k == "vehicle_id" else k for k in config.dedupe_keys),
        window_start,
        row_filter=plan.filter_evaluator(),
    )
    routing = build_routing_plan(
        scan["vehicle_trips"],
        scan["total_unique"],
        config.parallelism,
        config.max_shards,
        config.shard_skew_cap,
        samples=scan["samples"],
    )
    orchestrator = Orchestrator(config, plan, dataset, routing, scan)
    return orchestrator.run()


def status(job_id: str, state_db: str, results_db: str) -> dict[str, Any]:
    state = open_state(state_db)
    results = open_results(results_db)
    try:
        job = get_job(state, job_id)
        if not job:
            raise EngineError("IOV_STATE_INCOMPATIBLE", f"job {job_id!r} not found")
        stages = [dict(r) for r in state.execute(
            "SELECT * FROM stages WHERE job_id=? ORDER BY ordinal", (job_id,)
        ).fetchall()]
        shards = [dict(r) for r in state.execute(
            "SELECT * FROM stage_shards WHERE job_id=? ORDER BY stage_id, shard_id", (job_id,)
        ).fetchall()]
        recoveries = [dict(r) for r in state.execute(
            "SELECT * FROM recoveries WHERE job_id=? ORDER BY id", (job_id,)
        ).fetchall()]
        workers = [dict(r) for r in state.execute(
            "SELECT * FROM job_workers WHERE job_id=? ORDER BY worker_id", (job_id,)
        ).fetchall()]
        completed = state.execute(
            "SELECT COUNT(*) FROM completed_windows WHERE job_id=?", (job_id,)
        ).fetchone()[0]
        late = state.execute(
            "SELECT COALESCE(SUM(late_count),0) FROM late_windows WHERE job_id=?", (job_id,)
        ).fetchone()[0]
        outputs = results.execute(
            "SELECT COUNT(*) FROM window_results WHERE job_id=? AND is_final=1", (job_id,)
        ).fetchone()[0]
        config = json.loads(job["config_json"])
        return {
            "job_id": job_id,
            "status": job["status"],
            "mode": job["mode"],
            "sql_hash": job["sql_hash"],
            "error_code": job["error_code"],
            "error_message": job["error_message"],
            "stages": stages,
            "shards": shards,
            "recoveries": recoveries,
            "workers": workers,
            "takeover_count": sum(int(s["takeover_count"]) for s in shards),
            "completed_windows": int(completed),
            "late_events": int(late),
            "final_output_rows": int(outputs),
            "allowed_lateness": config.get("allowed_lateness"),
            "report": json.loads(job["report_json"]) if job["report_json"] else None,
        }
    finally:
        state.close()
        results.close()


def state_snapshot(
    job_id: str,
    state_db: str,
    batch_id: int | None = None,
) -> dict[str, Any]:
    conn = open_state(state_db)
    try:
        job = get_job(conn, job_id)
        if not job:
            raise EngineError("IOV_STATE_INCOMPATIBLE", f"job {job_id!r} not found")
        if batch_id is None:
            batch_id = int(
                conn.execute(
                    "SELECT COALESCE(MAX(batch_id),-1) FROM watermark_history WHERE job_id=?",
                    (job_id,),
                ).fetchone()[0]
            )
        wm = conn.execute(
            "SELECT * FROM watermark_history WHERE job_id=? AND batch_id=?",
            (job_id, batch_id),
        ).fetchone()
        accum = [
            {
                "vehicle_id": r["vehicle_id"],
                "shard_id": r["shard_id"],
                "window_start": r["window_start"],
                "window_end": r["window_end"],
                "acc": json.loads(r["acc_json"]),
                "closed": bool(r["closed"]),
            }
            for r in conn.execute(
                """
                SELECT * FROM window_accum WHERE job_id=? AND updated_batch<=?
                ORDER BY vehicle_id, window_start, shard_id
                """,
                (job_id, batch_id),
            ).fetchall()
        ]
        completed = [
            {
                "vehicle_id": r["vehicle_id"],
                "window_start": r["window_start"],
                "result": json.loads(r["result_json"]),
            }
            for r in conn.execute(
                """
                SELECT * FROM completed_windows WHERE job_id=? AND closed_batch<=?
                ORDER BY vehicle_id, window_start
                """,
                (job_id, batch_id),
            ).fetchall()
        ]
        late = [
            dict(r)
            for r in conn.execute(
                "SELECT * FROM late_windows WHERE job_id=? ORDER BY vehicle_id, window_start",
                (job_id,),
            ).fetchall()
        ]
        return {
            "job_id": job_id,
            "batch_id": batch_id,
            "watermark": int(wm["watermark"]) if wm else None,
            "declared_event_time": wm["declared_event_time"] if wm else None,
            "open_windows": accum,
            "completed_windows": completed,
            "late_windows": late,
            "deterministic": True,
        }
    finally:
        conn.close()

