"""Unified window aggregation engine.

The same event-processing function is used for historical Parquet shards and
streaming JSONL batches, which is what keeps the dedup / watermark / window
semantics identical on both sides.
"""

from __future__ import annotations

import os
from concurrent.futures import ThreadPoolExecutor

from .config import FaultPlan, FaultRule, JobConfig
from .errors import InputError, LimitError, StateError
from .io_sources import read_jsonl_batch, read_parquet_shard
from .semantics import (
    SplitPlan,
    dedup_key,
    is_window_closed,
    window_start,
)
from .state import StateBackend
from .util import directory_size, rss_bytes


class KillAfterCommit(Exception):
    """Internal control signal for the kill_after_commit fault injection."""


class Engine:
    def __init__(
        self,
        cfg: JobConfig,
        manifest: dict,
        split_plan: SplitPlan,
        fault_plan: FaultPlan | None = None,
    ):
        self.cfg = cfg
        self.manifest = manifest
        self.split_plan = split_plan
        self.fault_plan = fault_plan or FaultPlan()
        self.state = StateBackend(
            os.path.join(cfg.state_dir, "job"),
            os.path.join(cfg.spill_dir, "job"),
            cfg.state_memory_bytes,
        )
        self.peak_rss = 0
        self.commit_points: list[str] = []
        self.fault_log: list[dict] = []

    # ------------------------------------------------------------- lifecycle
    def open(self) -> bool:
        resumed = self.state.open()
        self.state.initialize_shards(self.cfg.partitions)
        self.state.watermark.allowed_lateness_ms = self.cfg.allowed_lateness_ms
        for entry in self.state.faults:
            if entry not in self.fault_log:
                self.fault_log.append(entry)
        self.peak_rss = rss_bytes()
        return resumed

    def close(self) -> None:
        self.state.cleanup_spill()
        self.state.close()

    # ------------------------------------------------------------- faults
    def _matching_rule(self, fault: str, batch_id: int) -> FaultRule | None:
        for rule in self.fault_plan.rules:
            if rule.fault != fault:
                continue
            if rule.batch is None or rule.batch == batch_id:
                return rule
        return None

    def _record_fault(self, fault: str, batch_id: int) -> None:
        entry = {"fault": fault, "batch": batch_id}
        if entry not in self.fault_log:
            self.fault_log.append(entry)

    def _inject_state_locked(self, batch_id: int) -> None:
        rule = self._matching_rule("state_locked", batch_id)
        if rule is None:
            return
        self._record_fault("state_locked", batch_id)
        # Simulate another process holding the exclusive state lock by
        # attempting a second non-blocking acquisition.
        from .util import FileLock

        try:
            FileLock(self.state.lock_path).acquire()
        except OSError:
            raise StateError(
                "state backend is locked by another process",
                details={"injected": True, "batch": batch_id},
            )

    def _inject_corrupt(self, batch_id: int) -> None:
        rule = self._matching_rule("state_corrupt", batch_id)
        if rule is None:
            return
        self._record_fault("state_corrupt", batch_id)
        # Persist all progress so far, then damage the on-disk checkpoint.
        # The verification failure must terminate the job with E_STATE and
        # leave nothing a later job could treat as committed state.
        self.state.commit(
            batch_id, job_done=False, extra_faults=self.fault_log
        )
        target = self.state.shard_path(0)
        with open(target, "wb") as fh:
            fh.write(b"corrupted-bytes")
        from .state import loads_snapshot

        try:
            with open(target, "rb") as snapshot_fh:
                loads_snapshot(snapshot_fh.read())
        except StateError as exc:
            exc.details.update({"injected": True, "batch": batch_id})
            self.state._discard_checkpoint()
            raise

    # ------------------------------------------------------------- resources
    def _check_limits(self) -> None:
        rss = rss_bytes()
        if rss > self.peak_rss:
            self.peak_rss = rss
        if rss > self.cfg.memory_hard_limit_bytes:
            raise LimitError(
                "memory hard limit exceeded",
                details={"rss_bytes": rss, "limit": self.cfg.memory_hard_limit_bytes},
            )
        temp_used = directory_size(self.cfg.state_dir) + directory_size(
            self.cfg.spill_dir
        )
        if temp_used > self.cfg.temp_hard_limit_bytes:
            raise LimitError(
                "temp space hard limit exceeded",
                details={"bytes": temp_used, "limit": self.cfg.temp_hard_limit_bytes},
            )

    # ------------------------------------------------------------- processing
    def _apply_events(self, unit: int, events: list[dict], early_close: bool) -> dict:
        shard = self.state.shard(unit)
        applied = late = duplicates = 0
        wm = self.state.watermark.value
        size = self.cfg.window_size_ms
        with self.state.shard_lock(unit):
            for row in events:
                w_start = window_start(row["event_time_ms"], size)
                key = dedup_key(row["payment_id"], w_start, row["event_id"])
                if key in shard.seen or key in shard.late_seen:
                    duplicates += 1
                    continue
                closed = is_window_closed(wm, w_start, size)
                if early_close:
                    closed = True
                if closed:
                    shard.late_seen.add(key)
                    shard.late[w_start] = shard.late.get(w_start, 0) + 1
                    late += 1
                    continue
                shard.seen.add(key)
                agg = shard.windows.setdefault(w_start, [0, 0])
                agg[0] += 1
                agg[1] += int(row["amount_cents"])
                applied += 1
        return {"applied": applied, "late": late, "duplicates": duplicates}

    def _close_windows(self) -> None:
        """Move windows closed by the current watermark into the output set."""

        wm = self.state.watermark.value
        size = self.cfg.window_size_ms
        emitted = {row["window_start"] for row in self.state.output_rows}
        # Load every shard so closed windows can be merged exactly once.
        for shard in self.state.shards.values():
            if not shard.resident:
                self.state._load_shard(shard)
        merged: dict[int, list[int]] = {}
        for unit, shard in self.state.shards.items():
            with self.state.shard_lock(unit):
                ready = [
                    w
                    for w in list(shard.windows)
                    if is_window_closed(wm, w, size)
                ]
                for w in ready:
                    agg = shard.windows[w]
                    target = merged.setdefault(w, [0, 0])
                    target[0] += agg[0]
                    target[1] += agg[1]
                    del shard.windows[w]
        for w in sorted(merged):
            if w in emitted:
                continue
            self.state.output_rows.append(
                {
                    "window_start": w,
                    "event_count": merged[w][0],
                    "amount_cents": merged[w][1],
                    "late_count": 0,
                    "completed": True,
                }
            )
            for shard in self.state.shards.values():
                if shard.resident:
                    shard.completed[w] = True
        self.state.output_rows.sort(key=lambda r: r["window_start"])

    def process_unit(
        self, source_kind: str, source_id: int, rows: list[dict]
    ) -> None:
        """Consume one source unit (history shard or stream batch)."""

        if source_id in self.state.consumed_batches:
            return  # idempotent resume: never re-consume

        rule = self._matching_rule("batch_read_failure", source_id)
        if rule is not None:
            self._record_fault("batch_read_failure", source_id)
            raise InputError(
                f"injected read failure on {source_kind} {source_id}",
                details={"injected": True, "batch": source_id},
            )

        self._inject_state_locked(source_id)
        self._inject_corrupt(source_id)

        early_rule = self._matching_rule("early_watermark", source_id)
        early_close = early_rule is not None
        if early_rule is not None:
            self._record_fault("early_watermark", source_id)

        # Partition events deterministically by payment key.
        buckets: dict[int, list[dict]] = {
            unit: [] for unit in range(self.cfg.partitions)
        }
        for row in rows:
            unit = self.split_plan.unit_for(row["payment_id"], row["event_id"])
            buckets[unit].append(row)

        totals = {"applied": 0, "late": 0, "duplicates": 0}
        with ThreadPoolExecutor(max_workers=self.cfg.workers) as pool:
            futures = [
                pool.submit(self._apply_events, unit, events, early_close)
                for unit, events in buckets.items()
                if events
            ]
            for future in futures:
                result = future.result()
                for key in totals:
                    totals[key] += result[key]
        self.state.dedup_dropped += totals["duplicates"]

        # Watermark is driven by the source-declared event time.
        declared = max(row["event_time_ms"] for row in rows)
        self.state.watermark.observe(
            declared, force_lateness_zero=False
        )
        self._close_windows()
        self.state.maybe_spill()
        self._check_limits()

        kill_rule = self._matching_rule("kill_after_commit", source_id)
        if kill_rule is not None:
            self._record_fault("kill_after_commit", source_id)

        point = self.state.commit(
            source_id,
            job_done=False,
            extra_faults=self.fault_log,
        )
        self.commit_points.append(point)

        if kill_rule is not None:
            raise KillAfterCommit(f"injected external kill after batch {source_id}")

    def finalize(self) -> None:
        self.state.watermark.finalize()
        for shard in self.state.shards.values():
            if not shard.resident:
                self.state._load_shard(shard)
        self._close_windows()
        # Late events never modify emitted rows; windows with only late events
        # get a zero-amount row so the late counts stay auditable.
        emitted = {row["window_start"] for row in self.state.output_rows}
        late_totals: dict[int, int] = {}
        for shard in self.state.shards.values():
            for w, count in shard.late.items():
                late_totals[w] = late_totals.get(w, 0) + count
        for w, count in sorted(late_totals.items()):
            # Always a separate row: late events must never mutate an
            # already-emitted window result.
            self.state.output_rows.append(
                {
                    "window_start": w,
                    "event_count": 0,
                    "amount_cents": 0,
                    "late_count": count,
                    "completed": True,
                }
            )
            for shard in self.state.shards.values():
                shard.completed[w] = True
        self.state.output_rows.sort(key=lambda r: r["window_start"])
        last_batch = (
            max(self.state.consumed_batches)
            if self.state.consumed_batches
            else -1
        )
        self.state.commit(last_batch, job_done=True, extra_faults=self.fault_log)
        self._check_limits()

    # ------------------------------------------------------------- driver
    def run_mixed(self) -> dict:
        # History shards get negative batch ids; stream batches keep manifest ids.
        source_units: list[tuple[str, int, list[dict]]] = []
        for shard_index, shard in enumerate(self.manifest["history"]):
            source_id = -1 - shard_index
            source_units.append(
                ("history", source_id, read_parquet_shard(self.cfg.data_dir, shard))
            )
        for batch in self.manifest["stream_batches"]:
            source_units.append(
                (
                    "stream",
                    batch["batch_id"],
                    read_jsonl_batch(self.cfg.data_dir, batch),
                )
            )
        for kind, source_id, rows in source_units:
            self.process_unit(kind, source_id, rows)
        self.finalize()
        return self.build_report()

    def build_report(self) -> dict:
        report = {
            "mode": self.cfg.mode,
            "allowed_lateness_ms": self.cfg.allowed_lateness_ms,
            "window_size_ms": self.cfg.window_size_ms,
            "partitions": self.cfg.partitions,
            "workers": self.cfg.workers,
            "input": {
                "history_shards": len(self.manifest["history"]),
                "stream_batches": len(self.manifest["stream_batches"]),
                "history_rows": self.manifest["history_row_count"],
                "stream_rows": self.manifest["stream_row_count"],
            },
            "dedup_dropped": self.state.dedup_dropped,
            "late_events": sum(
                sum(shard.late.values())
                for shard in self.state.shards.values()
            ),
            "window_count": len(self.state.output_rows),
            "windows": self.state.output_rows,
            "split_plan": self.split_plan.to_dict(),
            "recovery": {
                "recoveries": self.state.recoveries,
                "recovery_commit_points": list(self.state.recovery_points),
                "commit_points": sorted(set(self.commit_points)),
                "consumed_batches": self.state.consumed_batches,
            },
            "spill_bytes": self.state.spill_bytes,
            "peak_rss_bytes": max(self.peak_rss, rss_bytes()),
            "faults": sorted(self.fault_log, key=lambda f: (f["batch"], f["fault"])),
        }
        return report
