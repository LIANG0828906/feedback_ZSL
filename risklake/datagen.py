"""Deterministic sample-data generator.

Produces, under ``<data_dir>``:

* ``historical/dt=.../hour=.../part-XXXX.parquet`` hourly shards
* ``incremental/batch_XXXX.jsonl`` increment batches
* ``manifest.json`` describing what was generated (for review/tests)

Everything is driven by a single seeded ``random.Random`` so re-running
with the same config yields byte-identical inputs.
"""

from __future__ import annotations

import json
import random
import time
from pathlib import Path

import pyarrow as pa
import pyarrow.parquet as pq

from .config import JobConfig

BASE_TIME = 1_700_000_000  # fixed epoch base, keeps runs reproducible
HOT_KEY = "ORD-HOT-0001"
HOT_KEY_SHARE = 0.45  # deliberately above the 0.40 slot skew threshold
NORMAL_KEYS = 2_000
REFUND_PROB = 0.35
HISTORICAL_HOURS = 12
BATCH_INTERVAL_SECONDS = 56

SCHEMA = pa.schema(
    [
        ("event_id", pa.string()),
        ("event_type", pa.string()),
        ("order_id", pa.string()),
        ("event_time", pa.int64()),
        ("amount_cents", pa.int64()),
    ]
)


def _normal_key(rng: random.Random) -> str:
    return f"ORD-{rng.randrange(NORMAL_KEYS):07d}"


def _make_events(
    rng: random.Random,
    target_rows: int,
    id_prefix: str,
    time_fn,
) -> list[dict]:
    """Generate ``target_rows`` unique order/refund events."""
    events: list[dict] = []
    seq = 0
    while len(events) < target_rows:
        seq += 1
        order_id = HOT_KEY if rng.random() < HOT_KEY_SHARE else _normal_key(rng)
        order_time = time_fn()
        amount = rng.randrange(100, 100_000)
        events.append(
            {
                "event_id": f"{id_prefix}{seq:08d}",
                "event_type": "order",
                "order_id": order_id,
                "event_time": order_time,
                "amount_cents": amount,
            }
        )
        if rng.random() < REFUND_PROB and len(events) < target_rows:
            seq += 1
            events.append(
                {
                    "event_id": f"{id_prefix}{seq:08d}",
                    "event_type": "refund",
                    "order_id": order_id,
                    "event_time": order_time + rng.randrange(0, 121),
                    "amount_cents": rng.randrange(100, amount + 1),
                }
            )
    return events[:target_rows]


def _inject_duplicates(
    rng: random.Random, batches: list[list[dict]], ratio: float
) -> int:
    """Append exact-copy duplicates of already emitted events.

    Duplicates may land in the same or a later batch (re-report), which is
    what forces cross-batch dedup. Returns the number of duplicates added.
    """
    total = sum(len(b) for b in batches)
    dup_target = int(round(total * ratio))
    added = 0
    for batch_idx, batch in enumerate(batches):
        if added >= dup_target:
            break
        pool = [e for b in batches[: batch_idx + 1] for e in b]
        if not pool:
            continue
        take = min(dup_target - added, max(1, int(round(len(batch) * ratio))))
        for _ in range(take):
            batch.append(dict(rng.choice(pool)))
            added += 1
    return added


def _arrival_order(rng, events: list[dict], lo: int, hi: int) -> None:
    """Reorder events in-place into arrival order: event time plus a
    uniform ``[lo, hi]`` seconds of out-of-order jitter."""
    jittered = [
        (e["event_time"] + rng.randrange(lo, hi + 1), i, e)
        for i, e in enumerate(events)
    ]
    jittered.sort(key=lambda item: (item[0], item[1]))
    events[:] = [e for _, _, e in jittered]


def _write_parquet_shard(path: Path, events: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    columns = {name: [e[name] for e in events] for name in SCHEMA.names}
    pq.write_table(pa.table(columns, schema=SCHEMA), path)


def _write_jsonl(path: Path, events: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        for e in events:
            fh.write(json.dumps(e, sort_keys=True) + "\n")


def generate(cfg: JobConfig) -> dict:
    """Generate the full sample dataset; returns the manifest."""
    rng = random.Random(cfg.seed)
    data_dir = cfg.abs_path(cfg.data_dir)
    hist_dir = data_dir / "historical"
    inc_dir = data_dir / "incremental"

    # ---- historical hourly shards -------------------------------------
    per_hour, remainder = divmod(cfg.historical_rows, HISTORICAL_HOURS)
    hist_shards: list[list[dict]] = []
    for hour in range(HISTORICAL_HOURS):
        target = per_hour + (1 if hour < remainder else 0)
        start = BASE_TIME + hour * 3600
        shard = _make_events(
            rng, target, f"H{hour:02d}-", lambda s=start: s + rng.randrange(3600)
        )
        hist_shards.append(shard)

    # duplicates inside the historical side: exact copies re-reported in
    # the same hourly shard as the original occurrence
    hist_dups = 0
    for shard in hist_shards:
        take = int(round(len(shard) * cfg.duplicate_ratio))
        for _ in range(take):
            shard.append(dict(rng.choice(shard)))
            hist_dups += 1

    shard_files: list[Path] = []
    lo, hi = cfg.disorder_seconds
    for hour_idx, shard in enumerate(hist_shards):
        _arrival_order(rng, shard, lo, hi)  # out-of-order inside the shard
        start = BASE_TIME + hour_idx * 3600
        stamp = time.gmtime(start)
        shard_path = (
            hist_dir
            / f"dt={stamp.tm_year:04d}-{stamp.tm_mon:02d}-{stamp.tm_mday:02d}"
            / f"hour={stamp.tm_hour:02d}"
            / f"part-{hour_idx:04d}.parquet"
        )
        _write_parquet_shard(shard_path, shard)
        shard_files.append(shard_path)

    # ---- incremental jsonl batches ------------------------------------
    inc_start = BASE_TIME + HISTORICAL_HOURS * 3600
    per_batch = cfg.incremental_rows // cfg.incremental_batches
    batches: list[list[dict]] = []
    for b in range(cfg.incremental_batches):
        arrival = inc_start + b * BATCH_INTERVAL_SECONDS
        lo, hi = cfg.disorder_seconds
        batch = _make_events(
            rng,
            per_batch,
            f"I{b:04d}-",
            lambda a=arrival: a - rng.randrange(lo, hi + 1),
        )
        batches.append(batch)

    inc_dups = _inject_duplicates(rng, batches, cfg.duplicate_ratio)
    for b, batch in enumerate(batches):
        _arrival_order(rng, batch, 0, BATCH_INTERVAL_SECONDS)
        _write_jsonl(inc_dir / f"batch_{b:04d}.jsonl", batch)

    manifest = {
        "seed": cfg.seed,
        "duplicate_ratio": cfg.duplicate_ratio,
        "disorder_seconds": list(cfg.disorder_seconds),
        "base_time": BASE_TIME,
        "hot_key": HOT_KEY,
        "hot_key_share": HOT_KEY_SHARE,
        "historical": {
            "shards": len(shard_files),
            "events": sum(
                pq.read_metadata(p).num_rows for p in shard_files
            ),
            "duplicates": hist_dups,
        },
        "incremental": {
            "batches": len(batches),
            "events": sum(len(b) for b in batches),
            "duplicates": inc_dups,
        },
    }
    (data_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest
