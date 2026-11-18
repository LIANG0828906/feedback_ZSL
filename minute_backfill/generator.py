"""Deterministic local dataset generator.

The generated layout is:

    <out>/manifest.json
    <out>/payments/shards.json
    <out>/payments/day=YYYY-MM-DD/part-00000.parquet
    <out>/refunds/batches.json
    <out>/refunds/batch-000001.jsonl
    <out>/_generation_report.json
"""

from __future__ import annotations

import json
import math
import random
import shutil
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from typing import Any

import pyarrow as pa
import pyarrow.parquet as pq

from .timeutil import (
    MICROS_PER_MINUTE,
    format_time_us,
    minute_start_us,
    parse_time_us,
)
from .util import sha256_file, write_json


@dataclass
class GeneratorConfig:
    out_dir: Path
    seed: int = 42
    payment_rows: int = 100_000
    refund_rows: int = 40_000
    batches: int = 128
    minutes: int = 128
    duplicate_rate: float = 0.05
    delay_rate: float = 0.03
    late_min_seconds: int = 30
    late_max_seconds: int = 900
    out_of_order_rate: float = 0.20
    skew_share: float = 0.0
    hot_payment_id: str = "P-HOT-0001"
    start_time: str = "2026-11-01T00:00:00Z"
    currency: str = "CNY"
    force: bool = False


PAYMENT_FIELDS: dict[str, dict[str, Any]] = {
    "payment_id": {"type": "string", "nullable": False},
    "merchant_id": {"type": "string", "nullable": False},
    "event_time": {"type": "timestamp_us_utc", "nullable": False},
    "amount": {"type": "decimal(18,2)", "nullable": False},
    "currency": {"type": "string", "nullable": False},
    "event_type": {
        "type": "string",
        "nullable": False,
        "allowed_values": ["PAYMENT"],
    },
}

REFUND_FIELDS: dict[str, dict[str, Any]] = {
    "refund_id": {"type": "string", "nullable": False},
    "payment_id": {"type": "string", "nullable": False},
    "merchant_id": {"type": "string", "nullable": False},
    "event_time": {"type": "timestamp_us_utc", "nullable": False},
    "amount": {"type": "decimal(18,2)", "nullable": False},
    "currency": {"type": "string", "nullable": False},
    "event_type": {
        "type": "string",
        "nullable": False,
        "allowed_values": ["REFUND"],
    },
}


def _amount_text(minor: int) -> str:
    return f"{minor // 100}.{minor % 100:02d}"


def _amount_decimal(minor: int) -> Decimal:
    return (Decimal(minor) / Decimal(100)).quantize(Decimal("0.01"))


def _date_partition(event_time_us: int) -> str:
    seconds, micros = divmod(int(event_time_us), 1_000_000)
    dt = datetime.fromtimestamp(seconds, tz=timezone.utc).replace(microsecond=micros)
    return dt.strftime("%Y-%m-%d")


def _dt_from_us(event_time_us: int) -> datetime:
    seconds, micros = divmod(int(event_time_us), 1_000_000)
    return datetime.fromtimestamp(seconds, tz=timezone.utc).replace(microsecond=micros)


def _build_payments(
    cfg: GeneratorConfig, rng: random.Random, start_us: int
) -> list[dict[str, Any]]:
    if cfg.payment_rows <= 0:
        raise ValueError("payment_rows must be positive")
    if cfg.minutes <= 0:
        raise ValueError("minutes must be positive")
    rows: list[dict[str, Any]] = []
    hot_count = int(round(cfg.payment_rows * cfg.skew_share)) if cfg.skew_share else 0
    hot_count = min(hot_count, cfg.payment_rows)
    normal_count = cfg.payment_rows - hot_count

    for index in range(normal_count):
        minute_index = rng.randrange(cfg.minutes)
        minute_start = start_us + minute_index * MICROS_PER_MINUTE
        event_time = minute_start + rng.randrange(MICROS_PER_MINUTE)
        rows.append(
            {
                "payment_id": f"P{index + 1:09d}",
                "merchant_id": f"M{rng.randint(1, 250):04d}",
                "event_time": event_time,
                "amount_minor": rng.randint(100, 1_000_000),
                "currency": cfg.currency,
                "event_type": "PAYMENT",
                "_minute_index": minute_index,
            }
        )

    # Hot rows are spread across distinct minutes first so that the canonical
    # key-window state itself is skewed, not just the duplicate event count.
    hot_minutes = []
    if cfg.minutes >= hot_count:
        hot_minutes = rng.sample(range(cfg.minutes), hot_count)
    else:
        hot_minutes = [index % cfg.minutes for index in range(hot_count)]
    for minute_index in hot_minutes:
        minute_start = start_us + minute_index * MICROS_PER_MINUTE
        event_time = minute_start + rng.randrange(MICROS_PER_MINUTE)
        rows.append(
            {
                "payment_id": cfg.hot_payment_id,
                "merchant_id": "M0001",
                "event_time": event_time,
                "amount_minor": rng.randint(100, 200_000),
                "currency": cfg.currency,
                "event_type": "PAYMENT",
                "_minute_index": minute_index,
            }
        )
    rows.sort(key=lambda row: (row["event_time"], row["payment_id"]))
    for index, row in enumerate(rows, start=1):
        row["_payment_order"] = index
    return rows


