"""Deterministic local workload generator.

Builds:
  - batch side: hourly partitioned Parquet files (with injected duplicates)
  - stream side: NB json (ndjson) batches where each logical record appears
    multiple times; duplicate appearances always arrive at or after the
    primary appearance so late-arrival semantics can be exercised.
"""

import datetime as dt
import json
import os

import numpy as np
import pyarrow as pa
import pyarrow.parquet as pq

from . import manifest as manifest_mod

MS_PER_HOUR = 3600_000


def _day_start_ms(day_str):
    day = dt.date.fromisoformat(day_str)
    return int(dt.datetime(day.year, day.month, day.day,
                           tzinfo=dt.timezone.utc).timestamp() * 1000)


def generate(cfg):
    gen_cfg = cfg["generator"]
    seed = cfg["seed"]
    rng = np.random.default_rng(seed)
    data_dir = os.path.join(cfg["_base_dir"], cfg["data_dir"])
    batch_dir = os.path.join(data_dir, "batch")
    stream_dir = os.path.join(data_dir, "stream")
    os.makedirs(batch_dir, exist_ok=True)
    os.makedirs(stream_dir, exist_ok=True)

    fleets = gen_cfg["fleets"]
    vehicles_per_fleet = gen_cfg["vehicles_per_fleet"]
    skew_n = gen_cfg["skew_vehicles_per_fleet"]
    skew_share = gen_cfg["skew_share"]
    total_records = gen_cfg["batch_records"]
    hours = gen_cfg["hours"]
    day_ms = _day_start_ms(gen_cfg["day"])
    span_ms = hours * MS_PER_HOUR

    per_fleet = total_records // len(fleets)
    skew_rows = int(per_fleet * skew_share) // skew_n
    normal_rows = (per_fleet - skew_rows * skew_n) // (vehicles_per_fleet - skew_n)

    # vehicle_id -> list of (event_time, speed_x10, fuel_ml, distance_m)
    vehicles = {}
    order = []
    for fleet in fleets:
        for idx in range(vehicles_per_fleet):
            vid = f"{fleet}-{idx:04d}"
            n = skew_rows if idx < skew_n else normal_rows
            secs = rng.permutation(hours * 3600)[:n].astype(np.int64)
            jitter = idx % 1000
            ets = day_ms + secs * 1000 + jitter
            speed = rng.integers(0, 1200, n)
            fuel = rng.integers(0, 5000, n)
            dist = rng.integers(0, 30000, n)
            vehicles[vid] = list(zip(ets.tolist(), speed.tolist(),
                                     fuel.tolist(), dist.tolist()))
            order.append(vid)

    # ---- batch side: hourly parquet partitions (+ injected duplicates) ----
    by_hour = {h: [] for h in range(hours)}
    flat = []
    for vid in order:
        for et, sp, fu, di in vehicles[vid]:
            rec = (vid, et, sp, fu, di)
            flat.append(rec)
            by_hour[(et - day_ms) // MS_PER_HOUR].append(rec)
    dup_n = gen_cfg["batch_duplicate_records"]
    dup_idx = rng.choice(len(flat), size=min(dup_n, len(flat)), replace=False)
    for i in dup_idx:
        rec = flat[int(i)]
        by_hour[(rec[1] - day_ms) // MS_PER_HOUR].append(rec)

    for h in range(hours):
        recs = by_hour[h]
        table = pa.table({
            "vehicle_id": pa.array([r[0] for r in recs], type=pa.string()),
            "event_time": pa.array([r[1] for r in recs], type=pa.int64()),
            "speed_x10": pa.array([r[2] for r in recs], type=pa.int32()),
            "fuel_ml": pa.array([r[3] for r in recs], type=pa.int32()),
            "distance_m": pa.array([r[4] for r in recs], type=pa.int32()),
        })
        part_dir = os.path.join(batch_dir, f"hour={h:02d}")
        os.makedirs(part_dir, exist_ok=True)
        pq.write_table(table, os.path.join(part_dir, "part-000.parquet"))

    # ---- stream side: duplicated appearances across batches ----
    n_batches = gen_cfg["stream_batches"]
    avg_app = gen_cfg["stream_avg_appearances"]
    batches = [[] for _ in range(n_batches)]
    for vid in order:
        for et, sp, fu, di in vehicles[vid]:
            hour = (et - day_ms) // MS_PER_HOUR
            primary = int(min(hour * 2 + rng.integers(0, 2), n_batches - 1))
            rec = {"vehicle_id": vid, "event_time": int(et),
                   "speed_x10": int(sp), "fuel_ml": int(fu),
                   "distance_m": int(di)}
            batches[primary].append(rec)
            n_extra = int(avg_app - 1) + int(rng.random() < (avg_app - 1) % 1)
            if n_extra > 0:
                extra = rng.integers(primary, n_batches, n_extra)
                for b in extra:
                    batches[int(b)].append(rec)

    total_stream = 0
    for b, recs in enumerate(batches):
        perm = rng.permutation(len(recs))
        path = os.path.join(stream_dir, f"batch_{b:03d}.json")
        with open(path, "w", encoding="utf-8") as fh:
            for i in perm:
                fh.write(json.dumps(recs[int(i)], separators=(",", ":")))
                fh.write("\n")
        total_stream += len(recs)

    manifest_mod.write_manifest(data_dir, gen_cfg, seed)
    return {
        "batch_records": len(flat) + len(dup_idx),
        "stream_records": total_stream,
        "vehicles": len(order),
        "batches": n_batches,
    }
