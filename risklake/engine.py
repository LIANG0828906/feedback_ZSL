"""Incremental windowed join engine.

Processes the local data directory batch by batch (historical parquet
shards first, then jsonl increment batches), keeping all durable state in
SQLite.  Each batch commits atomically, so the job can be killed after
any batch and resumed from the committed state with results identical to
an uninterrupted run.
"""

from __future__ import annotations

import hashlib
import json
import os
import pickle
import shutil
import time
import tracemalloc
from pathlib import Path

import pyarrow.parquet as pq

from .config import JobConfig
from .errors import InputError, MemoryLimitError
from .slots import assign_slots
from .state import StateStore


def window_start_of(event_time: int, window_seconds: int) -> int:
    return (event_time // window_seconds) * window_seconds


# peak memory is reported at a fixed 64 KiB precision so that repeated
# executions of the same input produce byte-identical reports
PEAK_MEMORY_QUANTUM = 64 * 1024


class JobRunner:
    def __init__(self, cfg: JobConfig, progress=None):
        self.cfg = cfg
        self.progress = progress or (lambda msg: None)
        self.data_dir = cfg.abs_path(cfg.data_dir)
        self.spill_dir = cfg.abs_path(cfg.temp_dir) / f"spill-{os.getpid()}"
        self.spill_bytes = 0
        self._spill_files: list[Path] = []

    # -- input enumeration ----------------------------------------------
    def _list_batches(self) -> list[tuple[str, Path]]:
        """Enumerate batches; numbering gaps become explicit "missing"
        entries so a lost batch is recorded instead of silently skipped."""
        batches: list[tuple[str, Path]] = []
        for kind, found in (
            ("parquet", self._index_by_number("part-", "parquet")),
            ("jsonl", self._index_by_number("batch_", "jsonl")),
        ):
            if not found:
                continue
            for idx in range(max(found) + 1):
                if idx in found:
                    batches.append((kind, found[idx]))
                else:
                    name = f"part-{idx:04d}.parquet" if kind == "parquet" else f"batch_{idx:04d}.jsonl"
                    sub = "historical" if kind == "parquet" else "incremental"
                    batches.append(("missing", self.data_dir / sub / name))
        return batches

    def _index_by_number(self, prefix: str, kind: str) -> dict[int, Path]:
        sub = "historical" if kind == "parquet" else "incremental"
        suffix = "*.parquet" if kind == "parquet" else "batch_*.jsonl"
        found: dict[int, Path] = {}
        for path in sorted((self.data_dir / sub).rglob(suffix)):
            digits = "".join(ch for ch in path.stem if ch.isdigit())
            if digits:
                found[int(digits)] = path
        return found

    @staticmethod
    def _fingerprint(batches: list[tuple[str, Path]]) -> str:
        digest = hashlib.sha256()
        for kind, path in batches:
            digest.update(kind.encode())
            digest.update(str(path.name).encode())
            size = path.stat().st_size if path.exists() else -1
            digest.update(str(size).encode())
        return digest.hexdigest()

    @staticmethod
    def _read_batch(kind: str, path: Path) -> list[dict]:
        if kind == "parquet":
            return pq.read_table(path).to_pylist()
        events = []
        with path.open("r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line:
                    events.append(json.loads(line))
        return events

    # -- spill ------------------------------------------------------------
    def _maybe_spill(self, agg: dict) -> None:
        threshold = self.cfg.state_mem_threshold_bytes
        if len(agg) * self.cfg.spill_entry_bytes <= threshold:
            return
        self.spill_dir.mkdir(parents=True, exist_ok=True)
        path = self.spill_dir / f"spill-{len(self._spill_files):05d}.pkl"
        payload = pickle.dumps(sorted(agg.items()))
        path.write_bytes(payload)
        self.spill_bytes += len(payload)
        self._spill_files.append(path)
        agg.clear()

    def _merge_spills(self, agg: dict) -> dict:
        if not self._spill_files:
            return agg
        merged: dict = {}
        for path in self._spill_files:
            for key, value in pickle.loads(path.read_bytes()):
                acc = merged.setdefault(key, [0, 0, 0, 0])
                for i in range(4):
                    acc[i] += value[i]
            path.unlink()
        for key, value in agg.items():
            acc = merged.setdefault(key, [0, 0, 0, 0])
            for i in range(4):
                acc[i] += value[i]
        self._spill_files.clear()
        agg.clear()
        return merged

    # -- main loop ---------------------------------------------------------
    def run(self) -> dict:
        started = time.time()
        generated_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
        if self.spill_dir.exists():
            shutil.rmtree(self.spill_dir, ignore_errors=True)

        batches = self._list_batches()
        fingerprint = self._fingerprint(batches)

        store = StateStore(self.cfg.abs_path(self.cfg.state_db))
        try:
            report = self._run_with_store(store, batches, fingerprint)
        finally:
            store.close()
            shutil.rmtree(self.spill_dir, ignore_errors=True)

        report["generated_at"] = generated_at
        report["duration_seconds"] = round(time.time() - started, 3)
        out_path = self.cfg.abs_path(self.cfg.report_path)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_text(
            json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return report

    def _run_with_store(self, store: StateStore, batches, fingerprint) -> dict:
        cfg = self.cfg
        stored_fp = store.get_meta("input_fingerprint")
        if stored_fp is None:
            with store.transaction() as conn:
                StateStore.set_meta(conn, "input_fingerprint", fingerprint)
        elif stored_fp != fingerprint:
            raise InputError("input data does not match persisted state")

        last_committed = store.get_meta_int("last_committed_batch", -1)
        finalized = store.get_meta("finalized") == "1"
        resumed_with_work = last_committed >= 0 and not finalized
        resume_point = last_committed if resumed_with_work else -1
        recoveries = store.get_meta_int("recoveries", 0)
        if resumed_with_work:
            recoveries += 1
            with store.transaction() as conn:
                StateStore.set_meta(conn, "recoveries", recoveries)

        # runtime counters, rehydrated from durable state
        counters = {
            "events_read": store.get_meta_int("events_read"),
            "dedup_dropped": store.get_meta_int("dedup_dropped"),
            "late_events": store.get_meta_int("late_events"),
            "skipped_batches": store.get_meta_int("skipped_batches"),
        }
        self.spill_bytes = store.get_meta_int("spill_bytes")
        watermark = store.get_meta_int("watermark", -(1 << 62))
        max_seen = store.get_meta_int("max_event_time", -(1 << 62))
        max_load_ratio = float(store.get_meta("max_load_ratio", "0"))
        seen = store.load_seen()
        cum_counts = store.load_key_counts()
        cum_loads = json.loads(
            store.get_meta("slot_loads", "null") or "null"
        ) or [0] * cfg.slots
        total_cum = store.get_meta_int("events_accepted")

        tracemalloc.start()
        try:
            for seq, (kind, path) in enumerate(batches):
                if seq <= last_committed:
                    continue
                if not path.exists():
                    # a missing batch must never move the watermark backwards
                    with store.transaction() as conn:
                        StateStore.set_meta(conn, "last_committed_batch", seq)
                        conn.execute(
                            "INSERT OR IGNORE INTO consumed_batches(batch_seq) VALUES (?)",
                            (seq,),
                        )
                        counters["skipped_batches"] += 1
                        StateStore.set_meta(
                            conn, "skipped_batches", counters["skipped_batches"]
                        )
                    self.progress(f"BATCH_COMMITTED {seq}")
                    continue

                events = self._read_batch(kind, path)
                accepted = []
                late_ids = []
                for e in events:
                    counters["events_read"] += 1
                    event_id = e["event_id"]
                    if event_id in seen:
                        counters["dedup_dropped"] += 1
                        continue
                    seen.add(event_id)
                    event_time = int(e["event_time"])
                    window_start = window_start_of(event_time, cfg.window_seconds)
                    if window_start + cfg.window_seconds <= watermark:
                        counters["late_events"] += 1
                        late_ids.append(event_id)
                        continue
                    if event_time > max_seen:
                        max_seen = event_time
                        candidate = max_seen - cfg.lateness_tolerance_seconds
                        if candidate > watermark:
                            watermark = candidate
                    accepted.append((e, window_start))

                for e, _ in accepted:
                    key = e["order_id"]
                    cum_counts[key] = cum_counts.get(key, 0) + 1
                total_cum += len(accepted)
                slot_of, splits, ratio = assign_slots(
                    [e for e, _ in accepted],
                    cfg.slots,
                    cfg.slot_skew_threshold,
                    cum_counts,
                    cum_loads,
                    total_cum,
                )
                if ratio > max_load_ratio:
                    max_load_ratio = ratio

                agg: dict = {}
                for (e, window_start), _slot in zip(accepted, slot_of):
                    key = (window_start, e["order_id"])
                    acc = agg.setdefault(key, [0, 0, 0, 0])
                    if e["event_type"] == "order":
                        acc[0] += int(e["amount_cents"])
                        acc[1] += 1
                    else:
                        acc[2] += int(e["amount_cents"])
                        acc[3] += 1
                    self._maybe_spill(agg)
                agg = self._merge_spills(agg)

                close_before = watermark - cfg.window_seconds
                with store.transaction() as conn:
                    conn.executemany(
                        "INSERT INTO window_states(window_start, order_id, o_amt, o_cnt, r_amt, r_cnt) "
                        "VALUES (?, ?, ?, ?, ?, ?) "
                        "ON CONFLICT(window_start, order_id) DO UPDATE SET "
                        "o_amt = o_amt + excluded.o_amt, o_cnt = o_cnt + excluded.o_cnt, "
                        "r_amt = r_amt + excluded.r_amt, r_cnt = r_cnt + excluded.r_cnt",
                        [
                            (ws, oid, v[0], v[1], v[2], v[3])
                            for (ws, oid), v in agg.items()
                        ],
                    )
                    # late events are also marked seen so their re-reports
                    # count as dedup drops, matching the uninterrupted run
                    conn.executemany(
                        "INSERT INTO seen_events(event_id) VALUES (?)",
                        [(e["event_id"],) for e, _ in accepted]
                        + [(event_id,) for event_id in late_ids],
                    )
                    conn.execute(
                        "INSERT INTO committed_windows "
                        "SELECT window_start, SUM(o_amt), SUM(o_cnt), SUM(r_amt), SUM(r_cnt), "
                        "SUM(CASE WHEN o_cnt > 0 AND r_cnt > 0 THEN 1 ELSE 0 END), ? "
                        "FROM window_states WHERE window_start <= ? GROUP BY window_start",
                        (seq, close_before),
                    )
                    conn.execute(
                        "DELETE FROM window_states WHERE window_start <= ?",
                        (close_before,),
                    )
                    conn.executemany(
                        "INSERT INTO skew_splits(batch_seq, order_id, parts) VALUES (?, ?, ?)",
                        [(seq, s["key"], s["parts"]) for s in splits],
                    )
                    conn.executemany(
                        "INSERT INTO key_counts(order_id, cnt) VALUES (?, ?) "
                        "ON CONFLICT(order_id) DO UPDATE SET cnt = excluded.cnt",
                        [
                            (key, cum_counts[key])
                            for key in {e["order_id"] for e, _ in accepted}
                        ],
                    )
                    conn.execute(
                        "INSERT OR IGNORE INTO consumed_batches(batch_seq) VALUES (?)",
                        (seq,),
                    )
                    StateStore.set_meta(conn, "slot_loads", json.dumps(cum_loads))
                    StateStore.set_meta(conn, "events_accepted", total_cum)
                    StateStore.set_meta(conn, "watermark", watermark)
                    StateStore.set_meta(conn, "max_event_time", max_seen)
                    StateStore.set_meta(conn, "last_committed_batch", seq)
                    StateStore.set_meta(conn, "events_read", counters["events_read"])
                    StateStore.set_meta(conn, "dedup_dropped", counters["dedup_dropped"])
                    StateStore.set_meta(conn, "late_events", counters["late_events"])
                    StateStore.set_meta(conn, "spill_bytes", self.spill_bytes)
                    StateStore.set_meta(conn, "max_load_ratio", repr(max_load_ratio))
                    StateStore.set_meta(conn, "recoveries", recoveries)
                self.progress(f"BATCH_COMMITTED {seq}")
                self._check_memory()

            if not finalized:
                with store.transaction() as conn:
                    last_seq = len(batches) - 1
                    conn.execute(
                        "INSERT INTO committed_windows "
                        "SELECT window_start, SUM(o_amt), SUM(o_cnt), SUM(r_amt), SUM(r_cnt), "
                        "SUM(CASE WHEN o_cnt > 0 AND r_cnt > 0 THEN 1 ELSE 0 END), ? "
                        "FROM window_states GROUP BY window_start",
                        (last_seq,),
                    )
                    conn.execute("DELETE FROM window_states")
                    StateStore.set_meta(conn, "finalized", "1")

            peak = tracemalloc.get_traced_memory()[1]
            peak = (
                (peak + PEAK_MEMORY_QUANTUM // 2) // PEAK_MEMORY_QUANTUM
            ) * PEAK_MEMORY_QUANTUM
        finally:
            tracemalloc.stop()

        return self._build_report(
            store, batches, counters, watermark, max_load_ratio,
            resume_point, recoveries, peak,
        )

    def _check_memory(self) -> None:
        _, peak = tracemalloc.get_traced_memory()
        if peak > self.cfg.memory_limit_bytes:
            raise MemoryLimitError(
                f"memory limit exceeded: {peak} > {self.cfg.memory_limit_bytes}"
            )

    def _build_report(self, store, batches, counters, watermark,
                      max_load_ratio, resume_point, recoveries, peak) -> dict:
        cfg = self.cfg
        parquet_shards = sum(1 for k, _ in batches if k == "parquet")
        windows = [
            {
                "window_start": w["window_start"],
                "order_amount": round(w["order_amount_cents"] / 100, 2),
                "order_count": w["order_count"],
                "refund_amount": round(w["refund_amount_cents"] / 100, 2),
                "refund_count": w["refund_count"],
                "matched_keys": w["matched_keys"],
            }
            for w in store.committed_windows()
        ]
        return {
            "job": {
                "slots": cfg.slots,
                "window_seconds": cfg.window_seconds,
                "lateness_tolerance_seconds": cfg.lateness_tolerance_seconds,
                "seed": cfg.seed,
            },
            "input": {
                "parquet_shards": parquet_shards,
                "jsonl_batches": len(batches) - parquet_shards,
                "batches_consumed": store.consumed_batch_count(),
                "skipped_batches": counters["skipped_batches"],
                "events_read": counters["events_read"],
                "events_unique": counters["events_read"] - counters["dedup_dropped"],
                "dedup_dropped": counters["dedup_dropped"],
            },
            "watermark": {
                "final": watermark,
                "late_events": counters["late_events"],
            },
            "slots": {
                "count": cfg.slots,
                "max_load_ratio": max_load_ratio,
                "splits": store.skew_splits(),
            },
            "recovery": {
                "resumed_from_batch": resume_point,
                "recoveries": recoveries,
            },
            "resources": {
                "peak_memory_bytes": peak,
                "spill_bytes": self.spill_bytes,
            },
            "windows": windows,
        }


def run_job(cfg: JobConfig, progress=None) -> dict:
    return JobRunner(cfg, progress=progress).run()
