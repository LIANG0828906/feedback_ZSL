"""Fixed-seed generator for the 96-batch JSON Lines workload."""

from __future__ import annotations

import json
import math
import random
from collections import Counter
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from .util import canonical_json, fingerprint, iso, ts_iso, utc_now, write_json


BASE_TIME = datetime(2026, 1, 1, 0, 0, 0, tzinfo=timezone.utc)


@dataclass
class GeneratorConfig:
    out_dir: str
    seed: int = 3202026
    rows: int = 80000
    batches: int = 96
    vehicles: int = 1200
    trips_per_vehicle: int = 4
    duplicate_ratio: float = 0.04
    delayed_ratio: float = 0.03
    hot_vehicle_share: float = 0.12
    base_time: datetime = BASE_TIME
    dedupe_keys: tuple[str, ...] = ("vehicle_id", "trip_id", "event_time", "event_type")


def generate(config: GeneratorConfig) -> dict[str, Any]:
    out = Path(config.out_dir)
    events_dir = out / "events"
    events_dir.mkdir(parents=True, exist_ok=True)
    for old in events_dir.glob("*.jsonl"):
        old.unlink()
    rng = random.Random(config.seed)
    duplicate_target = int(round(config.rows * config.duplicate_ratio))
    primary_target = config.rows - duplicate_target
    if primary_target <= 0:
        raise ValueError("rows must be larger than the duplicate target")
    per_batch = [primary_target // config.batches] * config.batches
    for i in range(primary_target % config.batches):
        per_batch[i] += 1

    delayed_indices = set(
        rng.sample(range(primary_target), int(round(primary_target * config.delayed_ratio)))
    )
    moved_indices = set(
        rng.sample(range(primary_target), max(1, int(round(primary_target * 0.01))))
    )
    hot_vehicle = "V0001"
    rows_by_batch: list[list[dict[str, Any]]] = [[] for _ in range(config.batches)]
    primary_fps: set[str] = set()
    idx = 0
    for batch_no, count in enumerate(per_batch):
        declared = int(config.base_time.timestamp()) + batch_no * 60
        for _ in range(count):
            # A hot vehicle exercises balanced routing; the remaining vehicles
            # are long-tailed so the 40% shard cap matters.
            if rng.random() < config.hot_vehicle_share:
                vid = hot_vehicle
            else:
                vid = f"V{rng.randint(2, config.vehicles):04d}"
            trip_no = rng.randint(1, config.trips_per_vehicle)
            trip_id = f"{vid}-T{trip_no:02d}"
            event_type = "payment" if rng.random() < 0.62 else "backfill"
            if rng.random() < 0.03:
                event_type = "normal"
            event_time = declared + rng.randint(0, 59)
            delivery_delay = 0
            delayed = idx in delayed_indices
            if delayed:
                delivery_delay = rng.randint(30, 600)
                event_time = declared - delivery_delay
            # The natural dedupe key must identify a distinct primary report.
            # Avoid accidental collisions so the 4% figure describes the
            # deliberately duplicated reports.
            for bump in range(180):
                probe = {
                    "vehicle_id": vid,
                    "trip_id": trip_id,
                    "event_time": ts_iso(event_time + bump),
                    "event_type": event_type,
                }
                if fingerprint(probe, config.dedupe_keys) not in primary_fps:
                    event_time += bump
                    primary_fps.add(fingerprint(probe, config.dedupe_keys))
                    break
            row = {
                "event_id": f"E{idx:08d}",
                "vehicle_id": vid,
                "trip_id": trip_id,
                "event_time": ts_iso(event_time),
                "mileage_delta": round(rng.uniform(0.0, 8.0), 3),
                "battery_delta": round(rng.uniform(0.0, 3.0), 3),
                "speed": round(rng.uniform(0.0, 120.0), 2),
                "event_type": event_type,
                "source_side": "payment" if event_type == "payment" else "backfill",
                "odometer": round(rng.uniform(0.0, 300000.0), 3),
                "soc": round(rng.uniform(5.0, 100.0), 2),
                "_delivery_delay_s": delivery_delay,
                "_batch_declared": ts_iso(declared),
            }
            target_batch = batch_no
            if idx in moved_indices and batch_no + 1 < config.batches:
                target_batch = batch_no + 1
                row["_moved_batch"] = True
            rows_by_batch[target_batch].append(row)
            idx += 1

    # Duplicate exact natural keys, sometimes in a later minute batch.  The
    # copy includes event_id; dedupe is configured by the natural key.
    all_primaries = [row for batch in rows_by_batch for row in batch]
    duplicate_rows: list[dict[str, Any]] = []
    if all_primaries:
        for _ in range(duplicate_target):
            original = all_primaries[rng.randrange(len(all_primaries))]
            copy = dict(original)
            copy["_duplicate"] = True
            current = _batch_index_from_declared(copy.get("_batch_declared"), config)
            target = min(config.batches - 1, current + rng.choice([0, 0, 1, 2]))
            rows_by_batch[target].append(copy)
            duplicate_rows.append(copy)

    for batch in rows_by_batch:
        rng.shuffle(batch)

    manifest_batches: list[dict[str, Any]] = []
    total_rows = 0
    for batch_no, batch in enumerate(rows_by_batch):
        declared = int(config.base_time.timestamp()) + batch_no * 60
        path = events_dir / f"batch_{batch_no:06d}.jsonl"
        with open(path, "w", encoding="utf-8", newline="\n") as fh:
            for row in batch:
                fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True))
                fh.write("\n")
        total_rows += len(batch)
        times = [int(_epoch(row["event_time"])) for row in batch]
        manifest_batches.append(
            {
                "batch_id": batch_no,
                "batch_no": batch_no,
                "file": f"events/{path.name}",
                "declared_event_time": ts_iso(declared),
                "rows": len(batch),
                "min_event_time": ts_iso(min(times)),
                "max_event_time": ts_iso(max(times)),
            }
        )

    seen: set[str] = set()
    duplicate_count = 0
    delayed_count = 0
    normal_count = 0
    inversion_rows = 0
    cross_batch_older = 0
    windows: set[tuple[str, int]] = set()
    vehicles: Counter[str] = Counter()
    for batch_no, batch in enumerate(rows_by_batch):
        declared = int(config.base_time.timestamp()) + batch_no * 60
        prev: int | None = None
        for row in batch:
            fp = fingerprint(row, config.dedupe_keys)
            if fp in seen:
                duplicate_count += 1
            else:
                seen.add(fp)
            ts = int(_epoch(row["event_time"]))
            if row.get("_delivery_delay_s", 0):
                delayed_count += 1
            if row.get("event_type") == "normal":
                normal_count += 1
            if prev is not None and ts < prev:
                inversion_rows += 1
            prev = ts
            if ts < declared:
                cross_batch_older += 1
            vid = str(row["vehicle_id"])
            vehicles[vid] += 1
            windows.add((vid, (ts // 300) * 300))

    stats = {
        "seed": config.seed,
        "rows": total_rows,
        "primaries": primary_target,
        "duplicate_rows": duplicate_count,
        "duplicate_ratio": duplicate_count / total_rows if total_rows else 0.0,
        "delayed_rows": delayed_count,
        "delayed_ratio": delayed_count / total_rows if total_rows else 0.0,
        "normal_rows": normal_count,
        "in_batch_inversions": inversion_rows,
        "in_batch_inversion_ratio": inversion_rows / total_rows if total_rows else 0.0,
        "cross_batch_older_rows": cross_batch_older,
        "cross_batch_older_ratio": cross_batch_older / total_rows if total_rows else 0.0,
        "unique_rows": len(seen),
        "unique_vehicles": len(vehicles),
        "window_keys": len(windows),
        "hot_vehicle": hot_vehicle,
        "hot_vehicle_rows": vehicles.get(hot_vehicle, 0),
        "hot_vehicle_share": (vehicles.get(hot_vehicle, 0) / total_rows) if total_rows else 0.0,
        "max_vehicle_rows": max(vehicles.values()) if vehicles else 0,
        "batch_count": config.batches,
    }
    manifest = {
        "format": "iovflow-events-v1",
        "seed": config.seed,
        "generated_at": iso(),
        "rows": total_rows,
        "batches_count": config.batches,
        "base_time": ts_iso(int(config.base_time.timestamp())),
        "declared_interval_seconds": 60,
        "dedupe_keys": list(config.dedupe_keys),
        "stats": stats,
        "batches": manifest_batches,
    }
    validation = {
        "rows_exact": total_rows == config.rows,
        "batch_count_exact": len(manifest_batches) == config.batches,
        "duplicate_ratio_ok": abs(stats["duplicate_ratio"] - config.duplicate_ratio) <= 0.005,
        "delayed_ratio_ok": abs(stats["delayed_ratio"] - config.delayed_ratio) <= 0.005,
        "out_of_order_present": (
            stats["in_batch_inversions"] > 0 or stats["cross_batch_older_rows"] > 0
        ),
    }
    validation["ok"] = all(validation.values())
    manifest["validation"] = validation
    if not validation["ok"]:
        raise ValueError(f"generated workload failed validation: {validation}")
    write_json(out / "manifest.json", manifest)
    return manifest


def _epoch(value: Any) -> int:
    from .util import parse_time

    return parse_time(value)


def _batch_index_from_declared(value: Any, config: GeneratorConfig) -> int:
    if value is None:
        return 0
    return max(
        0,
        min(
            config.batches - 1,
            int((_epoch(value) - int(config.base_time.timestamp())) // 60),
        ),
    )

