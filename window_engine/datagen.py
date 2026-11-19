"""Deterministic local data generation (fixed seed, no network needed).

Produces hourly Parquet history shards plus JSON Lines incremental batches,
with a manifest declaring the schema. Re-running with the same seed yields
byte-identical logical data.
"""

from __future__ import annotations

import json
import os
import random
from dataclasses import dataclass

import pyarrow as pa
import pyarrow.parquet as pq

from .semantics import window_start
from .util import ensure_dir, write_json

SCHEMA = {
    "fields": [
        {"name": "event_id", "type": "string", "nullable": False},
        {"name": "payment_id", "type": "string", "nullable": False},
        {"name": "event_time_ms", "type": "int64", "nullable": False},
        {"name": "amount_cents", "type": "int64", "nullable": False},
        {"name": "event_type", "type": "string", "nullable": False},
    ],
    "dedup_key": ["payment_id", "window_start", "event_id"],
    "window_size_ms": 60_000,
}


@dataclass
class GenSpec:
    seed: int = 20261119
    history_rows: int = 120_000
    incremental_rows: int = 40_000
    batches: int = 128
    duplicate_ratio: float = 0.04
    delayed_ratio: float = 0.03
    delay_min_ms: int = 30_000
    delay_max_ms: int = 600_000
    hot_keys: int = 1
    hot_key_share: float = 0.55
    base_time_ms: int = 1_763_000_000_000  # fixed deterministic epoch anchor
    history_hours: int = 8


def _new_event(
    rng: random.Random,
    seq: int,
    spec: GenSpec,
    time_low_ms: int,
    time_span_ms: int,
) -> dict:
    if rng.random() < spec.hot_key_share:
        key_index = rng.randrange(spec.hot_keys)
        payment_id = f"PAY-HOT-{key_index:03d}"
    else:
        payment_id = f"PAY-{rng.randrange(spec.history_rows // 4):06d}"
    return {
        "event_id": f"EVT-{seq:08d}",
        "payment_id": payment_id,
        "event_time_ms": time_low_ms + rng.randrange(time_span_ms),
        "amount_cents": rng.randint(100, 500_000),
        "event_type": rng.choice(["CREATE", "UPDATE", "NOTIFY", "SETTLE"]),
    }


def _clone_with_delay(rng: random.Random, event: dict, seq: int, spec: GenSpec) -> dict:
    """Same business event reported again (same dedup identity).

    Duplicates keep the source event time and arrival position; they exercise
    the dedup path rather than the out-of-order / late path.
    """

    clone = dict(event)
    clone["event_id"] = event["event_id"]  # identical id => duplicate
    clone["event_time_ms"] = event["event_time_ms"]  # same event-time window
    clone["amount_cents"] = event["amount_cents"]
    return clone


