from __future__ import annotations

import hashlib
import json
import math
import random
import shutil
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

from .errors import DataError
from .timeutil import iso_dt


@dataclass(frozen=True)
class GeneratorConfig:
    seed: int = 20260101
    start: datetime = datetime(2026, 1, 1, tzinfo=timezone.utc)
    days: int = 3
    telemetry_rows: int = 80_000
    supplement_rows: int = 30_000
    batches: int = 96
    duplicate_ratio: float = 0.05
    late_ratio: float = 0.03
    vehicle_count: int = 200


def _canonical_json(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _vehicle_weights(count: int) -> list[float]:
    # Keep a deliberately skewed head of the fleet so the routing plan must
    # split at least one vehicle across multiple logical processing slots.
    fixed = [18.0, 11.0, 8.0, 6.0, 5.0, 4.0, 3.0, 2.5]
    weights = list(fixed[: min(count, len(fixed))])
    remaining = max(0, count - len(weights))
    if remaining:
        weights.extend(1.0 / math.sqrt(index + 1) for index in range(remaining))
    return weights


def _write_parquet_partition(rows: list[dict], target: Path) -> int:
    import pyarrow as pa
    import pyarrow.parquet as pq

    target.parent.mkdir(parents=True, exist_ok=True)
    schema = pa.schema(
        [
            pa.field("vehicle_id", pa.string()),
            pa.field("trip_id", pa.string()),
            pa.field("event_time", pa.timestamp("us")),
            pa.field("mileage_delta", pa.float64()),
            pa.field("energy_delta", pa.float64()),
            pa.field("event_id", pa.string()),
        ]
    )
    table = pa.Table.from_pylist(rows, schema=schema)
    pq.write_table(
        table,
        target,
        compression="snappy",
        use_dictionary=["vehicle_id", "trip_id"],
        row_group_size=16_384,
    )
    return table.num_rows


def generate_dataset(
    out_dir: str | Path,
    config: GeneratorConfig | None = None,
    *,
    overwrite: bool = False,
) -> Path:
    """Generate deterministic daily Parquet shards and shuffled supplement batches."""

    cfg = config or GeneratorConfig()
    if cfg.telemetry_rows <= 0 or cfg.supplement_rows <= 0 or cfg.batches <= 0:
        raise DataError("generator row and batch counts must be positive")
    if not 0 <= cfg.duplicate_ratio < 1 or not 0 <= cfg.late_ratio < 1:
        raise DataError("duplicate_ratio and late_ratio must be in [0, 1)")
    if cfg.vehicle_count < 1:
        raise DataError("vehicle_count must be positive")

    out = Path(out_dir)
    telemetry_dir = out / "telemetry"
    batches_dir = out / "supplement_batches"
    if out.exists() and overwrite:
        shutil.rmtree(out)
    if telemetry_dir.exists() or batches_dir.exists():
        raise DataError(f"dataset already exists under {out}; pass --overwrite to replace it")
    telemetry_dir.mkdir(parents=True, exist_ok=True)
    batches_dir.mkdir(parents=True, exist_ok=True)

    rng = random.Random(cfg.seed)
    start = cfg.start.astimezone(timezone.utc)
    end = start + timedelta(days=cfg.days)
    span_seconds = int((end - start).total_seconds())
    vehicles = [f"V{index:04d}" for index in range(1, cfg.vehicle_count + 1)]
    weights = _vehicle_weights(cfg.vehicle_count)

    trips_by_vehicle: dict[str, list[str]] = {}
    for vehicle in vehicles:
        trip_count = rng.randint(2, 5)
        trips_by_vehicle[vehicle] = [f"{vehicle}-T{index:02d}" for index in range(1, trip_count + 1)]

    telemetry: list[dict] = []
    for row_index in range(cfg.telemetry_rows):
        vehicle = rng.choices(vehicles, weights=weights, k=1)[0]
        trip = rng.choice(trips_by_vehicle[vehicle])
        offset_us = rng.randrange(span_seconds * 1_000_000)
        event_time = start + timedelta(microseconds=offset_us)
        mileage = round(rng.uniform(0.15, 6.0), 3)
        energy = round(rng.uniform(0.03, 1.4), 3)
        telemetry.append(
            {
                "vehicle_id": vehicle,
                "trip_id": trip,
                "event_time": event_time.replace(tzinfo=None),
                "mileage_delta": mileage,
                "energy_delta": energy,
                "event_id": f"T{row_index + 1:09d}",
            }
        )

    by_partition: dict[str, list[dict]] = defaultdict(list)
    for row in telemetry:
        by_partition[row["event_time"].date().isoformat()].append(row)

    partitions: list[dict] = []
    for day, rows in sorted(by_partition.items()):
        rows.sort(key=lambda item: (item["event_time"], item["event_id"]))
        relative_path = Path("telemetry") / f"partition_date={day}" / "part-00000.parquet"
        absolute_path = out / relative_path
        count = _write_parquet_partition(rows, absolute_path)
        partitions.append(
            {
                "partition_date": day,
                "path": relative_path.as_posix(),
                "rows": count,
                "sha256": _sha256_file(absolute_path),
            }
        )

    # Build supplements around each batch's availability time. The batches
    # advance monotonically; a small fraction is delayed by 30-900 seconds.
    duplicate_count = round(cfg.supplement_rows * cfg.duplicate_ratio)
    unique_count = cfg.supplement_rows - duplicate_count
    batch_interval = span_seconds / cfg.batches
    telemetry_by_time = sorted(telemetry, key=lambda item: item["event_time"])
    telemetry_times = [item["event_time"] for item in telemetry_by_time]
    unique_events: list[dict] = []
    for index in range(unique_count):
        batch_index = index % cfg.batches
        target = start + timedelta(seconds=(batch_index + 1) * batch_interval)
        # Pick a real telemetry trip active near the target so supplements only
        # reference keys that exist on the telemetry side.
        target_naive = target.replace(tzinfo=None)
        lo = max(0, _bisect_left(telemetry_times, target_naive - timedelta(minutes=60)))
        hi = min(len(telemetry_by_time), _bisect_left(telemetry_times, target_naive + timedelta(seconds=1)))
        if lo >= hi:
            lo = max(0, min(len(telemetry_by_time) - 1, int(index * len(telemetry_by_time) / unique_count)))
            hi = min(len(telemetry_by_time), lo + 1)
        base = telemetry_by_time[rng.randrange(lo, hi)]
        is_late = rng.random() < cfg.late_ratio
        delay_seconds = rng.uniform(30, 900) if is_late else rng.uniform(0, 30)
        event_time = target_naive - timedelta(seconds=delay_seconds)
        event_candidate = {
            "vehicle_id": base["vehicle_id"],
            "trip_id": base["trip_id"],
            "event_time": event_time,
            "mileage_delta": round(rng.uniform(0.02, 1.8), 3),
            "energy_delta": round(rng.uniform(0.01, 0.6), 3),
            "record_id": f"C{index + 1:09d}",
        }
        if is_late:
            event_candidate["_generated_delay_seconds"] = round(delay_seconds, 3)
        unique_events.append(event_candidate)

    batch_rows: dict[int, list[dict]] = defaultdict(list)
    for index, event in enumerate(unique_events):
        batch_index = index % cfg.batches
        batch_rows[batch_index].append(event)

    duplicate_candidates = list(unique_events)
    rng.shuffle(duplicate_candidates)
    for index in range(duplicate_count):
        source = duplicate_candidates[index]
        original_batch = unique_events.index(source) % cfg.batches
        target_batch = rng.randint(original_batch, cfg.batches - 1)
        batch_rows[target_batch].append(dict(source))

    batches: list[dict] = []
    for batch_index in range(cfg.batches):
        rows = batch_rows[batch_index]
        rng.shuffle(rows)
        batch_id = batch_index + 1
        relative_path = Path("supplement_batches") / f"batch_{batch_id:04d}.jsonl"
        absolute_path = out / relative_path
        with absolute_path.open("w", encoding="utf-8", newline="\n") as handle:
            for event in rows:
                serializable = dict(event)
                serializable["event_time"] = iso_dt(event["event_time"])
                handle.write(_canonical_json(serializable))
                handle.write("\n")
        available_at = start + timedelta(seconds=(batch_index + 1) * batch_interval)
        batches.append(
            {
                "batch_id": batch_id,
                "path": relative_path.as_posix(),
                "available_at": iso_dt(available_at),
                "rows": len(rows),
                "sha256": _sha256_file(absolute_path),
            }
        )

    manifest = {
        "format_version": 1,
        "seed": cfg.seed,
        "start": iso_dt(start),
        "end": iso_dt(end),
        "telemetry_rows": sum(item["rows"] for item in partitions),
        "supplement_rows": sum(item["rows"] for item in batches),
        "batch_count": len(batches),
        "duplicate_ratio_target": cfg.duplicate_ratio,
        "late_ratio_target": cfg.late_ratio,
        "telemetry_partitions": partitions,
        "supplement_batches": batches,
    }
    manifest_path = out / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return manifest_path


def _bisect_left(values: list[datetime], target: datetime) -> int:
    import bisect

    return bisect.bisect_left(values, target)

