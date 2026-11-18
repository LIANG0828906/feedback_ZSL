"""Batch (Parquet) and stream (NDJSON) ingestion with late-record semantics."""

import json
import os

import pyarrow.parquet as pq

from . import manifest as manifest_mod

MS_PER_HOUR = 3600_000


def read_batch(data_dir, manifest, fleet_prefix):
    """Read hourly parquet partitions; returns (records, input_count)."""
    batch_dir = os.path.join(data_dir, manifest["batch"]["dir"])
    records = []
    input_count = 0
    for entry in sorted(os.listdir(batch_dir)):
        part_dir = os.path.join(batch_dir, entry)
        if not os.path.isdir(part_dir):
            continue
        for fname in sorted(os.listdir(part_dir)):
            if not fname.endswith(".parquet"):
                continue
            table = pq.read_table(os.path.join(part_dir, fname))
            manifest_mod.validate_parquet_schema(table.schema.names)
            cols = table.to_pydict()
            for i in range(table.num_rows):
                vid = cols["vehicle_id"][i]
                if vid.startswith(fleet_prefix + "-"):
                    input_count += 1
                    records.append((vid, cols["event_time"][i],
                                    cols["speed_x10"][i], cols["fuel_ml"][i],
                                    cols["distance_m"][i]))
    return records, input_count


def read_stream(data_dir, manifest, fleet_prefix, window_ms, allowed_lateness_ms):
    """Read stream batches in arrival order.

    The watermark is a property of the shared stream (computed over all
    records of a batch, before tenant filtering). Records arriving after
    their event-time window closed are counted as late and dropped.
    Returns (on_time_records, input_count, late_count).
    """
    stream_dir = os.path.join(data_dir, manifest["stream"]["dir"])
    n_batches = manifest["stream"]["batches"]
    pattern = manifest["stream"]["file_pattern"]
    records = []
    input_count = 0
    late_count = 0
    watermark = None
    validated = False
    for b in range(n_batches):
        path = os.path.join(stream_dir, pattern.format(b))
        batch_max = None
        pending = []
        with open(path, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                obj = json.loads(line)
                if not validated:
                    manifest_mod.validate_stream_record(obj.keys())
                    validated = True
                et = int(obj["event_time"])
                if batch_max is None or et > batch_max:
                    batch_max = et
                vid = obj["vehicle_id"]
                if vid.startswith(fleet_prefix + "-"):
                    input_count += 1
                    pending.append((vid, et, int(obj["speed_x10"]),
                                    int(obj["fuel_ml"]), int(obj["distance_m"])))
        for rec in pending:
            if (watermark is not None
                    and rec[1] + window_ms - (rec[1] % window_ms) <= watermark):
                late_count += 1
            else:
                records.append(rec)
        if batch_max is not None:
            candidate = batch_max - allowed_lateness_ms
            watermark = candidate if watermark is None else max(watermark, candidate)
    return records, input_count, late_count
