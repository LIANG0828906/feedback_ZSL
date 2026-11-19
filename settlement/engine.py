from __future__ import annotations

import hashlib
import json
import time
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

from .errors import (
    ExecutionStopped,
    MemoryLimitError,
    PlanError,
    SettlementError,
    StateError,
)
from .loader import (
    DatasetManifest,
    iter_batches,
    load_manifest,
    read_batch,
    read_telemetry,
)
from .memory import MemoryGuard, MemorySnapshot, deterministic_memory_estimate
from .routing import RoutingPlan, build_routing
from .spill import SpillManager
from .sql import SettlementQuery, parse_sql
from .state import StateStore
from .timeutil import (
    MICROS_PER_SECOND,
    energy_per_100km,
    iso_us,
    number_from_fixed,
    us_to_datetime,
)
from .util import canonical_json, sha256_text


DEFAULT_MEMORY_LIMIT = 1024 * 1024 * 1024
DEFAULT_SPILL_THRESHOLD = 64 * 1024 * 1024
DEFAULT_TEMP_LIMIT = 2 * 1024 * 1024 * 1024


@dataclass(frozen=True)
class ExecutionConfig:
    sql: str | Path | None = None
    data_dir: str | Path = "data"
    db_path: str | Path = "settlement.db"
    mode: str = "pushdown"
    slots: int = 5
    cpu_workers: int = 2
    allowed_lateness_seconds: int = 180
    skew_threshold_ratio: float = 0.08
    memory_limit_bytes: int = DEFAULT_MEMORY_LIMIT
    spill_threshold_bytes: int = DEFAULT_SPILL_THRESHOLD
    temp_limit_bytes: int = DEFAULT_TEMP_LIMIT
    temp_dir: str | Path = ".settlement_tmp"
    stop_after_batch: int | None = None
    fail_batch: int | None = None
    reset: bool = False
    report_out: str | Path | None = None

    @property
    def effective_slots(self) -> int:
        # The 40% per-slot bound cannot be met with fewer than three shards.
        return max(3, int(self.slots))

    @property
    def effective_workers(self) -> int:
        return max(1, min(int(self.cpu_workers), 2, self.effective_slots))


@dataclass
class ExecutionResult:
    status: str
    run_id: str
    plan_mode: str
    report: dict[str, Any]
    plan: dict[str, Any]
    verified: bool | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "run_id": self.run_id,
            "plan_mode": self.plan_mode,
            "plan": self.plan,
            "report": self.report,
            "verified": self.verified,
        }


@dataclass
class BatchOutcome:
    batch_id: int
    rows_read: int
    accepted: int
    dedup_discards: int
    late_events: int
    closed_windows: int
    watermark_us: int


@dataclass
class _RunContext:
    run_id: str
    mode: str
    query: SettlementQuery
    manifest: DatasetManifest
    config: ExecutionConfig
    memory: MemoryGuard
    spill: SpillManager
    started_at: str
    start_monotonic: float
    routing: RoutingPlan | None = None
    trip_keys: set[tuple[str, str]] = field(default_factory=set)
    peak_spill_bytes: int = 0


