"""Generate deterministic mixed CSV/Parquet sample data and metadata."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

import numpy as np
import pyarrow as pa
import pyarrow.compute as pc
import pyarrow.parquet as pq
from pyarrow import csv as pacsv


def _write_table(table: pa.Table, path: Path, fmt: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if fmt == "parquet":
        pq.write_table(
            table,
            path,
            compression="zstd",
            use_dictionary=True,
            write_statistics=True,
        )
    else:
        pacsv.write_csv(
            table,
            path,
            write_options=pacsv.WriteOptions(
                include_header=True,
                delimiter=",",
                quoting_style="needed",
            ),
        )


def _column_metadata(table: pa.Table) -> dict[str, dict[str, int]]:
    out: dict[str, dict[str, int]] = {}
    for name in table.column_names:
        arr = table[name]
        out[name] = {
            "distinct_count": max(1, int(pc.count_distinct(arr).as_py())),
            "null_count": int(arr.null_count),
        }
    return out


def generate(output: Path, seed: int, fact_rows: int, dim_rows: int) -> dict[str, str]:
    rng = np.random.default_rng(seed)
    store_rows = int(dim_rows * 0.6)
    product_rows = dim_rows - store_rows
    if store_rows < 60 or product_rows < 30:
        raise ValueError("dim_rows must be large enough for 60 stores and 30 products")

    # Fact table: 30,000 rows, deliberately using a small referenced dimension
    # subset so the pushed-down partial aggregate is materially smaller.
    store_ids = rng.integers(1, 61, size=fact_rows, dtype=np.int64)
    product_ids = rng.integers(1, 31, size=fact_rows, dtype=np.int64)
    day_offsets = rng.integers(0, 3, size=fact_rows, dtype=np.int64)
    second_offsets = rng.integers(0, 86400, size=fact_rows, dtype=np.int64)
    base = np.datetime64("2025-01-01T00:00:00", "us")
    event_ts = (
        base
        + day_offsets.astype("timedelta64[D]")
        + second_offsets.astype("timedelta64[s]")
    ).astype("datetime64[us]")
    amounts = rng.integers(-700, 1001, size=fact_rows, dtype=np.int64)
    quantities = rng.integers(-20, 21, size=fact_rows, dtype=np.int64)
    customers = rng.integers(1, 5001, size=fact_rows, dtype=np.int64)
    fact = pa.table(
        {
            "sale_id": pa.array(np.arange(1, fact_rows + 1, dtype=np.int64)),
            "event_ts": pa.array(event_ts, type=pa.timestamp("us")),
            "store_id": pa.array(store_ids),
            "product_id": pa.array(product_ids),
            "customer_id": pa.array(customers),
            "amount": pa.array(amounts),
            "qty": pa.array(quantities),
        }
    )

    regions = np.array(
        ["North", "South", "East", "West", "Central", "Northeast", "Southwest", None],
        dtype=object,
    )
    store_region_idx = rng.integers(0, len(regions), size=store_rows, dtype=np.int64)
    store_regions = [regions[idx] for idx in store_region_idx]
    # The referenced 1..60 window has a deterministic active/inactive mix.
    active = (np.arange(1, store_rows + 1) % 5 != 0).astype(np.int64)
    store = pa.table(
        {
            "store_id": pa.array(np.arange(1, store_rows + 1, dtype=np.int64)),
            "region": pa.array(store_regions, type=pa.string()),
            "active": pa.array(active),
            "store_name": pa.array([f"store-{idx:05d}" for idx in range(1, store_rows + 1)]),
        }
    )

    categories = np.array(
        ["Books", "Electronics", "Food", "Home", "Sports", "Garden", None],
        dtype=object,
    )
    product_category_idx = rng.integers(
        0, len(categories), size=product_rows, dtype=np.int64
    )
    product_categories = [categories[idx] for idx in product_category_idx]
    product = pa.table(
        {
            "product_id": pa.array(np.arange(1, product_rows + 1, dtype=np.int64)),
            "category": pa.array(product_categories, type=pa.string()),
            "price": pa.array(
                rng.integers(100, 10000, size=product_rows, dtype=np.int64)
            ),
            "product_name": pa.array(
                [f"product-{idx:05d}" for idx in range(1, product_rows + 1)]
            ),
        }
    )

    key_count = 5000
    duplicate_count = fact_rows if fact_rows == 15000 else 10000
    event_base = np.datetime64("2025-02-01T00:00:00", "us")
    key_seconds = np.sort(
        rng.integers(0, 7200, size=key_count, dtype=np.int64)
    )
    first_rows: list[dict[str, Any]] = []
    for idx in range(key_count):
        key = None if idx in {137, 811, 2027} else f"event-{idx:05d}"
        first_rows.append(
            {
                "event_key": key,
                "event_ts": event_base + np.timedelta64(int(key_seconds[idx]), "s"),
                "user_id": int(idx % 2000 + 1),
                "payload": f"payload-{idx:05d}",
            }
        )
    duplicate_rows: list[dict[str, Any]] = []
    for idx in range(duplicate_count - key_count):
        key_idx = idx % key_count
        key = None if key_idx in {137, 811, 2027} else f"event-{key_idx:05d}"
        duplicate_rows.append(
            {
                "event_key": key,
                "event_ts": event_base
                + np.timedelta64(
                    int(key_seconds[key_idx]) + 60 + (idx % 600), "s"
                ),
                "user_id": int(key_idx % 2000 + 1),
                "payload": f"late-{idx:05d}",
            }
        )
    stream_rows = first_rows + duplicate_rows
    stream = pa.table(
        {
            "event_key": pa.array(
                [row["event_key"] for row in stream_rows], type=pa.string()
            ),
            "event_ts": pa.array(
                [row["event_ts"] for row in stream_rows], type=pa.timestamp("us")
            ),
            "user_id": pa.array(
                [row["user_id"] for row in stream_rows], type=pa.int64()
            ),
            "payload": pa.array(
                [row["payload"] for row in stream_rows], type=pa.string()
            ),
            "received_seq": pa.array(
                np.arange(1, len(stream_rows) + 1, dtype=np.int64)
            ),
        }
    )

    output.mkdir(parents=True, exist_ok=True)
    _write_table(fact, output / "fact_sales.parquet", "parquet")
    _write_table(store, output / "dim_store.csv", "csv")
    _write_table(product, output / "dim_product.parquet", "parquet")
    _write_table(stream, output / "stream_events.csv", "csv")

    schema: dict[str, Any] = {
        "schema_version": 1,
        "generator": {"seed": seed, "fact_rows": fact_rows, "dim_rows": dim_rows},
        "tables": {
            "fact_sales": {
                "files": ["fact_sales.parquet"],
                "format": "parquet",
                "columns": {
                    "sale_id": "BIGINT",
                    "event_ts": "TIMESTAMP",
                    "store_id": "BIGINT",
                    "product_id": "BIGINT",
                    "customer_id": "BIGINT",
                    "amount": "BIGINT",
                    "qty": "BIGINT",
                },
                "primary_key": ["sale_id"],
                "row_count": fact.num_rows,
                "stats": _column_metadata(fact),
            },
            "dim_store": {
                "files": ["dim_store.csv"],
                "format": "csv",
                "columns": {
                    "store_id": "BIGINT",
                    "region": "VARCHAR",
                    "active": "BIGINT",
                    "store_name": "VARCHAR",
                },
                "primary_key": ["store_id"],
                "row_count": store.num_rows,
                "stats": _column_metadata(store),
            },
            "dim_product": {
                "files": ["dim_product.parquet"],
                "format": "parquet",
                "columns": {
                    "product_id": "BIGINT",
                    "category": "VARCHAR",
                    "price": "BIGINT",
                    "product_name": "VARCHAR",
                },
                "primary_key": ["product_id"],
                "row_count": product.num_rows,
                "stats": _column_metadata(product),
            },
            "stream_events": {
                "files": ["stream_events.csv"],
                "format": "csv",
                "columns": {
                    "event_key": "VARCHAR",
                    "event_ts": "TIMESTAMP",
                    "user_id": "BIGINT",
                    "payload": "VARCHAR",
                    "received_seq": "BIGINT",
                },
                "primary_key": ["received_seq"],
                "row_count": stream.num_rows,
                "stats": _column_metadata(stream),
            },
        },
        "stream": {
            "table": "stream_events",
            "event_time_column": "event_ts",
            "ingest_order_column": "received_seq",
            "dedup_keys": ["event_key"],
            "allowed_lateness_seconds": 30,
        },
    }
    (output / "schema.json").write_text(
        json.dumps(schema, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )
    return {
        "fact_rows": str(fact.num_rows),
        "dim_rows": str(store.num_rows + product.num_rows),
        "stream_rows": str(stream.num_rows),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default="data")
    parser.add_argument("--seed", type=int, default=20261119)
    parser.add_argument("--fact-rows", type=int, default=30000)
    parser.add_argument("--dim-rows", type=int, default=20000)
    args = parser.parse_args()
    print(json.dumps(generate(Path(args.output), args.seed, args.fact_rows, args.dim_rows), sort_keys=True))


if __name__ == "__main__":
    main()

