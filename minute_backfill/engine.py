"""Top-level backfill orchestration."""

from __future__ import annotations

import os
import sqlite3
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import psutil

from .errors import EngineError, memory_error, state_error
from .inputs import InputCatalog, read_payment_shard, read_refund_batch
from .metrics import BackfillMetrics
from .parallel import ParallelAggregator
from .report import build_report, write_key_window_csv, write_report, write_window_csv
from .spill import SpillBuffer, TempWorkspace
from .state import RunContext, StateStore
from .timeutil import MICROS_PER_SECOND
from .util import stable_hash64, write_json


@dataclass
class EngineConfig:
    data_dir: Path
    state_db: Path
    out_dir: Path
    slots: int = 6
    workers: int = 2
    allowed_lateness_seconds: int = 60
    skew_threshold: float = 0.40
    spill_mb: float = 128.0
    memory_limit_mb: float = 900.0
    temp_dir: Path | None = None
    terminate_after_batch: int | None = None
    metrics_port: int | None = None
    reset_state: bool = False

    def validate(self) -> None:
        if self.slots < 1:
            raise ValueError("slots must be >= 1")
        if self.workers < 0:
            raise ValueError("workers must be >= 0")
        if self.allowed_lateness_seconds < 0:
            raise ValueError("allowed_lateness_seconds must be >= 0")
        if not 0 < self.skew_threshold <= 1:
            raise ValueError("skew_threshold must be in (0, 1]")
        if self.spill_mb < 0 or self.memory_limit_mb <= 0:
            raise ValueError("spill/memory thresholds must be positive")
        if self.terminate_after_batch is not None and self.terminate_after_batch < 1:
            raise ValueError("terminate_after_batch must be >= 1")


def utc_now_text() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


