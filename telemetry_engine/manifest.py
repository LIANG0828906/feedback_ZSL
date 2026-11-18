"""Dataset manifest: schema declaration and validation."""

import json
import os

from . import errors

SCHEMA_FIELDS = [
    {"name": "vehicle_id", "type": "string"},
    {"name": "event_time", "type": "int64"},
    {"name": "speed_x10", "type": "int32"},
    {"name": "fuel_ml", "type": "int32"},
    {"name": "distance_m", "type": "int32"},
]

FIELD_NAMES = [f["name"] for f in SCHEMA_FIELDS]


class SchemaViolation(Exception):
    code = errors.E_SCHEMA_VIOLATION


def manifest_path(data_dir):
    return os.path.join(data_dir, "manifest.json")


def write_manifest(data_dir, generator_cfg, seed):
    manifest = {
        "version": 1,
        "seed": seed,
        "schema": {"fields": SCHEMA_FIELDS},
        "batch": {
            "format": "parquet",
            "dir": "batch",
            "partition_pattern": "hour={HH}",
            "partitions": generator_cfg["hours"],
        },
        "stream": {
            "format": "ndjson",
            "dir": "stream",
            "batches": generator_cfg["stream_batches"],
            "file_pattern": "batch_{:03d}.json",
        },
        "generator": generator_cfg,
    }
    with open(manifest_path(data_dir), "w", encoding="utf-8") as fh:
        json.dump(manifest, fh, indent=2, sort_keys=True)
    return manifest


def load_manifest(data_dir):
    path = manifest_path(data_dir)
    if not os.path.exists(path):
        raise SchemaViolation(f"manifest not found: {path}")
    with open(path, "r", encoding="utf-8") as fh:
        manifest = json.load(fh)
    declared = [f["name"] for f in manifest.get("schema", {}).get("fields", [])]
    if declared != FIELD_NAMES:
        raise SchemaViolation(
            f"manifest schema {declared} does not match engine schema {FIELD_NAMES}")
    return manifest


def validate_parquet_schema(schema_names):
    if list(schema_names) != FIELD_NAMES:
        raise SchemaViolation(
            f"parquet schema {list(schema_names)} does not match manifest {FIELD_NAMES}")


def validate_stream_record(keys):
    for name in FIELD_NAMES:
        if name not in keys:
            raise SchemaViolation(f"stream record missing field: {name}")
