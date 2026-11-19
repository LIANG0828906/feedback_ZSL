"""Input readers for hourly Parquet shards and JSON Lines batches."""

from __future__ import annotations

import json
import os

import pyarrow.parquet as pq

from .errors import InputError

REQUIRED_COLUMNS = (
    "event_id",
    "payment_id",
    "event_time_ms",
    "amount_cents",
    "event_type",
)


def _validate(rows: list[dict], source: str) -> list[dict]:
    for row in rows:
        missing = [c for c in REQUIRED_COLUMNS if c not in row]
        if missing:
            raise InputError(
                f"schema violation in {source}: missing {missing}",
                details={"source": source},
            )
        if not isinstance(row["event_time_ms"], int) or row["event_time_ms"] < 0:
            raise InputError(f"invalid event_time_ms in {source}")
        if not isinstance(row["amount_cents"], int):
            raise InputError(f"invalid amount_cents in {source}")
    return rows


def read_parquet_shard(data_dir: str, shard: dict) -> list[dict]:
    path = os.path.join(data_dir, shard["path"])
    try:
        table = pq.read_table(path)
    except (OSError, FileNotFoundError) as exc:
        raise InputError(
            f"cannot read parquet shard {shard['path']}: {exc}",
            details={"source": shard["path"]},
        ) from exc
    missing = [c for c in REQUIRED_COLUMNS if c not in table.column_names]
    if missing:
        raise InputError(
            f"schema violation in {shard['path']}: missing {missing}",
            details={"source": shard["path"]},
        )
    return _validate(table.to_pylist(), shard["path"])


def read_jsonl_batch(data_dir: str, batch: dict) -> list[dict]:
    path = os.path.join(data_dir, batch["path"])
    rows: list[dict] = []
    try:
        with open(path, "r", encoding="utf-8") as fh:
            for line_no, line in enumerate(fh, 1):
                line = line.strip()
                if not line:
                    continue
                try:
                    rows.append(json.loads(line))
                except json.JSONDecodeError as exc:
                    raise InputError(
                        f"invalid json on {batch['path']}:{line_no}: {exc}",
                        details={"source": batch["path"], "line": line_no},
                    ) from exc
    except (OSError, FileNotFoundError) as exc:
        raise InputError(
            f"cannot read batch {batch['path']}: {exc}",
            details={"source": batch["path"]},
        ) from exc
    return _validate(rows, batch["path"])


def key_counts_duckdb(data_dir: str, manifest: dict) -> dict[str, int]:
    """One cheap scan over every source file to count events per payment key."""

    import duckdb

    con = duckdb.connect()
    counts: dict[str, int] = {}
    parquet_paths = [
        os.path.join(data_dir, s["path"]) for s in manifest["history"]
    ]
    if parquet_paths:
        rows = con.execute(
            "SELECT payment_id, count(*) FROM read_parquet(?) GROUP BY 1",
            [parquet_paths],
        ).fetchall()
        for key, count in rows:
            counts[key] = counts.get(key, 0) + int(count)
    con.close()

    for batch in manifest["stream_batches"]:
        with open(
            os.path.join(data_dir, batch["path"]), "r", encoding="utf-8"
        ) as fh:
            for line in fh:
                if not line.strip():
                    continue
                key = json.loads(line)["payment_id"]
                counts[key] = counts.get(key, 0) + 1
    return counts
