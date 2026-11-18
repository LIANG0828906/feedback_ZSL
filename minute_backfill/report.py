"""Stable, reviewable job report and window detail exports."""

from __future__ import annotations

import csv
import hashlib
import io
import json
from pathlib import Path
from typing import Any

from .inputs import InputCatalog
from .slots import assign_slots
from .state import StateStore, RunContext
from .timeutil import format_amount_minor, format_time_us
from .util import atomic_write_text, write_json


def _fmt_optional_time(value: int | None) -> str | None:
    return None if value is None else format_time_us(int(value))


def _format_minute_window(row: dict[str, Any]) -> dict[str, Any]:
    start = int(row["window_start"])
    return {
        "window_start": format_time_us(start),
        "window_end": format_time_us(start + 60_000_000),
        "pay_count": int(row["pay_count"]),
        "pay_amount": format_amount_minor(int(row["pay_amount_minor"])),
        "pay_amount_minor": int(row["pay_amount_minor"]),
        "refund_count": int(row["refund_count"]),
        "refund_amount": format_amount_minor(int(row["refund_amount_minor"])),
        "refund_amount_minor": int(row["refund_amount_minor"]),
        "net": format_amount_minor(int(row["net_minor"])),
        "net_minor": int(row["net_minor"]),
        "currency": str(row["currency"]),
        "close_reason": str(row["close_reason"]),
        "commit_seq": int(row["commit_seq"]),
        "closed_watermark": _fmt_optional_time(row.get("closed_watermark")),
    }


def _skew_report(state: StateStore, *, slots: int, threshold: float) -> dict[str, Any]:
    groups = list(state.group_meta())
    assignment = assign_slots(groups, slots=slots, threshold=threshold)
    loads = []
    for slot, events in enumerate(assignment.slot_loads):
        loads.append(
            {
                "slot": slot,
                "events": events,
                "share": round(events / assignment.total_events, 8)
                if assignment.total_events
                else 0.0,
            }
        )
    actual_loads = [
        state.counter_get(f"slot_events_{slot}") for slot in range(slots)
    ]
    actual_total = sum(actual_loads)
    actual_rows = [
        {
            "slot": slot,
            "events": events,
            "share": round(events / actual_total, 8) if actual_total else 0.0,
        }
        for slot, events in enumerate(actual_loads)
    ]
    actual_max_share = (
        max(actual_loads) / actual_total if actual_total else 0.0
    )
    return {
        "method": (
            "greedy key redistribution; keys above threshold are split by "
            "minute window while payment and refund of a key-window stay together"
        ),
        "slots": slots,
        "threshold": threshold,
        "canonical_events": assignment.total_events,
        "slot_loads": loads,
        "max_share": round(assignment.max_share, 8),
        "threshold_satisfied": assignment.max_share <= threshold + 1e-12,
        "actual_processed_events": actual_total,
        "actual_slot_loads": actual_rows,
        "actual_max_share": round(actual_max_share, 8),
        "actual_threshold_satisfied": actual_max_share <= threshold + 1e-12,
        "heavy_keys": assignment.heavy_keys,
        "unsplittable_heavy_keys": assignment.unsplittable_heavy_keys,
        "splits": assignment.splits,
    }