def generate(data_dir: str, spec: GenSpec) -> dict:
    rng = random.Random(spec.seed)
    history_dir = ensure_dir(os.path.join(data_dir, "history"))
    stream_dir = ensure_dir(os.path.join(data_dir, "stream"))
    hour_ms = 3_600_000

    # ------------------------------------------------------------- history
    base_events: list[dict] = []
    seq = 0
    per_hour = spec.history_rows // spec.history_hours
    for hour in range(spec.history_hours):
        low = spec.base_time_ms + hour * hour_ms
        for _ in range(per_hour):
            base_events.append(_new_event(rng, seq, spec, low, hour_ms))
            seq += 1
    while seq < spec.history_rows:
        low = spec.base_time_ms
        base_events.append(
            _new_event(rng, seq, spec, low, spec.history_hours * hour_ms)
        )
        seq += 1

    # Duplicates inside the historical set.
    dup_count = int(spec.history_rows * spec.duplicate_ratio)
    history_dups = []
    for idx in range(dup_count):
        source = base_events[rng.randrange(len(base_events))]
        history_dups.append(
            _clone_with_delay(rng, source, seq + idx, spec)
        )
    seq += dup_count

    history_all = base_events + history_dups
    rng.shuffle(history_all)

    shards = []
    by_hour: dict[int, list[dict]] = {}
    for row in history_all:
        hour = (
            (row["event_time_ms"] - spec.base_time_ms) // 3_600_000
        )
        by_hour.setdefault(hour, []).append(row)
    for hour in sorted(by_hour):
        rows = sorted(
            by_hour[hour], key=lambda r: (r["event_time_ms"], r["event_id"])
        )
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
            }
        )
        name = f"hour={hour:02d}/part-0.parquet"
        full = os.path.join(history_dir, name)
        ensure_dir(os.path.dirname(full))
        pq.write_table(table, full, compression="zstd")
        shards.append({"path": f"history/{name}", "rows": len(rows), "hour": hour})

    # ----------------------------------------------------------- stream
    # Incremental events start exactly where history ends. Rows are produced
    # in event-time order, then each one is placed at an arrival position a
    # little later than its event time so batches are out of order while the
    # watermark still advances naturally.
    stream_low = spec.base_time_ms + spec.history_hours * hour_ms
    stream_span = max(hour_ms, spec.incremental_rows * 20)
    stream_base: list[dict] = []
    next_seq = seq
    for _ in range(spec.incremental_rows):
        stream_base.append(
            _new_event(rng, next_seq, spec, stream_low, stream_span)
        )
        next_seq += 1
    stream_base.sort(key=lambda r: r["event_time_ms"])

    # Delayed arrivals: same event time, arrival pushed 30-600s later. Delays
    # beyond the job tolerance become late events; the rest arrive in time.
    delayed_n = int(spec.incremental_rows * spec.delayed_ratio)
    delayed_indices = set(
        rng.sample(range(len(stream_base)), delayed_n)
    )
    for idx in delayed_indices:
        delay = rng.randint(spec.delay_min_ms, spec.delay_max_ms)
        stream_base[idx]["_delay_ms"] = delay

    # Assign monotonic arrival timestamps (~3000 events/min), then apply the
    # per-event delay on top of the base arrival position.
    arrival_start = stream_low
    for idx, row in enumerate(stream_base):
        base_arrival = arrival_start + idx * 20
        row["_arrival_ms"] = base_arrival + row.get("_delay_ms", 0)

    # Duplicates repeat an existing stream event with identical identity and
    # the same arrival position, so they exercise dedup rather than lateness.
    stream_dup_count = int(spec.incremental_rows * spec.duplicate_ratio)
    arrival_by_id = {row["event_id"]: row["_arrival_ms"] for row in stream_base}
    duplicates = []
    for idx in range(stream_dup_count):
        source = stream_base[rng.randrange(len(stream_base))]
        clone = _clone_with_delay(rng, source, next_seq + idx, spec)
        clone["_arrival_ms"] = arrival_by_id[source["event_id"]]
        duplicates.append(clone)
    next_seq += stream_dup_count

    stream_rows = stream_base + duplicates
    stream_rows.sort(key=lambda r: (r["_arrival_ms"], r["event_id"]))

    batches = []
    per_batch = max(1, len(stream_rows) // spec.batches)
    declared_max = 0
    for batch_id in range(spec.batches):
        start = batch_id * per_batch
        end = len(stream_rows) if batch_id == spec.batches - 1 else (
            start + per_batch
        )
        chunk = stream_rows[start:end]
        # Disorder inside the batch as well.
        rng.shuffle(chunk)
        declared = max(r["event_time_ms"] for r in chunk)
        declared_max = max(declared_max, declared)
        name = f"batch-{batch_id:04d}.jsonl"
        with open(os.path.join(stream_dir, name), "w", encoding="utf-8") as fh:
            for row in chunk:
                payload = {k: v for k, v in row.items() if not k.startswith("_")}
                payload["arrival_ms"] = row["_arrival_ms"]
                fh.write(json.dumps(payload, sort_keys=True) + "\n")
        batches.append(
            {
                "batch_id": batch_id,
                "path": f"stream/{name}",
                "rows": len(chunk),
                "declared_event_time_ms": declared,
            }
        )

    manifest = {
        "seed": spec.seed,
        "schema": SCHEMA,
        "history": shards,
        "stream_batches": batches,
        "history_row_count": len(history_all),
        "stream_row_count": len(stream_rows),
        "duplicate_rows": dup_count + stream_dup_count,
        "delayed_rows": delayed_n,
        "spec": spec.__dict__,
    }
    write_json(os.path.join(data_dir, "manifest.json"), manifest)
    return manifest