class Executor:
    """Resumable local settlement executor.

    The public entry point accepts the same inputs as the CLI and may be used
    from tests without starting a server or external database.
    """

    def __init__(
        self,
        config: ExecutionConfig | None = None,
        *,
        sql_text: str | None = None,
    ):
        self.config = config or ExecutionConfig()
        self.sql_text = sql_text
        if self.config.mode not in {"pushdown", "replay", "both"}:
            raise PlanError("mode must be one of pushdown, replay or both")
        if self.config.effective_slots < 3:
            raise PlanError("at least three logical slots are required")
        if self.config.allowed_lateness_seconds < 0:
            raise PlanError("allowed_lateness_seconds must be non-negative")
        if not (0 < self.config.skew_threshold_ratio <= 1):
            raise PlanError("skew_threshold_ratio must be in (0, 1]")
        if self.config.memory_limit_bytes <= 0:
            raise PlanError("memory_limit_bytes must be positive")
        if self.config.temp_limit_bytes <= 0:
            raise PlanError("temp_limit_bytes must be positive")

    def plan(self) -> dict[str, Any]:
        query = parse_sql(self._read_sql())
        manifest = load_manifest(self.config.data_dir)
        summary = query.plan_summary()
        summary["dataset"] = {
            "path": str(manifest.path),
            "sha256": manifest.sha256,
            "telemetry_rows": manifest.telemetry_rows,
            "telemetry_partitions": len(manifest.telemetry_partitions),
            "supplement_rows": manifest.supplement_rows,
            "supplement_batches": manifest.batch_count,
        }
        summary["execution"] = {
            "requested_slots": int(self.config.slots),
            "effective_slots": self.config.effective_slots,
            "cpu_workers": self.config.effective_workers,
            "allowed_lateness_seconds": int(self.config.allowed_lateness_seconds),
            "skew_threshold_ratio": self.config.skew_threshold_ratio,
        }
        return summary

    def run(self) -> list[ExecutionResult]:
        query = parse_sql(self._read_sql())
        manifest = load_manifest(self.config.data_dir)
        modes = ["pushdown", "replay"] if self.config.mode == "both" else [self.config.mode]
        store = StateStore(self.config.db_path)
        results: list[ExecutionResult] = []
        verify_payload: dict[str, Any] | None = None
        try:
            for mode in modes:
                result, verification = self._run_mode(store, mode, query, manifest)
                results.append(result)
                if verification:
                    verify_payload = verification
            if len(results) == 2:
                from .verify import compare_plan_results

                push_result = next(item for item in results if item.plan_mode == "pushdown")
                replay_result = next(item for item in results if item.plan_mode == "replay")
                verify_payload = compare_plan_results(
                    store,
                    push_run_id=push_result.run_id,
                    replay_run_id=replay_result.run_id,
                )
                for item in results:
                    item.verified = bool(verify_payload["equivalent"])
                    item.report = self._refresh_report(
                        store, item.run_id, query, manifest, verify_payload
                    )
            self._write_result_report(results)
            return results
        finally:
            store.close()

    def _read_sql(self) -> str:
        if self.sql_text is not None:
            return self.sql_text
        if self.config.sql is None:
            raise PlanError("an SQL file or sql_text is required")
        path = Path(self.config.sql)
        try:
            return path.read_text(encoding="utf-8")
        except OSError as exc:
            raise PlanError(f"could not read settlement SQL: {exc}", details={"path": str(path)}) from exc

    def _run_mode(
        self,
        store: StateStore,
        mode: str,
        query: SettlementQuery,
        manifest: DatasetManifest,
    ) -> tuple[ExecutionResult, dict[str, Any] | None]:
        run_id = self._run_id(query, manifest, mode)
        started_at = _utc_now()
        started_monotonic = time.monotonic()
        existing = store.fetchone("SELECT * FROM job_runs WHERE run_id = ?", (run_id,))
        if existing and existing["status"] == "published" and not self.config.reset:
            report = self._load_or_build_report(store, run_id, query, manifest)
            return (
                ExecutionResult(
                    status="already_published",
                    run_id=run_id,
                    plan_mode=mode,
                    report=report,
                    plan=query.plan_summary(),
                    verified=report.get("verification", {}).get("equivalent"),
                ),
                report.get("verification"),
            )
        if self.config.reset and existing:
            self._delete_run(store, run_id)
            existing = None

        memory = MemoryGuard(self.config.memory_limit_bytes)
        spill = SpillManager(
            self.config.temp_dir,
            threshold_bytes=self.config.spill_threshold_bytes,
            memory_limit_bytes=self.config.memory_limit_bytes,
            temp_limit_bytes=self.config.temp_limit_bytes,
        )
        context = _RunContext(
            run_id=run_id,
            mode=mode,
            query=query,
            manifest=manifest,
            config=self.config,
            memory=memory,
            spill=spill,
            started_at=started_at,
            start_monotonic=started_monotonic,
        )
        try:
            memory.start()
            memory.check("startup")
            if existing:
                self._prepare_resume(store, context, existing)
            else:
                existing = store.fetchone("SELECT * FROM job_runs WHERE run_id = ?", (run_id,))
            if not existing or not bool(existing["telemetry_loaded"]):
                self._initialize_telemetry(store, context)

            self._load_routing_and_keys(store, context)
            self._run_batches(store, context)
            self._finalize(store, context)
            self._publish(store, context)
            memory.check("after_publish")
            report = self._build_report(store, context)
            self._store_report(store, context, report)
            return (
                ExecutionResult(
                    status="published",
                    run_id=run_id,
                    plan_mode=mode,
                    report=report,
                    plan=query.plan_summary(),
                    verified=report.get("verification", {}).get("equivalent"),
                ),
                report.get("verification"),
            )
        except ExecutionStopped:
            self._mark_run(store, run_id, "stopped", "E_STOPPED")
            report = self._build_report(store, context, include_verification=False)
            self._store_report(store, context, report)
            raise
        except MemoryLimitError as exc:
            self._mark_run(store, run_id, "failed", "E_MEMORY")
            exc.details.setdefault("run_id", run_id)
            exc.details.setdefault("committed_batch", self._consumed_batch(store, run_id))
            raise
        except SettlementError as exc:
            self._mark_run(store, run_id, "failed", exc.code)
            raise
        except Exception as exc:
            self._mark_run(store, run_id, "failed", "E_INTERNAL")
            raise SettlementError("E_INTERNAL", f"unexpected executor failure: {exc}") from exc
        finally:
            snapshot = memory.snapshot()
            self._record_runtime_meta(store, context, snapshot)
            memory.stop()
            spill.cleanup()

    def _run_id(self, query: SettlementQuery, manifest: DatasetManifest, mode: str) -> str:
        payload = {
            "version": 1,
            "sql_sha256": query.sql_sha256,
            "dataset_sha256": manifest.sha256,
            "mode": mode,
            "slots": self.config.effective_slots,
            "allowed_lateness_seconds": int(self.config.allowed_lateness_seconds),
            "skew_threshold_ratio": float(self.config.skew_threshold_ratio),
            "dedup": "source_type+vehicle_id+trip_id+window_start+source_id_or_business_fields",
            "numeric_scale": 1_000_000,
        }
        return sha256_text(canonical_json(payload))[:32]

    def _prepare_resume(self, store: StateStore, context: _RunContext, row: Any) -> None:
        now = _utc_now()
        with store.tx() as conn:
            resume_seq = int(
                conn.execute(
                    "SELECT COALESCE(MAX(resume_seq), 0) + 1 FROM resume_events WHERE run_id = ?",
                    (context.run_id,),
                ).fetchone()[0]
            )
            conn.execute(
                "INSERT INTO resume_events(run_id,resume_seq,commit_seq,consumed_batch,resume_at) "
                "VALUES (?,?,?,?,?)",
                (
                    context.run_id,
                    resume_seq,
                    int(row["commit_seq"]),
                    int(row["consumed_batch"]),
                    now,
                ),
            )
            conn.execute(
                "UPDATE job_runs SET status='running', error_code=NULL, updated_at=?, "
                "resume_count=resume_count+1 WHERE run_id=?",
                (now, context.run_id),
            )
            conn.execute(
                "UPDATE batch_attempts SET status='interrupted', error_code='E_INTERRUPTED', "
                "finished_at=? WHERE run_id=? AND status='running'",
                (now, context.run_id),
            )
        context.peak_spill_bytes = int(
            store.fetchone(
                "SELECT COALESCE(SUM(bytes_written), 0) AS total FROM spill_events WHERE run_id=?",
                (context.run_id,),
            )["total"]
        )

    def _initialize_telemetry(self, store: StateStore, context: _RunContext) -> None:
        rows = read_telemetry(
            context.manifest,
            context.query,
            pushdown=context.mode == "pushdown",
        )
        filtered = [event for event in rows if context.query.matches(event)]
        del rows
        context.memory.check("after_telemetry_scan")

        seen: set[str] = set()
        unique: list[dict[str, Any]] = []
        duplicate_issues: list[tuple[str, dict[str, Any]]] = []
        for event in filtered:
            key = _dedup_key(event)
            if key in seen:
                duplicate_issues.append((key, event))
                continue
            seen.add(key)
            unique.append(event)
        del filtered

        routing = build_routing(
            unique,
            slots=context.config.effective_slots,
            skew_threshold_ratio=context.config.skew_threshold_ratio,
        )
        context.routing = routing
        trip_keys = {(event["vehicle_id"], event["trip_id"]) for event in unique}
        context.trip_keys = trip_keys

        state_groups = _group_events(
            unique,
            routing,
            workers=context.config.effective_workers,
        )
        slot_counts: Counter[int] = Counter()
        for event in unique:
            slot_counts[routing.route(event)] += 1
        now = _utc_now()
        with store.tx() as conn:
            self._delete_run_tables(conn, context.run_id)
            conn.execute(
                "INSERT INTO job_runs("
                "run_id,signature,sql_text,sql_sha256,dataset_sha256,plan_mode,status,slots,"
                "cpu_workers,allowed_lateness_seconds,skew_threshold_ratio,memory_limit_bytes,"
                "spill_threshold_bytes,created_at,updated_at,consumed_batch,commit_seq,telemetry_loaded"
                ") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)",
                (
                    context.run_id,
                    self._run_id(context.query, context.manifest, context.mode),
                    context.query.sql,
                    context.query.sql_sha256,
                    context.manifest.sha256,
                    context.mode,
                    "running",
                    context.config.effective_slots,
                    context.config.effective_workers,
                    int(context.config.allowed_lateness_seconds),
                    float(context.config.skew_threshold_ratio),
                    int(context.config.memory_limit_bytes),
                    int(context.config.spill_threshold_bytes),
                    now,
                    now,
                    0,
                    1,
                ),
            )
            for key, event in duplicate_issues:
                self._insert_issue(
                    conn, context, "duplicate_telemetry", event, detail="duplicate telemetry event"
                )
            for event in unique:
                conn.execute(
                    "INSERT INTO dedup_seen(run_id,dedup_key,source_type,batch_id) VALUES (?,?,?,NULL)",
                    (context.run_id, _dedup_key(event), event["source_type"]),
                )
            for vehicle, trip in sorted(trip_keys):
                conn.execute(
                    "INSERT INTO trip_keys(run_id,vehicle_id,trip_id) VALUES (?,?,?)",
                    (context.run_id, vehicle, trip),
                )
            for vehicle, slot in sorted(routing.vehicle_slots.items()):
                conn.execute(
                    "INSERT INTO vehicle_route(run_id,vehicle_id,slot,split) VALUES (?,?,?,0)",
                    (context.run_id, vehicle, slot),
                )
            for (vehicle, trip, window_us), slot in sorted(routing.unit_slots.items()):
                conn.execute(
                    "INSERT INTO unit_route(run_id,vehicle_id,trip_id,window_start_us,slot) "
                    "VALUES (?,?,?,?,?)",
                    (context.run_id, vehicle, trip, window_us, slot),
                )
            for vehicle, count in sorted(routing.vehicle_counts.items()):
                split = vehicle in routing.split_vehicles
                slots = sorted(set(routing.split_vehicles.get(vehicle, [])))
                share = count / max(1, len(unique))
                unit_count = sum(1 for key in routing.unit_slots if key[0] == vehicle)
                conn.execute(
                    "INSERT INTO vehicle_load(run_id,vehicle_id,telemetry_count,share,split,"
                    "slots_json,unit_count) VALUES (?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        vehicle,
                        count,
                        share,
                        1 if split else 0,
                        canonical_json(slots),
                        unit_count,
                    ),
                )
            for slot, count in sorted(slot_counts.items()):
                conn.execute(
                    "INSERT INTO slot_stats(run_id,slot,event_count,telemetry_count,supplement_count) "
                    "VALUES (?,?,?,?,0)",
                    (context.run_id, slot, count, count),
                )
            for group in state_groups:
                _upsert_state(conn, context.run_id, group)
            conn.execute(
                "UPDATE job_runs SET telemetry_loaded=1, updated_at=?, commit_seq=1 WHERE run_id=?",
                (now, context.run_id),
            )
            conn.execute(
                "INSERT INTO checkpoints(run_id,commit_seq,batch_id,consumed_batch,watermark_us,"
                "accepted_events,late_events,dedup_discards,closed_windows,committed_at) "
                "VALUES (?,?,?,?,?,?,?,?,?,?)",
                (
                    context.run_id,
                    1,
                    0,
                    0,
                    None,
                    len(unique),
                    0,
                    len(duplicate_issues),
                    0,
                    now,
                ),
            )
        context.memory.check("after_telemetry_commit")

    def _load_routing_and_keys(self, store: StateStore, context: _RunContext) -> None:
        if context.routing is not None:
            return
        routing = RoutingPlan(
            slots=context.config.effective_slots,
            skew_threshold_ratio=context.config.skew_threshold_ratio,
        )
        for row in store.fetchall(
            "SELECT vehicle_id, slot, split FROM vehicle_route WHERE run_id=?",
            (context.run_id,),
        ):
            if int(row["split"]):
                routing.split_vehicles.setdefault(row["vehicle_id"], [])
            else:
                routing.vehicle_slots[row["vehicle_id"]] = int(row["slot"])
        for row in store.fetchall(
            "SELECT vehicle_id, trip_id, window_start_us, slot FROM unit_route WHERE run_id=?",
            (context.run_id,),
        ):
            key = (row["vehicle_id"], row["trip_id"], int(row["window_start_us"]))
            routing.unit_slots[key] = int(row["slot"])
            routing.split_vehicles.setdefault(row["vehicle_id"], []).append(int(row["slot"]))
        for vehicle in routing.split_vehicles:
            routing.split_vehicles[vehicle] = sorted(set(routing.split_vehicles[vehicle]))
        for row in store.fetchall(
            "SELECT vehicle_id, telemetry_count FROM vehicle_load WHERE run_id=?",
            (context.run_id,),
        ):
            routing.vehicle_counts[row["vehicle_id"]] = int(row["telemetry_count"])
        context.routing = routing
        context.trip_keys = {
            (row["vehicle_id"], row["trip_id"])
            for row in store.fetchall(
                "SELECT vehicle_id, trip_id FROM trip_keys WHERE run_id=?",
                (context.run_id,),
            )
        }

    def _run_batches(self, store: StateStore, context: _RunContext) -> None:
        run = store.fetchone("SELECT * FROM job_runs WHERE run_id=?", (context.run_id,))
        consumed = int(run["consumed_batch"])
        total_batches = context.manifest.batch_count
        for batch in iter_batches(context.manifest):
            batch_id = int(batch["batch_id"])
            if batch_id <= consumed:
                continue
            if batch_id != consumed + 1:
                raise StateError(
                    "batch sequence gap; cannot continue without a contiguous committed prefix",
                    details={"committed": consumed, "next_file_batch": batch_id},
                )
            outcome = self._process_batch(store, context, batch)
            consumed = batch_id
            context.memory.check(f"after_batch_{batch_id}")
            if (
                context.config.stop_after_batch == batch_id
                and self._claim_stop(store, batch_id)
            ):
                raise ExecutionStopped(
                    "execution stopped after committed batch",
                    details={
                        "run_id": context.run_id,
                        "committed_batch": batch_id,
                        "commit_seq": self._commit_seq(store, context.run_id),
                    },
                )
        if consumed < total_batches:
            raise StateError(
                "supplement batches ended before the manifest batch count",
                details={"consumed": consumed, "expected": total_batches},
            )

    def _process_batch(
        self,
        store: StateStore,
        context: _RunContext,
        batch: dict[str, Any],
    ) -> BatchOutcome:
        batch_id = int(batch["batch_id"])
        attempt_no = self._next_attempt_no(store, context.run_id, batch_id)
        started_at = _utc_now()
        with store.tx() as conn:
            conn.execute(
                "INSERT INTO batch_attempts(run_id,plan_mode,batch_id,attempt_no,status,started_at) "
                "VALUES (?,?,?,?,?,?)",
                (context.run_id, context.mode, batch_id, attempt_no, "running", started_at),
            )
        inject_failure = self._claim_failure_injection(store, batch_id, attempt_no)
        try:
            events, batch_meta = read_batch(context.manifest, batch, context.query)
            filtered = [event for event in events if context.query.matches(event)]
            del events
            spill_result = context.spill.maybe_spill(filtered, batch_id=batch_id)
            filtered_iter = spill_result.records
            if spill_result.bytes_written:
                context.peak_spill_bytes += spill_result.bytes_written
            run_row = store.fetchone("SELECT * FROM job_runs WHERE run_id=?", (context.run_id,))
            previous_watermark = (
                int(run_row["watermark_us"]) if run_row["watermark_us"] is not None else None
            )
            candidate_watermark = int(batch_meta["available_at_us"]) - (
                int(context.config.allowed_lateness_seconds) * MICROS_PER_SECOND
            )
            new_watermark = (
                candidate_watermark
                if previous_watermark is None
                else max(previous_watermark, candidate_watermark)
            )
            accepted = 0
            dedup_discards = 0
            late_events = 0
            closed_windows = 0
            accepted_events: list[dict[str, Any]] = []
            stats: Counter[int] = Counter()
            now = _utc_now()
            with store.tx() as conn:
                for event in filtered_iter:
                    assert context.routing is not None
                    slot = context.routing.route(event)
                    event["_processing_slot"] = slot
                    stats[slot] += 1
                    key = _dedup_key(event)
                    cursor = conn.execute(
                        "INSERT OR IGNORE INTO dedup_seen(run_id,dedup_key,source_type,batch_id) "
                        "VALUES (?,?,?,?)",
                        (context.run_id, key, event["source_type"], batch_id),
                    )
                    if cursor.rowcount == 0:
                        dedup_discards += 1
                        self._insert_issue(
                            conn,
                            context,
                            "duplicate_supplement",
                            event,
                            detail="duplicate supplement retained by deterministic first-wins key",
                        )
                        continue
                    trip_key = (event["vehicle_id"], event["trip_id"])
                    if trip_key not in context.trip_keys:
                        self._insert_issue(
                            conn,
                            context,
                            "orphan_supplement",
                            event,
                            detail="no matching telemetry vehicle_id/trip_id",
                        )
                        continue
                    if event["event_time_us"] < new_watermark:
                        late_events += 1
                        self._insert_issue(
                            conn,
                            context,
                            "late_event",
                            event,
                            detail=(
                                "event_time is before the arrival watermark; "
                                "event-time late predicates are retained, never scan-pushed"
                            ),
                        )
                        continue
                    accepted_events.append(event)
                    accepted += 1

                assert context.routing is not None
                for group in _group_events(
                    accepted_events,
                    context.routing,
                    workers=context.config.effective_workers,
                ):
                    _upsert_state(conn, context.run_id, group)

                for slot, count in sorted(stats.items()):
                    conn.execute(
                        "INSERT INTO slot_stats(run_id,slot,event_count,telemetry_count,supplement_count) "
                        "VALUES (?,?,?,0,?) "
                        "ON CONFLICT(run_id,slot) DO UPDATE SET "
                        "event_count=event_count+excluded.event_count, "
                        "supplement_count=supplement_count+excluded.supplement_count",
                        (context.run_id, slot, count, count),
                    )
                if spill_result.bytes_written:
                    seq = int(
                        conn.execute(
                            "SELECT COALESCE(MAX(sequence_no),0)+1 FROM spill_events WHERE run_id=?",
                            (context.run_id,),
                        ).fetchone()[0]
                    )
                    conn.execute(
                        "INSERT INTO spill_events(run_id,sequence_no,batch_id,bytes_written,reason) "
                        "VALUES (?,?,?,?,?)",
                        (
                            context.run_id,
                            seq,
                            batch_id,
                            spill_result.bytes_written,
                            "batch buffer exceeded configured in-memory threshold",
                        ),
                    )
                if inject_failure:
                    raise SettlementError(
                        "E_INJECTED_BATCH_FAILURE",
                        "injected batch failure before commit",
                        details={"batch_id": batch_id, "attempt_no": attempt_no},
                    )
                closed_windows = self._close_windows(
                    conn,
                    context,
                    cutoff_us=new_watermark,
                    now=now,
                )
                commit_seq = int(run_row["commit_seq"]) + 1
                conn.execute(
                    "UPDATE job_runs SET consumed_batch=?, watermark_us=?, commit_seq=?, "
                    "status='running', updated_at=? WHERE run_id=?",
                    (batch_id, new_watermark, commit_seq, now, context.run_id),
                )
                conn.execute(
                    "INSERT INTO checkpoints(run_id,commit_seq,batch_id,consumed_batch,watermark_us,"
                    "accepted_events,late_events,dedup_discards,closed_windows,committed_at) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        commit_seq,
                        batch_id,
                        batch_id,
                        new_watermark,
                        accepted,
                        late_events,
                        dedup_discards,
                        closed_windows,
                        now,
                    ),
                )
            finished_at = _utc_now()
            with store.tx() as conn:
                conn.execute(
                    "UPDATE batch_attempts SET status='success', rows_read=?, accepted_events=?, "
                    "dedup_discards=?, late_events=?, watermark_us=?, finished_at=? "
                    "WHERE run_id=? AND batch_id=? AND attempt_no=?",
                    (
                        batch_meta["rows"],
                        accepted,
                        dedup_discards,
                        late_events,
                        new_watermark,
                        finished_at,
                        context.run_id,
                        batch_id,
                        attempt_no,
                    ),
                )
            del filtered
            return BatchOutcome(
                batch_id=batch_id,
                rows_read=batch_meta["rows"],
                accepted=accepted,
                dedup_discards=dedup_discards,
                late_events=late_events,
                closed_windows=closed_windows,
                watermark_us=new_watermark,
            )
        except MemoryLimitError:
            self._finish_attempt_failure(store, context, batch_id, attempt_no, "E_MEMORY")
            raise
        except SettlementError as exc:
            self._finish_attempt_failure(store, context, batch_id, attempt_no, exc.code)
            raise
        except Exception as exc:
            self._finish_attempt_failure(store, context, batch_id, attempt_no, "E_INTERNAL")
            raise SettlementError(
                "E_INTERNAL",
                f"unexpected failure while processing batch {batch_id}: {exc}",
                details={"batch_id": batch_id},
            ) from exc

    def _claim_failure_injection(
        self, store: StateStore, batch_id: int, attempt_no: int
    ) -> bool:
        if self.config.fail_batch != batch_id or attempt_no != 1:
            return False
        key = f"fault_injected:{batch_id}"
        try:
            with store.tx() as conn:
                cursor = conn.execute(
                    "INSERT OR IGNORE INTO job_meta(key,value) VALUES (?,?)",
                    (key, _utc_now()),
                )
                return cursor.rowcount == 1
        except Exception:
            return False

    def _claim_stop(self, store: StateStore, batch_id: int) -> bool:
        key = f"stop_after_batch_claimed:{batch_id}"
        try:
            with store.tx() as conn:
                cursor = conn.execute(
                    "INSERT OR IGNORE INTO job_meta(key,value) VALUES (?,?)",
                    (key, _utc_now()),
                )
                return cursor.rowcount == 1
        except Exception:
            return False

    def _next_attempt_no(self, store: StateStore, run_id: str, batch_id: int) -> int:
        row = store.fetchone(
            "SELECT COALESCE(MAX(attempt_no),0)+1 AS next_no FROM batch_attempts "
            "WHERE run_id=? AND batch_id=?",
            (run_id, batch_id),
        )
        return int(row["next_no"])

    def _finish_attempt_failure(
        self,
        store: StateStore,
        context: _RunContext,
        batch_id: int,
        attempt_no: int,
        code: str,
    ) -> None:
        try:
            with store.tx() as conn:
                conn.execute(
                    "UPDATE batch_attempts SET status='failed', error_code=?, finished_at=? "
                    "WHERE run_id=? AND batch_id=? AND attempt_no=?",
                    (code, _utc_now(), context.run_id, batch_id, attempt_no),
                )
                conn.execute(
                    "UPDATE job_runs SET status='failed', error_code=?, updated_at=? WHERE run_id=?",
                    (code, _utc_now(), context.run_id),
                )
        except Exception:
            pass

    def _close_windows(
        self,
        conn: Any,
        context: _RunContext,
        *,
        cutoff_us: int,
        now: str,
    ) -> int:
        windows = conn.execute(
            "SELECT DISTINCT window_start_us, window_end_us FROM window_state "
            "WHERE run_id=? AND window_end_us<=? ORDER BY window_start_us",
            (context.run_id, cutoff_us),
        ).fetchall()
        for window in windows:
            start_us = int(window["window_start_us"])
            end_us = int(window["window_end_us"])
            rows = conn.execute(
                "SELECT vehicle_id, SUM(mileage_u) AS mileage_u, SUM(energy_u) AS energy_u, "
                "SUM(event_count) AS event_count, "
                "SUM(CASE WHEN source_type='telemetry' THEN event_count ELSE 0 END) AS telemetry_count, "
                "SUM(CASE WHEN source_type='charging_supplement' THEN event_count ELSE 0 END) "
                "AS supplement_count "
                "FROM window_state WHERE run_id=? AND window_start_us=? GROUP BY vehicle_id "
                "ORDER BY vehicle_id",
                (context.run_id, start_us),
            ).fetchall()
            for row in rows:
                conn.execute(
                    "INSERT INTO stage_vehicle_energy(run_id,plan_mode,vehicle_id,window_start_us,"
                    "mileage_u,energy_u,event_count,telemetry_count,supplement_count) "
                    "VALUES (?,?,?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        context.mode,
                        row["vehicle_id"],
                        start_us,
                        int(row["mileage_u"]),
                        int(row["energy_u"]),
                        int(row["event_count"]),
                        int(row["telemetry_count"]),
                        int(row["supplement_count"]),
                    ),
                )
            vehicle_count = len(rows)
            event_count = sum(int(row["event_count"]) for row in rows)
            conn.execute(
                "INSERT INTO stage_window_manifest(run_id,window_start_us,window_end_us,"
                "vehicle_count,event_count,committed_at) VALUES (?,?,?,?,?,?)",
                (context.run_id, start_us, end_us, vehicle_count, event_count, now),
            )
            conn.execute(
                "INSERT INTO closed_windows(run_id,window_start_us,window_end_us,closed_at) "
                "VALUES (?,?,?,?)",
                (context.run_id, start_us, end_us, now),
            )
            conn.execute(
                "DELETE FROM window_state WHERE run_id=? AND window_start_us=?",
                (context.run_id, start_us),
            )
        return len(windows)

    def _finalize(self, store: StateStore, context: _RunContext) -> None:
        state_count = int(
            store.fetchone(
                "SELECT COUNT(*) AS count FROM window_state WHERE run_id=?",
                (context.run_id,),
            )["count"]
        )
        if state_count == 0:
            return
        now = _utc_now()
        with store.tx() as conn:
            closed = self._close_windows(conn, context, cutoff_us=2**63 - 1, now=now)
            job = conn.execute(
                "SELECT commit_seq,watermark_us FROM job_runs WHERE run_id=?",
                (context.run_id,),
            ).fetchone()
            commit_seq = int(job["commit_seq"]) + 1
            conn.execute(
                "UPDATE job_runs SET commit_seq=?, updated_at=? WHERE run_id=?",
                (commit_seq, now, context.run_id),
            )
            conn.execute(
                "INSERT INTO checkpoints(run_id,commit_seq,batch_id,consumed_batch,watermark_us,"
                "accepted_events,late_events,dedup_discards,closed_windows,committed_at) "
                "VALUES (?,?,?,?,?,0,0,0,?,?)",
                (
                    context.run_id,
                    commit_seq,
                    context.manifest.batch_count,
                    context.manifest.batch_count,
                    job["watermark_us"],
                    closed,
                    now,
                ),
            )

    def _publish(self, store: StateStore, context: _RunContext) -> None:
        run = store.fetchone("SELECT * FROM job_runs WHERE run_id=?", (context.run_id,))
        if run["status"] == "published":
            return
        remaining = int(
            store.fetchone(
                "SELECT COUNT(*) AS count FROM window_state WHERE run_id=?",
                (context.run_id,),
            )["count"]
        )
        if remaining:
            raise StateError("cannot publish while window state is still open")
        if int(run["consumed_batch"]) != context.manifest.batch_count:
            raise StateError("cannot publish before all supplement batches are consumed")
        now = _utc_now()
        with store.tx() as conn:
            stage_rows = conn.execute(
                "SELECT * FROM stage_vehicle_energy WHERE run_id=? ORDER BY window_start_us,vehicle_id",
                (context.run_id,),
            ).fetchall()
            for row in stage_rows:
                ratio_text = energy_per_100km(int(row["mileage_u"]), int(row["energy_u"]))
                conn.execute(
                    "INSERT INTO vehicle_energy(run_id,plan_mode,vehicle_id,window_start,"
                    "window_start_us,mileage_delta,energy_delta,energy_per_100km,"
                    "energy_per_100km_text,event_count,telemetry_count,supplement_count,published_at) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        context.mode,
                        row["vehicle_id"],
                        iso_us(int(row["window_start_us"])),
                        int(row["window_start_us"]),
                        number_from_fixed(int(row["mileage_u"])),
                        number_from_fixed(int(row["energy_u"])),
                        float(ratio_text),
                        ratio_text,
                        int(row["event_count"]),
                        int(row["telemetry_count"]),
                        int(row["supplement_count"]),
                        now,
                    ),
                )
            total_rows = conn.execute(
                "SELECT vehicle_id, SUM(mileage_u) AS mileage_u, SUM(energy_u) AS energy_u, "
                "SUM(event_count) AS event_count, SUM(telemetry_count) AS telemetry_count, "
                "SUM(supplement_count) AS supplement_count, COUNT(*) AS window_count "
                "FROM stage_vehicle_energy WHERE run_id=? GROUP BY vehicle_id "
                "ORDER BY vehicle_id",
                (context.run_id,),
            ).fetchall()
            for row in total_rows:
                ratio_text = energy_per_100km(int(row["mileage_u"]), int(row["energy_u"]))
                conn.execute(
                    "INSERT INTO vehicle_energy_total(run_id,plan_mode,vehicle_id,mileage_delta,"
                    "energy_delta,energy_per_100km,energy_per_100km_text,event_count,"
                    "telemetry_count,supplement_count,window_count,published_at) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        context.mode,
                        row["vehicle_id"],
                        number_from_fixed(int(row["mileage_u"])),
                        number_from_fixed(int(row["energy_u"])),
                        float(ratio_text),
                        ratio_text,
                        int(row["event_count"]),
                        int(row["telemetry_count"]),
                        int(row["supplement_count"]),
                        int(row["window_count"]),
                        now,
                    ),
                )
            window_rows = conn.execute(
                "SELECT * FROM stage_window_manifest WHERE run_id=? ORDER BY window_start_us",
                (context.run_id,),
            ).fetchall()
            for row in window_rows:
                conn.execute(
                    "INSERT INTO window_manifest(run_id,plan_mode,window_start,window_start_us,"
                    "window_end,vehicle_count,event_count,published_at) VALUES (?,?,?,?,?,?,?,?)",
                    (
                        context.run_id,
                        context.mode,
                        iso_us(int(row["window_start_us"])),
                        int(row["window_start_us"]),
                        iso_us(int(row["window_end_us"])),
                        int(row["vehicle_count"]),
                        int(row["event_count"]),
                        now,
                    ),
                )
            conn.execute(
                "UPDATE job_runs SET status='published', published_at=?, updated_at=?, error_code=NULL "
                "WHERE run_id=?",
                (now, now, context.run_id),
            )

    def _build_report(
        self,
        store: StateStore,
        context: _RunContext,
        *,
        include_verification: bool = True,
    ) -> dict[str, Any]:
        run = store.fetchone("SELECT * FROM job_runs WHERE run_id=?", (context.run_id,))
        snapshot = context.memory.snapshot()
        checkpoints = store.fetchall(
            "SELECT * FROM checkpoints WHERE run_id=? ORDER BY commit_seq",
            (context.run_id,),
        )
        resume_events = store.fetchall(
            "SELECT * FROM resume_events WHERE run_id=? ORDER BY resume_seq",
            (context.run_id,),
        )
        issues = store.fetchall(
            "SELECT * FROM event_issues WHERE run_id=? ORDER BY "
            "issue_type,vehicle_id,trip_id,window_start_us,event_time_us,source_id",
            (context.run_id,),
        )
        late_details = [
            {
                "vehicle_id": row["vehicle_id"],
                "trip_id": row["trip_id"],
                "window_start": iso_us(int(row["window_start_us"])),
                "event_time": iso_us(int(row["event_time_us"])),
                "batch_id": row["batch_id"],
                "source_id": row["source_id"],
                "detail": row["detail"],
            }
            for row in issues
            if row["issue_type"] == "late_event"
        ]
        late_by_source = Counter(
            row["source_type"]
            for row in issues
            if row["issue_type"] == "late_event"
        )
        dedup_issues = [row for row in issues if row["issue_type"].startswith("duplicate_")]
        dedup_by_source = Counter(row["source_type"] for row in dedup_issues)
        orphan_count = sum(1 for row in issues if row["issue_type"] == "orphan_supplement")
        loads = store.fetchall(
            "SELECT * FROM vehicle_load WHERE run_id=? ORDER BY vehicle_id",
            (context.run_id,),
        )
        skew_records = [
            {
                "vehicle_id": row["vehicle_id"],
                "telemetry_count": int(row["telemetry_count"]),
                "share": round(float(row["share"]), 9),
                "split": bool(row["split"]),
                "slots": json.loads(row["slots_json"]),
                "unit_count": int(row["unit_count"]),
            }
            for row in loads
            if bool(row["split"])
        ]
        slot_rows = store.fetchall(
            "SELECT * FROM slot_stats WHERE run_id=? ORDER BY slot",
            (context.run_id,),
        )
        slot_total = sum(int(row["event_count"]) for row in slot_rows)
        slot_stats = [
            {
                "slot": int(row["slot"]),
                "event_count": int(row["event_count"]),
                "telemetry_count": int(row["telemetry_count"]),
                "supplement_count": int(row["supplement_count"]),
                "share": round(int(row["event_count"]) / max(1, slot_total), 9),
            }
            for row in slot_rows
        ]
        spill_rows = store.fetchall(
            "SELECT * FROM spill_events WHERE run_id=? ORDER BY sequence_no",
            (context.run_id,),
        )
        spill_bytes = sum(int(row["bytes_written"]) for row in spill_rows)
        last_checkpoint = checkpoints[-1] if checkpoints else None
        last_watermark_checkpoint = next(
            (
                checkpoint
                for checkpoint in reversed(checkpoints)
                if checkpoint["watermark_us"] is not None
            ),
            None,
        )
        state_rows = int(
            store.fetchone(
                "SELECT COUNT(*) AS count FROM window_state WHERE run_id=?",
                (context.run_id,),
            )["count"]
        )
        verification = None
        if include_verification:
            row = store.fetchone(
                "SELECT result_json FROM verification_results WHERE run_id=? "
                "AND comparison LIKE 'against:%' ORDER BY created_at DESC LIMIT 1",
                (context.run_id,),
            )
            if row:
                verification = json.loads(row["result_json"])
        finished_at = _utc_now()
        report: dict[str, Any] = {
            "report_version": 1,
            "run_id": context.run_id,
            "plan_mode": context.mode,
            "status": run["status"],
            "sql_sha256": run["sql_sha256"],
            "dataset_sha256": run["dataset_sha256"],
            "inputs": {
                "telemetry_rows": context.manifest.telemetry_rows,
                "telemetry_partitions": len(context.manifest.telemetry_partitions),
                "supplement_rows": context.manifest.supplement_rows,
                "supplement_batches": context.manifest.batch_count,
            },
            "scan": {
                "mode": context.mode,
                "parquet_filter_applied": context.mode == "pushdown",
                "full_replay": context.mode == "replay",
                "safe_predicate_count": len(context.query.pushed_predicates),
                "retained_predicate_count": len(context.query.retained_predicates),
            },
            "predicates": {
                "pushed": [predicate.to_dict() for predicate in context.query.pushed_predicates],
                "retained": [predicate.to_dict() for predicate in context.query.retained_predicates],
            },
            "preaggregation": {
                **context.query.plan_summary()["local_preaggregation"],
                "pushed_aggregates": context.query.plan_summary()["pushed_aggregates"],
            },
            "dedup": {
                "key_rule": (
                    "source_type, vehicle_id, trip_id, window_start and source_id "
                    "(event_time/mileage/energy fallback when no source_id); "
                    "first deterministic arrival wins"
                ),
                "discarded": len(dedup_issues),
                "by_source": dict(sorted(dedup_by_source.items())),
            },
            "late": {
                "count": len(late_details),
                "by_source": dict(sorted(late_by_source.items())),
                "watermark_allowed_lateness_seconds": int(
                    context.config.allowed_lateness_seconds
                ),
                "details": sorted(
                    late_details,
                    key=lambda item: (
                        item["vehicle_id"],
                        item["trip_id"],
                        item["window_start"],
                        item["event_time"],
                        item["source_id"],
                    ),
                ),
            },
            "join": {
                "keys": ["vehicle_id", "trip_id"],
                "orphan_supplements": orphan_count,
            },
            "skew": {
                "threshold_ratio": context.config.skew_threshold_ratio,
                "vehicles": skew_records,
            },
            "slots": {
                "requested": int(context.config.slots),
                "effective": context.config.effective_slots,
                "cpu_workers": context.config.effective_workers,
                "per_slot": slot_stats,
                "max_share": round(max((item["share"] for item in slot_stats), default=0.0), 9),
            },
            "checkpoints": {
                "consumed_batch": int(run["consumed_batch"]),
                "commit_seq": int(run["commit_seq"]),
                "last_committed_batch": (
                    int(last_checkpoint["batch_id"]) if last_checkpoint else None
                ),
                "watermark": (
                    iso_us(int(last_watermark_checkpoint["watermark_us"]))
                    if last_watermark_checkpoint is not None
                    else None
                ),
                "committed_windows": sum(
                    int(row["closed_windows"]) for row in checkpoints
                ),
                "resume_count": len(resume_events),
                "resume_points": [
                    {
                        "resume_seq": int(row["resume_seq"]),
                        "commit_seq": int(row["commit_seq"]),
                        "consumed_batch": int(row["consumed_batch"]),
                    }
                    for row in resume_events
                ],
            },
            "spill": {
                "bytes": spill_bytes,
                "files": len(spill_rows),
                "temporary_directory_removed": True,
            },
            "memory": {
                "configured_limit_bytes": int(context.config.memory_limit_bytes),
                "deterministic_estimate_bytes": deterministic_memory_estimate(
                    telemetry_rows=context.manifest.telemetry_rows,
                    supplement_rows=context.manifest.supplement_rows,
                    state_rows=state_rows,
                    slots=context.config.effective_slots,
                    batch_buffer_bytes=spill_bytes,
                ),
                "runtime_peak_rss_bytes": int(snapshot.peak_rss_bytes),
                "python_peak_bytes": int(snapshot.python_peak_bytes),
            },
            "runtime": {
                "started_at": context.started_at,
                "finished_at": finished_at,
                "duration_ms": int((time.monotonic() - context.start_monotonic) * 1000),
            },
        }
        if verification is not None:
            report["verification"] = verification
        report["stable_hash"] = _stable_report_hash(report)
        return report

    def _store_report(
        self, store: StateStore, context: _RunContext, report: dict[str, Any]
    ) -> None:
        with store.tx() as conn:
            conn.execute(
                "INSERT OR REPLACE INTO job_reports(run_id,plan_mode,report_json,stable_hash,created_at) "
                "VALUES (?,?,?,?,?)",
                (
                    context.run_id,
                    context.mode,
                    canonical_json(report),
                    report["stable_hash"],
                    _utc_now(),
                ),
            )

    def _load_or_build_report(
        self,
        store: StateStore,
        run_id: str,
        query: SettlementQuery,
        manifest: DatasetManifest,
    ) -> dict[str, Any]:
        row = store.fetchone("SELECT report_json FROM job_reports WHERE run_id=?", (run_id,))
        if row:
            return json.loads(row["report_json"])
        run = store.fetchone("SELECT * FROM job_runs WHERE run_id=?", (run_id,))
        if not run:
            raise StateError("published run has no state row")
        memory = MemoryGuard(self.config.memory_limit_bytes)
        spill = SpillManager(
            self.config.temp_dir,
            threshold_bytes=self.config.spill_threshold_bytes,
            memory_limit_bytes=self.config.memory_limit_bytes,
            temp_limit_bytes=self.config.temp_limit_bytes,
        )
        context = _RunContext(
            run_id=run_id,
            mode=str(run["plan_mode"]),
            query=query,
            manifest=manifest,
            config=self.config,
            memory=memory,
            spill=spill,
            started_at=str(run["created_at"]),
            start_monotonic=time.monotonic(),
        )
        try:
            memory.start()
            report = self._build_report(store, context)
            self._store_report(store, context, report)
            return report
        finally:
            memory.stop()
            spill.cleanup()

    def _refresh_report(
        self,
        store: StateStore,
        run_id: str,
        query: SettlementQuery,
        manifest: DatasetManifest,
        verification: dict[str, Any],
    ) -> dict[str, Any]:
        row = store.fetchone("SELECT report_json FROM job_reports WHERE run_id=?", (run_id,))
        if not row:
            report = self._load_or_build_report(store, run_id, query, manifest)
        else:
            report = json.loads(row["report_json"])
        report["verification"] = verification
        report["stable_hash"] = _stable_report_hash(report)
        run = store.fetchone("SELECT plan_mode FROM job_runs WHERE run_id=?", (run_id,))
        with store.tx() as conn:
            conn.execute(
                "INSERT OR REPLACE INTO job_reports(run_id,plan_mode,report_json,stable_hash,created_at) "
                "VALUES (?,?,?,?,?)",
                (
                    run_id,
                    run["plan_mode"],
                    canonical_json(report),
                    report["stable_hash"],
                    _utc_now(),
                ),
            )
        return report

    def _write_report_file(self, report: dict[str, Any]) -> None:
        if self.config.report_out is None:
            return
        path = Path(self.config.report_out)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(report, sort_keys=True, indent=2) + "\n", encoding="utf-8")

    def _write_result_report(self, results: list[ExecutionResult]) -> None:
        if not results:
            return
        if len(results) == 1:
            self._write_report_file(results[0].report)
            return
        payload = {
            "report_version": 1,
            "plan_mode": "both",
            "inputs": results[0].report.get("inputs"),
            "predicates": results[0].report.get("predicates"),
            "pushdown": results[0].report,
            "replay": results[1].report,
            "verification": results[-1].report.get("verification"),
        }
        self._write_report_file(payload)

    def _insert_issue(
        self,
        conn: Any,
        context: _RunContext,
        issue_type: str,
        event: dict[str, Any],
        *,
        detail: str,
    ) -> None:
        conn.execute(
            "INSERT INTO event_issues(run_id,plan_mode,issue_type,source_type,vehicle_id,trip_id,"
            "window_start_us,event_time_us,batch_id,source_id,detail) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                context.run_id,
                context.mode,
                issue_type,
                event["source_type"],
                event["vehicle_id"],
                event["trip_id"],
                int(event["window_start_us"]),
                int(event["event_time_us"]),
                event.get("batch_id"),
                event["source_id"],
                detail,
            ),
        )

    def _mark_run(self, store: StateStore, run_id: str, status: str, code: str | None) -> None:
        try:
            with store.tx() as conn:
                conn.execute(
                    "UPDATE job_runs SET status=?, error_code=?, updated_at=? WHERE run_id=?",
                    (status, code, _utc_now(), run_id),
                )
        except Exception:
            pass

    def _consumed_batch(self, store: StateStore, run_id: str) -> int:
        try:
            row = store.fetchone("SELECT consumed_batch FROM job_runs WHERE run_id=?", (run_id,))
            return int(row["consumed_batch"]) if row else 0
        except Exception:
            return 0

    def _commit_seq(self, store: StateStore, run_id: str) -> int:
        row = store.fetchone("SELECT commit_seq FROM job_runs WHERE run_id=?", (run_id,))
        return int(row["commit_seq"]) if row else 0

    def _record_runtime_meta(
        self,
        store: StateStore,
        context: _RunContext,
        snapshot: MemorySnapshot,
    ) -> None:
        try:
            with store.tx() as conn:
                conn.execute(
                    "INSERT OR REPLACE INTO job_meta(key,value) VALUES (?,?)",
                    (
                        f"runtime_memory:{context.run_id}",
                        canonical_json(
                            {
                                "peak_rss_bytes": snapshot.peak_rss_bytes,
                                "python_peak_bytes": snapshot.python_peak_bytes,
                            }
                        ),
                    ),
                )
        except Exception:
            pass

    def _delete_run(self, store: StateStore, run_id: str) -> None:
        with store.tx() as conn:
            self._delete_run_tables(conn, run_id)
            conn.execute("DELETE FROM job_meta WHERE key LIKE 'fault_injected:%'")
            conn.execute("DELETE FROM job_meta WHERE key LIKE 'stop_after_batch_claimed:%'")

    @staticmethod
    def _delete_run_tables(conn: Any, run_id: str) -> None:
        tables = [
            "checkpoints",
            "resume_events",
            "vehicle_route",
            "unit_route",
            "vehicle_load",
            "trip_keys",
            "slot_stats",
            "dedup_seen",
            "window_state",
            "closed_windows",
            "stage_vehicle_energy",
            "stage_window_manifest",
            "vehicle_energy",
            "vehicle_energy_total",
            "window_manifest",
            "batch_attempts",
            "event_issues",
            "spill_events",
            "verification_results",
            "job_reports",
            "job_runs",
        ]
        for table in tables:
            conn.execute(f"DELETE FROM {table} WHERE run_id=?", (run_id,))