class BackfillEngine:
    def __init__(self, config: EngineConfig) -> None:
        config.validate()
        self.config = config
        self.config.slots = max(1, self.config.slots)
        # The task grants at most two logical CPUs.  Six logical partitions may
        # be processed by at most two OS worker processes.
        self.config.workers = max(0, min(2, self.config.workers))
        self.config.memory_limit_mb = min(1024.0, float(self.config.memory_limit_mb))
        self.out_dir = config.out_dir.resolve()
        self.out_dir.mkdir(parents=True, exist_ok=True)
        self.metrics = BackfillMetrics(
            slots=self.config.slots, workers=self.config.workers
        )
        if self.config.metrics_port is not None:
            self.metrics.start_http(self.config.metrics_port)
        self.catalog = InputCatalog.load(config.data_dir)
        self.state = StateStore(
            config.state_db,
            reset=config.reset_state,
        )
        self.state.validate_resume_inputs(self.catalog, self.catalog.schema_sha256)
        temp_base = config.temp_dir or (self.out_dir / ".tmp")
        state_identity = f"{config.state_db.resolve()}:{stable_hash64(str(config.state_db.resolve())):016x}"
        token = _safe_token(f"{config.state_db.resolve().stem}-{stable_hash64(state_identity):016x}")
        self.temp = TempWorkspace(temp_base, token)
        self.temp.cleanup_stale()
        self.spill = SpillBuffer(
            self.temp,
            threshold_bytes=max(1, int(float(config.spill_mb) * 1024 * 1024)),
        )
        self.aggregator = ParallelAggregator(
            workers=self.config.workers,
            slots=self.config.slots,
            skew_threshold=self.config.skew_threshold,
        )
        self.process = psutil.Process(os.getpid())
        self.peak_memory_bytes = self._rss_tree()
        self.run_context: RunContext | None = None
        self.started_at = utc_now_text()
        self._started_monotonic = time.monotonic()
        self._temp_cleaned = False

    # ------------------------------------------------------------------
    # public entry point
    # ------------------------------------------------------------------
    def run(self) -> dict[str, Any]:
        try:
            self.run_context = self.state.start_run()
            self._check_memory(0)
            consumed = self.state.consumed_input_ids()
            pending = any(
                item.shard_id not in consumed for item in self.catalog.shards
            ) or any(item.batch_id not in consumed for item in self.catalog.batches)
            if pending:
                # A previously completed job may receive appended batches.  Clear
                # the final-flush marker so newly opened windows are flushed again.
                self.state.meta_set("finalized", "0")
            self._process_history(consumed)
            self._process_batches(consumed)
            self._finalize_if_needed()
            self.state.finish_run(self.run_context.run_id, "complete")
            self.aggregator.shutdown()
            self._cleanup_temp()
            report = self._build_report(
                status="complete",
                error=None,
            )
            self._write_success(report)
            return report
        except EngineError as exc:
            self.state.rollback()
            self._finish_failed(exc)
            self.aggregator.shutdown()
            self._cleanup_temp()
            if exc.code != "E_STATE":
                self._try_write_partial(exc)
            else:
                self._write_error_only(exc)
            raise
        except KeyboardInterrupt:
            self.state.rollback()
            err = EngineError("E_INTERRUPTED", "job interrupted by operator")
            self._finish_failed(err)
            self.aggregator.shutdown()
            self._cleanup_temp()
            self._try_write_partial(err)
            raise
        except MemoryError as exc:
            self.state.rollback()
            err = memory_error("Python allocator exhausted; committed windows are preserved")
            self._finish_failed(err)
            self.aggregator.shutdown()
            self._cleanup_temp()
            self._try_write_partial(err)
            raise err from exc
        except sqlite3.Error as exc:
            self.state.rollback()
            try:
                self.state.quarantine(f"mid-run SQLite failure: {exc}")
            except Exception:
                pass
            err = state_error(f"state database failed during processing: {exc}")
            self._finish_failed(err)
            self.aggregator.shutdown()
            self._cleanup_temp()
            self._write_error_only(err)
            raise err from exc
        except Exception as exc:
            self.state.rollback()
            err = EngineError("E_INTERNAL", f"unexpected engine failure: {exc}")
            self._finish_failed(err)
            self.aggregator.shutdown()
            self._cleanup_temp()
            self._try_write_partial(err)
            raise err from exc
        finally:
            try:
                self.aggregator.shutdown()
            except Exception:
                pass
            self._cleanup_temp()
            try:
                self.state.close()
            except Exception:
                pass

    # ------------------------------------------------------------------
    # input units
    # ------------------------------------------------------------------
    def _process_history(self, consumed: set[str]) -> None:
        for shard in self.catalog.shards:
            if shard.shard_id in consumed:
                continue
            self._check_memory(0)
            self.state.begin()
            try:
                seq = self.state.next_commit_seq()
                watermark = self.state.watermark()
                arrival_base = int(self.state.meta_get("arrival_seq") or 0)
                affected: set[tuple[str, int]] = set()
                total_rows = 0
                for chunk in read_payment_shard(shard, self.catalog.manifest.payments):
                    self._check_memory(_estimate_events(chunk))
                    stats = self.state.ingest(
                        chunk,
                        arrival_seq_start=arrival_base + total_rows,
                        watermark_before=watermark,
                    )
                    self.state.finalize_ingest(stats)
                    affected |= stats.affected
                    self._metrics_for_stats(stats)
                    total_rows += len(chunk)
                self._aggregate_keys(affected, commit_seq=seq)
                self.state.meta_set("arrival_seq", arrival_base + total_rows)
                self.state.record_consumed(
                    seq=seq,
                    input_id=shard.shard_id,
                    kind="history_shard",
                    batch_index=None,
                    declared_event_time=shard.declared_event_time_us,
                    rows=total_rows,
                    sha256=shard.sha256,
                    watermark_after=watermark,
                    closed_minute_count=0,
                )
                self.state.commit()
            except Exception:
                self.state.rollback()
                raise
            consumed.add(shard.shard_id)
            self.metrics.events_total.labels(kind="payment_history").inc(total_rows)
            self.metrics.shards_total.inc()
            self._observe_state()

    def _process_batches(self, consumed: set[str]) -> None:
        lateness_us = int(self.config.allowed_lateness_seconds) * MICROS_PER_SECOND
        for batch in self.catalog.batches:
            if batch.batch_id in consumed:
                continue
            self._check_memory(0)
            events = read_refund_batch(batch, self.catalog.manifest.refunds)
            self._check_memory(_estimate_events(events))
            watermark_before = self.state.watermark()
            self.state.begin()
            emitted = 0
            try:
                seq = self.state.next_commit_seq()
                arrival_base = int(self.state.meta_get("arrival_seq") or 0)
                stats = self.state.ingest(
                    events,
                    arrival_seq_start=arrival_base,
                    watermark_before=watermark_before,
                )
                self.state.finalize_ingest(stats)
                self._metrics_for_stats(stats)
                self._aggregate_keys(stats.affected, commit_seq=seq)
                declared_watermark = batch.declared_event_time_us - lateness_us
                new_watermark = (
                    declared_watermark
                    if watermark_before is None
                    else max(watermark_before, declared_watermark)
                )
                before_minutes = self.state.committed_minute_count()
                self.state.close_windows(
                    watermark=new_watermark,
                    commit_seq=seq,
                    reason="watermark",
                )
                emitted = self.state.committed_minute_count() - before_minutes
                self.state.set_watermark(new_watermark)
                self.state.meta_set("arrival_seq", arrival_base + len(events))
                self.state.record_consumed(
                    seq=seq,
                    input_id=batch.batch_id,
                    kind="refund_batch",
                    batch_index=batch.batch_index,
                    declared_event_time=batch.declared_event_time_us,
                    rows=len(events),
                    sha256=batch.sha256,
                    watermark_after=new_watermark,
                    closed_minute_count=emitted,
                )
                self.state.commit()
            except Exception:
                self.state.rollback()
                raise
            consumed.add(batch.batch_id)
            self.metrics.events_total.labels(kind="refund_batch").inc(len(events))
            self.metrics.batches_total.inc()
            if emitted:
                self.metrics.windows_committed_total.inc(emitted)
            self._observe_state()
            if (
                self.config.terminate_after_batch is not None
                and batch.batch_index >= self.config.terminate_after_batch
            ):
                self._inject_termination(batch)

    def _finalize_if_needed(self) -> None:
        if self.state.meta_get("finalized") == "1":
            return
        self.state.begin()
        try:
            seq = self.state.next_commit_seq()
            before = self.state.committed_minute_count()
            self.state.close_windows(
                watermark=None,
                commit_seq=seq,
                reason="end_of_input",
            )
            emitted = self.state.committed_minute_count() - before
            self.state.meta_set("finalized", "1")
            self.state.meta_set("finalized_seq", seq)
            self.state.commit()
        except Exception:
            self.state.rollback()
            raise
        if emitted:
            self.metrics.windows_committed_total.inc(emitted)
        self._observe_state()

    # ------------------------------------------------------------------
    # aggregation / state
    # ------------------------------------------------------------------
    def _aggregate_keys(self, keys: set[tuple[str, int]], *, commit_seq: int) -> None:
        if not keys:
            return
        # Sorting removes Python hash-randomized set iteration from chunk
        # boundaries, keeping actual slot-load metrics reproducible.
        for groups in self.state.load_groups(sorted(keys)):
            self._aggregate_group_chunk(groups)

    def _aggregate_group_chunk(self, groups: list[dict[str, Any]]) -> None:
        self._check_memory(_estimate_groups(groups))
        result = self.aggregator.aggregate(groups, self.spill)
        for slot, load in enumerate(result.assignment.slot_loads):
            if load:
                self.state.counter_add(f"slot_events_{slot}", load)
        spill_written = self.spill.written_bytes
        self.state.apply_key_windows(self.spill.iter_rows())
        if spill_written:
            self.state.counter_add("spill_bytes", spill_written)
            self.metrics.spill_bytes_total.inc(spill_written)
        self.spill.clear_after_apply()
        self._observe_memory()

    def _metrics_for_stats(self, stats: Any) -> None:
        if stats.dedup_dropped:
            self.metrics.dedup_dropped_total.inc(stats.dedup_dropped)
        if stats.late_count:
            self.metrics.late_total.inc(stats.late_count)

    def _observe_state(self) -> None:
        self.metrics.observe_state(self.state)
        self._observe_memory()

    def _observe_memory(self) -> None:
        current = self._rss_tree()
        self.peak_memory_bytes = max(self.peak_memory_bytes, current)
        self.metrics.peak_memory_bytes.set(self.peak_memory_bytes)
        limit = int(self.config.memory_limit_mb * 1024 * 1024)
        if current > limit:
            raise memory_error(
                "observed process-tree RSS exceeded the configured memory limit",
                observed_rss_bytes=current,
                memory_limit_bytes=limit,
            )

    # ------------------------------------------------------------------
    # memory / temp / termination
    # ------------------------------------------------------------------
    def _rss_tree(self) -> int:
        try:
            total = int(self.process.memory_info().rss)
        except (psutil.Error, AttributeError):
            total = 0
        try:
            children = self.process.children(recursive=True)
        except psutil.Error:
            children = []
        for child in children:
            try:
                total += int(child.memory_info().rss)
            except psutil.Error:
                continue
        return total

    def _check_memory(self, estimated_extra: int) -> None:
        current = self._rss_tree()
        self.peak_memory_bytes = max(self.peak_memory_bytes, current + estimated_extra)
        limit = int(self.config.memory_limit_mb * 1024 * 1024)
        if current + estimated_extra > limit:
            raise memory_error(
                "configured memory limit exceeded; committed windows are preserved",
                observed_rss_bytes=current,
                estimated_extra_bytes=estimated_extra,
                memory_limit_bytes=limit,
            )

    def _cleanup_temp(self) -> None:
        try:
            self.spill.clear_after_apply()
            self.temp.cleanup()
            self._temp_cleaned = not self.temp.path.exists()
        except Exception:
            self._temp_cleaned = False

    def _inject_termination(self, batch: Any) -> None:
        # The committed transaction is already durable.  Stop idle workers
        # before the hard exit so their inherited stdio handles do not keep a
        # supervisor pipe open after the engine process disappears.
        try:
            self.aggregator.shutdown()
        except Exception:
            pass
        marker = self.out_dir / "termination_injected.json"
        try:
            write_json(
                marker,
                {
                    "reason": "external termination injection",
                    "after_batch_id": batch.batch_id,
                    "after_batch_index": batch.batch_index,
                    "state_committed": True,
                    "injected_at": utc_now_text(),
                },
            )
        except Exception:
            pass
        os._exit(137)

    # ------------------------------------------------------------------
    # reporting
    # ------------------------------------------------------------------
    def _build_report(self, *, status: str, error: dict[str, Any] | None) -> dict[str, Any]:
        if self.run_context is None:
            raise state_error("run context is unavailable")
        finished_at = utc_now_text()
        return build_report(
            state=self.state,
            catalog=self.catalog,
            config=self.config,
            run_context=self.run_context,
            started_at=self.started_at,
            finished_at=finished_at,
            duration_seconds=time.monotonic() - self._started_monotonic,
            peak_memory_bytes=self.peak_memory_bytes,
            status=status,
            error=error,
            temp_cleanup=self._temp_cleaned,
        )

    def _write_success(self, report: dict[str, Any]) -> None:
        for stale in (
            self.out_dir / "report.partial.json",
            self.out_dir / "windows.partial.csv",
            self.out_dir / "key_windows.partial.csv",
            self.out_dir / "last_error.json",
        ):
            try:
                stale.unlink()
            except FileNotFoundError:
                pass
        write_report(self.out_dir, report)
        write_window_csv(self.out_dir, report)
        write_key_window_csv(self.out_dir, self.state)
        self.metrics.peak_memory_bytes.set(self.peak_memory_bytes)
        self.metrics.write(self.out_dir / "metrics.prom")

    def _try_write_partial(self, exc: EngineError) -> None:
        try:
            report = self._build_report(status="failed", error=exc.to_dict())
            write_report(self.out_dir, report, partial=True)
            write_window_csv(self.out_dir, report, partial=True)
            write_key_window_csv(self.out_dir, self.state, partial=True)
            self.metrics.write(self.out_dir / "metrics.prom")
        except Exception:
            self._write_error_only(exc)

    def _write_error_only(self, exc: EngineError) -> None:
        try:
            write_json(
                self.out_dir / "last_error.json",
                {
                    "status": "failed",
                    "error": exc.to_dict(),
                    "at": utc_now_text(),
                },
            )
        except Exception:
            pass

    def _finish_failed(self, exc: EngineError) -> None:
        if self.run_context is None:
            return
        try:
            self.state.finish_run(
                self.run_context.run_id, "failed", exc.code
            )
        except Exception:
            pass
        self._write_error_only(exc)


def _estimate_events(events: list[dict[str, Any]]) -> int:
    return 120 + len(events) * 260


def _estimate_groups(groups: list[dict[str, Any]]) -> int:
    total = 64
    for group in groups:
        total += 220 + len(group.get("events", [])) * 220
    return total


def _safe_token(value: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "-_" else "_" for ch in value)[:48] or "state"