def _build_refunds(
    cfg: GeneratorConfig,
    rng: random.Random,
    start_us: int,
    payments: list[dict[str, Any]],
) -> tuple[list[list[dict[str, Any]]], dict[str, int]]:
    if cfg.batches <= 0:
        raise ValueError("batches must be positive")
    if cfg.refund_rows < 0:
        raise ValueError("refund_rows must be non-negative")
    duplicate_count = int(round(cfg.refund_rows * cfg.duplicate_rate))
    unique_count = max(0, cfg.refund_rows - duplicate_count)
    batches: list[list[dict[str, Any]]] = [[] for _ in range(cfg.batches)]
    base_events: list[dict[str, Any]] = []
    delayed = 0
    out_of_order = 0
    for index in range(unique_count):
        payment = payments[rng.randrange(len(payments))]
        pay_time = int(payment["event_time"])
        minute_index = int(payment["_minute_index"])
        minute_end = start_us + (minute_index + 1) * MICROS_PER_MINUTE
        max_offset = max(0, minute_end - pay_time - 1)
        offset = rng.randint(0, max_offset) if max_offset else 0
        event_time = pay_time + offset
        amount = rng.randint(1, max(1, int(payment["amount_minor"])))
        arrival_batch = minute_index
        if rng.random() < cfg.delay_rate:
            delay_seconds = rng.randint(cfg.late_min_seconds, cfg.late_max_seconds)
            arrival_batch += max(1, math.ceil(delay_seconds / 60))
            delayed += 1
        elif rng.random() < cfg.out_of_order_rate:
            arrival_batch += 1
            out_of_order += 1
        arrival_batch = min(max(0, arrival_batch), cfg.batches - 1)
        row = {
            "refund_id": f"R{index + 1:09d}",
            "payment_id": payment["payment_id"],
            "merchant_id": payment["merchant_id"],
            "event_time": event_time,
            "amount_minor": amount,
            "currency": cfg.currency,
            "event_type": "REFUND",
            "_arrival_batch": arrival_batch,
            "_event_minute": minute_index,
        }
        base_events.append(row)
        batches[arrival_batch].append(dict(row))

    duplicates = 0
    for _ in range(duplicate_count):
        source = base_events[rng.randrange(len(base_events))] if base_events else None
        if source is None:
            break
        arrival_batch = int(source["_arrival_batch"])
        if rng.random() < 0.5:
            arrival_batch = min(cfg.batches - 1, arrival_batch + 1)
        copy = dict(source)
        copy["_duplicate"] = True
        batches[arrival_batch].append(copy)
        duplicates += 1

    for batch_index, rows in enumerate(batches):
        random.Random(cfg.seed * 1_000_003 + batch_index).shuffle(rows)
    stats = {
        "unique_refund_events": len(base_events),
        "duplicate_rows": duplicates,
        "delayed_rows": delayed,
        "out_of_order_rows": out_of_order,
    }
    return batches, stats


def _write_parquet(
    cfg: GeneratorConfig,
    payments: list[dict[str, Any]],
    out_dir: Path,
) -> list[dict[str, Any]]:
    by_day: dict[str, list[dict[str, Any]]] = {}
    for row in payments:
        by_day.setdefault(_date_partition(int(row["event_time"])), []).append(row)
    schema = pa.schema(
        [
            pa.field("payment_id", pa.string(), nullable=False),
            pa.field("merchant_id", pa.string(), nullable=False),
            pa.field("event_time", pa.timestamp("us", tz="UTC"), nullable=False),
            pa.field("amount", pa.decimal128(18, 2), nullable=False),
            pa.field("currency", pa.string(), nullable=False),
            pa.field("event_type", pa.string(), nullable=False),
        ]
    )
    shards: list[dict[str, Any]] = []
    for day in sorted(by_day):
        rows = sorted(by_day[day], key=lambda row: (row["event_time"], row["payment_id"]))
        table_rows = [
            {
                "payment_id": row["payment_id"],
                "merchant_id": row["merchant_id"],
                "event_time": _dt_from_us(int(row["event_time"])),
                "amount": _amount_decimal(int(row["amount_minor"])),
                "currency": row["currency"],
                "event_type": row["event_type"],
            }
            for row in rows
        ]
        table = pa.Table.from_pylist(table_rows, schema=schema)
        rel_path = Path("payments") / f"day={day}" / "part-00000.parquet"
        path = out_dir / rel_path
        path.parent.mkdir(parents=True, exist_ok=True)
        pq.write_table(
            table,
            path,
            compression="snappy",
            use_dictionary=True,
            write_statistics=True,
        )
        shards.append(
            {
                "shard_id": rel_path.as_posix(),
                "file": rel_path.as_posix(),
                "rows": len(rows),
                "sha256": sha256_file(path),
                "declared_event_time": format_time_us(
                    max(int(row["event_time"]) for row in rows)
                ),
                "partition_day": day,
            }
        )
    return shards