def _dedup_key(event: dict[str, Any]) -> str:
    source_id = event.get("source_id")
    if source_id:
        payload = [
            event["source_type"],
            event["vehicle_id"],
            event["trip_id"],
            int(event["window_start_us"]),
            str(source_id),
        ]
    else:
        payload = [
            event["source_type"],
            event["vehicle_id"],
            event["trip_id"],
            int(event["window_start_us"]),
            int(event["event_time_us"]),
            int(event["mileage_u"]),
            int(event["energy_u"]),
        ]
    return sha256_text(canonical_json(payload))


def _upsert_state(conn: Any, run_id: str, event: dict[str, Any]) -> None:
    event_count = int(event.get("event_count", 1))
    supplement_count = event_count if event["source_type"] == "charging_supplement" else 0
    slot = int(event["_processing_slot"]) if "_processing_slot" in event else int(event["processing_slot"])
    conn.execute(
        "INSERT INTO window_state(run_id,window_start_us,window_end_us,processing_slot,"
        "vehicle_id,trip_id,source_type,mileage_u,energy_u,event_count,supplement_count) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?) "
        "ON CONFLICT(run_id,window_start_us,processing_slot,vehicle_id,trip_id,source_type) "
        "DO UPDATE SET mileage_u=mileage_u+excluded.mileage_u, "
        "energy_u=energy_u+excluded.energy_u, event_count=event_count+excluded.event_count, "
        "supplement_count=supplement_count+excluded.supplement_count",
        (
            run_id,
            int(event["window_start_us"]),
            int(event["window_end_us"]),
            slot,
            event["vehicle_id"],
            event["trip_id"],
            event["source_type"],
            int(event["mileage_u"]),
            int(event["energy_u"]),
            event_count,
            supplement_count,
        ),
    )