def build_report(
    *,
    state: StateStore,
    catalog: InputCatalog,
    config: Any,
    run_context: RunContext,
    started_at: str,
    finished_at: str,
    duration_seconds: float,
    peak_memory_bytes: int,
    status: str,
    error: dict[str, Any] | None,
    temp_cleanup: bool,
) -> dict[str, Any]:
    consumed = state.consumed_inputs()
    consumed_ids = {item.input_id for item in consumed}
    shard_rows = sum(item.rows or 0 for item in catalog.shards)
    batch_rows = sum(item.rows or 0 for item in catalog.batches)
    consumed_shards = sum(1 for item in consumed if item.kind == "history_shard")
    consumed_batches = sum(1 for item in consumed if item.kind == "refund_batch")
    windows = [_format_minute_window(row) for row in state.minute_windows()]
    late = state.late_summary()
    late["by_window"] = [
        {
            **row,
            "window_start": format_time_us(int(row["window_start"])),
            "window_end": format_time_us(int(row["window_start"]) + 60_000_000),
            "late_amount": format_amount_minor(int(row["late_amount_minor"])),
            "first_late_event_time": _fmt_optional_time(row["first_late_event_time"]),
            "last_late_event_time": _fmt_optional_time(row["last_late_event_time"]),
        }
        for row in late["by_window"]
    ]
    canonical_count = state.canonical_event_count()
    canonical_by_side = state.canonical_counts_by_side()
    join_diagnostics = state.join_diagnostics()
    estimated_peak = (
        canonical_count * 320
        + state.open_key_windows_count() * 256
        + max(0, config.workers) * 24 * 1024 * 1024
        + 48 * 1024 * 1024
    )
    report: dict[str, Any] = {
        "schema_version": 1,
        "status": status,
        "error": error,
        "semantics": {
            "event_time": "UTC epoch microseconds internally",
            "window": "tumbling minute [start, start+60s)",
            "dedup_key": "side + payment_id + event_minute",
            "dedup_rule": "keep earliest event_time; equal event_time keeps first arrival",
            "join": "same payment_id and minute; refund event_time >= payment event_time",
            "watermark": "max(consumed batch declared_event_time) - allowed_lateness",
            "window_close": "watermark >= window_end",
            "late_rule": "events for an already closed window are counted, never applied",
            "final_flush": "open windows are committed at end of input with close_reason=end_of_input",
        },
        "input": {
            "data_dir": catalog.data_dir.as_posix(),
            "schema_manifest": catalog.manifest_path.name,
            "schema_manifest_sha256": catalog.schema_sha256,
            "payment_shard_count": len(catalog.shards),
            "refund_batch_count": len(catalog.batches),
            "payment_rows_declared": shard_rows,
            "refund_rows_declared": batch_rows,
            "payment_shards": [
                {
                    "shard_id": item.shard_id,
                    "rows": item.rows,
                    "sha256": item.sha256,
                    "declared_event_time": _fmt_optional_time(item.declared_event_time_us),
                    "partitions": list(item.partitions),
                    "consumed": item.shard_id in consumed_ids,
                }
                for item in catalog.shards
            ],
            "refund_batches": [
                {
                    "batch_id": item.batch_id,
                    "batch_index": item.batch_index,
                    "rows": item.rows,
                    "sha256": item.sha256,
                    "declared_event_time": format_time_us(item.declared_event_time_us),
                    "consumed": item.batch_id in consumed_ids,
                }
                for item in catalog.batches
            ],
        },
        "processing": {
            "consumed_inputs": [
                {
                    "commit_seq": item.seq,
                    "input_id": item.input_id,
                    "kind": item.kind,
                    "batch_index": item.batch_index,
                    "rows": item.rows,
                    "declared_event_time": _fmt_optional_time(item.declared_event_time),
                    "watermark_after": _fmt_optional_time(item.watermark_after),
                }
                for item in consumed
            ],
            "consumed_shards": consumed_shards,
            "consumed_batches": consumed_batches,
            "last_commit_seq": consumed[-1].seq if consumed else 0,
            "last_consumed_batch_index": max(
                (item.batch_index for item in consumed if item.batch_index is not None),
                default=0,
            ),
            "watermark": _fmt_optional_time(state.watermark()),
            "watermark_us": state.watermark(),
        },
        "dedup": {
            "canonical_events": canonical_count,
            "canonical_payment_events": canonical_by_side.get("PAYMENT", 0),
            "canonical_refund_events": canonical_by_side.get("REFUND", 0),
            "dropped": state.dedup_dropped(),
            "dropped_payment_events": state.counter_get("dedup_dropped_payment"),
            "dropped_refund_events": state.counter_get("dedup_dropped_refund"),
            "key_windows": state.key_window_count(),
        },
        "late": late,
        "windows": windows,
        "join_diagnostics": {
            "unmatched_refunds": join_diagnostics["unmatched_refunds"],
            "currency_mismatches": join_diagnostics["currency_mismatches"],
        },
        "skew": _skew_report(
            state,
            slots=int(config.slots),
            threshold=float(config.skew_threshold),
        ),
        "state": state.state_summary(),
        "recovery": state.recovery_report(),
        "resources": {
            "logical_slots": int(config.slots),
            "worker_processes": int(config.workers),
            "memory_limit_bytes": int(config.memory_limit_mb * 1024 * 1024),
            "spill_threshold_bytes": int(config.spill_mb * 1024 * 1024),
            "peak_memory_bytes": peak_memory_bytes,
            "peak_memory_bytes_estimated": estimated_peak,
            "peak_memory_under_1gib": peak_memory_bytes < 1_073_741_824,
            "spill_bytes": state.spill_bytes(),
            "temp_cleanup": temp_cleanup,
        },
        "warnings": list(catalog.warnings),
        "timing": {
            "started_at": started_at,
            "finished_at": finished_at,
            "duration_seconds": round(duration_seconds, 6),
        },
    }
    digest_payload = {
        "input": {
            "schema_manifest_sha256": report["input"]["schema_manifest_sha256"],
            "payment_shards": [
                {
                    "shard_id": item["shard_id"],
                    "rows": item["rows"],
                    "sha256": item["sha256"],
                }
                for item in report["input"]["payment_shards"]
            ],
            "refund_batches": [
                {
                    "batch_id": item["batch_id"],
                    "batch_index": item["batch_index"],
                    "rows": item["rows"],
                    "sha256": item["sha256"],
                    "declared_event_time": item["declared_event_time"],
                }
                for item in report["input"]["refund_batches"]
            ],
        },
        "processing": report["processing"],
        "dedup": report["dedup"],
        "late": report["late"],
        "windows": report["windows"],
        "join_diagnostics": report["join_diagnostics"],
        "skew": report["skew"],
    }
    report["business_digest"] = hashlib.sha256(
        json.dumps(
            digest_payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    ).hexdigest()
    return report


def write_report(
    out_dir: Path,
    report: dict[str, Any],
    *,
    partial: bool = False,
) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / ("report.partial.json" if partial else "report.json")
    write_json(path, report)
    return path


def _csv_text(fieldnames: list[str], rows: list[dict[str, Any]]) -> str:
    buffer = io.StringIO(newline="")
    writer = csv.DictWriter(
        buffer,
        fieldnames=fieldnames,
        lineterminator="\n",
        extrasaction="ignore",
    )
    writer.writeheader()
    for row in rows:
        writer.writerow(row)
    return buffer.getvalue()


def write_window_csv(out_dir: Path, report: dict[str, Any], *, partial: bool = False) -> Path:
    fieldnames = [
        "window_start",
        "window_end",
        "pay_count",
        "pay_amount",
        "refund_count",
        "refund_amount",
        "net",
        "currency",
        "close_reason",
        "commit_seq",
    ]
    path = out_dir / ("windows.partial.csv" if partial else "windows.csv")
    atomic_write_text(path, _csv_text(fieldnames, report["windows"]))
    return path


def write_key_window_csv(out_dir: Path, state: StateStore, *, partial: bool = False) -> Path:
    fieldnames = [
        "window_start",
        "payment_id",
        "pay_count",
        "pay_amount",
        "refund_count",
        "refund_amount",
        "currency",
        "closed",
        "close_reason",
        "commit_seq",
    ]
    rows = []
    for row in state.key_windows():
        rows.append(
            {
                "window_start": format_time_us(int(row["window_start"])),
                "payment_id": row["payment_id"],
                "pay_count": row["pay_count"],
                "pay_amount": format_amount_minor(int(row["pay_amount_minor"])),
                "refund_count": row["refund_count"],
                "refund_amount": format_amount_minor(int(row["refund_amount_minor"])),
                "currency": row["currency"],
                "closed": int(row["closed"]),
                "close_reason": row["close_reason"] or "",
                "commit_seq": row["commit_seq"] if row["commit_seq"] is not None else "",
            }
        )
    path = out_dir / ("key_windows.partial.csv" if partial else "key_windows.csv")
    atomic_write_text(path, _csv_text(fieldnames, rows))
    return path