def _write_refunds(
    cfg: GeneratorConfig,
    batches: list[list[dict[str, Any]]],
    out_dir: Path,
    start_us: int,
) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    refund_dir = out_dir / "refunds"
    refund_dir.mkdir(parents=True, exist_ok=True)
    for index, rows in enumerate(batches, start=1):
        batch_id = f"batch-{index:06d}"
        rel_path = Path("refunds") / f"{batch_id}.jsonl"
        path = out_dir / rel_path
        with path.open("w", encoding="utf-8", newline="\n") as fh:
            for row in rows:
                payload = {
                    "refund_id": row["refund_id"],
                    "payment_id": row["payment_id"],
                    "merchant_id": row["merchant_id"],
                    "event_time": format_time_us(int(row["event_time"])),
                    "amount": _amount_text(int(row["amount_minor"])),
                    "currency": row["currency"],
                    "event_type": row["event_type"],
                }
                fh.write(
                    json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n"
                )
        declared_us = start_us + index * MICROS_PER_MINUTE
        result.append(
            {
                "batch_id": batch_id,
                "batch_index": index,
                "file": rel_path.as_posix(),
                "declared_event_time": format_time_us(declared_us),
                "rows": len(rows),
                "sha256": sha256_file(path),
            }
        )
    return result


def generate_dataset(cfg: GeneratorConfig) -> dict[str, Any]:
    out_dir = cfg.out_dir.resolve()
    if out_dir.exists():
        if not cfg.force and any(out_dir.iterdir()):
            raise ValueError(f"output directory is not empty: {out_dir} (use --force)")
        if cfg.force:
            shutil.rmtree(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    rng = random.Random(cfg.seed)
    start_us = minute_start_us(parse_time_us(cfg.start_time))
    payments = _build_payments(cfg, rng, start_us)
    shards = _write_parquet(cfg, payments, out_dir)
    batch_rows, refund_stats = _build_refunds(cfg, rng, start_us, payments)
    batch_manifest = _write_refunds(cfg, batch_rows, out_dir, start_us)

    manifest = {
        "manifest_version": 1,
        "dataset": "minute-backfill-demo",
        "seed": cfg.seed,
        "timezone": "UTC",
        "payments": {
            "format": "parquet",
            "manifest": "payments/shards.json",
            "partition": "day=YYYY-MM-DD",
            "fields": PAYMENT_FIELDS,
        },
        "refunds": {
            "format": "jsonl",
            "manifest": "refunds/batches.json",
            "fields": REFUND_FIELDS,
        },
        "semantics": {
            "dedup": "per side/payment_id/event_minute; earliest event_time wins, first arrival breaks ties",
            "window": "UTC tumbling minute [start, start+60s)",
            "join": "same payment_id and minute; refund must be at or after payment",
            "watermark": "max(declared_event_time) - allowed_lateness",
            "late": "events arriving after their minute window has closed",
        },
    }
    write_json(out_dir / "manifest.json", manifest)
    write_json(
        out_dir / "payments" / "shards.json",
        {"shards": shards, "generated_by": "minute_backfill.generator", "seed": cfg.seed},
    )
    write_json(
        out_dir / "refunds" / "batches.json",
        {"batches": batch_manifest, "generated_by": "minute_backfill.generator", "seed": cfg.seed},
    )
    report = {
        "config": {
            key: value
            for key, value in asdict(cfg).items()
            if key not in {"out_dir", "force"}
        },
        "payment_rows": len(payments),
        "refund_rows": sum(len(rows) for rows in batch_rows),
        "refund_batches": len(batch_manifest),
        "payment_shards": len(shards),
        "refund_stats": refund_stats,
        "payment_bytes": sum((out_dir / item["file"]).stat().st_size for item in shards),
        "refund_bytes": sum((out_dir / item["file"]).stat().st_size for item in batch_manifest),
        "duplicate_rate_actual": (
            refund_stats["duplicate_rows"] / max(1, sum(len(rows) for rows in batch_rows))
        ),
        "delay_rate_actual": (
            refund_stats["delayed_rows"] / max(1, refund_stats["unique_refund_events"])
        ),
    }
    write_json(out_dir / "_generation_report.json", report)
    return report