def _group_events(
    events: Iterable[dict[str, Any]],
    routing: RoutingPlan,
    *,
    workers: int = 2,
) -> list[dict[str, Any]]:
    assigned: dict[int, list[dict[str, Any]]] = defaultdict(list)
    for event in events:
        slot = int(event.get("_processing_slot", routing.route(event)))
        event["_processing_slot"] = slot
        assigned[slot].append(event)

    groups: list[dict[str, Any]] = []
    if workers > 1 and len(assigned) > 1:
        with ThreadPoolExecutor(max_workers=min(workers, len(assigned))) as executor:
            for part in executor.map(_group_slot_events, assigned.values()):
                groups.extend(part)
    else:
        for part in assigned.values():
            groups.extend(_group_slot_events(part))
    return groups


def _group_slot_events(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    groups: dict[tuple[int, int, str, str, str], dict[str, Any]] = {}
    for event in events:
        key = (
            int(event["_processing_slot"]),
            int(event["window_start_us"]),
            event["vehicle_id"],
            event["trip_id"],
            event["source_type"],
        )
        group = groups.get(key)
        if group is None:
            group = dict(event)
            group["event_count"] = 1
            groups[key] = group
        else:
            group["mileage_u"] += event["mileage_u"]
            group["energy_u"] += event["energy_u"]
            group["event_count"] += 1
    return list(groups.values())


def _stable_report_hash(report: dict[str, Any]) -> str:
    stable = json.loads(json.dumps(report, sort_keys=True))
    stable.pop("stable_hash", None)
    stable.pop("runtime", None)
    memory = stable.get("memory", {})
    memory.pop("runtime_peak_rss_bytes", None)
    memory.pop("python_peak_bytes", None)
    stable["memory"] = memory
    return sha256_text(canonical_json(stable))


def _utc_now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")

