"""Controlled input fixtures for focused semantics tests."""

from __future__ import annotations

import json
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from typing import Any

import pyarrow as pa
import pyarrow.parquet as pq

from minute_backfill.generator import PAYMENT_FIELDS, REFUND_FIELDS
from minute_backfill.timeutil import format_time_us, minute_start_us, parse_time_us
from minute_backfill.util import sha256_file, write_json


def _to_us(value: int | str | datetime) -> int:
    if isinstance(value, int):
        return value
    return parse_time_us(value)


def _amount(value: Any) -> Decimal:
    return Decimal(str(value)).quantize(Decimal("0.01"))


def write_dataset(
    out_dir: Path,
    *,
    payments: list[dict[str, Any]],
    batches: list[dict[str, Any]],
    currency: str = "CNY",
) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    manifest = {
        "manifest_version": 1,
        "dataset": "test",
        "payments": {
            "format": "parquet",
            "manifest": "payments/shards.json",
            "fields": PAYMENT_FIELDS,
        },
        "refunds": {
            "format": "jsonl",
            "manifest": "refunds/batches.json",
            "fields": REFUND_FIELDS,
        },
    }
    write_json(out_dir / "manifest.json", manifest)

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
    by_day: dict[str, list[dict[str, Any]]] = {}
    for payment in payments:
        event_us = _to_us(payment["event_time"])
        seconds, micros = divmod(event_us, 1_000_000)
        dt = datetime.fromtimestamp(seconds, tz=timezone.utc).replace(microsecond=micros)
        day = dt.strftime("%Y-%m-%d")
        by_day.setdefault(day, []).append(
            {
                "payment_id": str(payment["payment_id"]),
                "merchant_id": str(payment.get("merchant_id", "M0001")),
                "event_time": dt,
                "amount": _amount(payment["amount"]),
                "currency": str(payment.get("currency", currency)),
                "event_type": "PAYMENT",
            }
        )
    shards: list[dict[str, Any]] = []
    for day, rows in sorted(by_day.items()):
        rel = Path("payments") / f"day={day}" / "part-00000.parquet"
        path = out_dir / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        pq.write_table(pa.Table.from_pylist(rows, schema=schema), path, compression="snappy")
        shards.append(
            {
                "shard_id": rel.as_posix(),
                "file": rel.as_posix(),
                "rows": len(rows),
                "sha256": sha256_file(path),
                "declared_event_time": format_time_us(
                    max(int(_to_us(payment["event_time"])) for payment in payments)
                ),
            }
        )
    write_json(out_dir / "payments" / "shards.json", {"shards": shards})

    batch_manifest: list[dict[str, Any]] = []
    for index, batch in enumerate(batches, start=1):
        batch_id = f"batch-{index:06d}"
        rel = Path("refunds") / f"{batch_id}.jsonl"
        path = out_dir / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("w", encoding="utf-8", newline="\n") as fh:
            for row in batch.get("rows", []):
                event_us = _to_us(row["event_time"])
                payload = {
                    "refund_id": str(row.get("refund_id", f"R{index:06d}")),
                    "payment_id": str(row["payment_id"]),
                    "merchant_id": str(row.get("merchant_id", "M0001")),
                    "event_time": format_time_us(event_us),
                    "amount": str(_amount(row["amount"])),
                    "currency": str(row.get("currency", currency)),
                    "event_type": "REFUND",
                }
                fh.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
        batch_manifest.append(
            {
                "batch_id": batch_id,
                "batch_index": index,
                "file": rel.as_posix(),
                "declared_event_time": format_time_us(_to_us(batch["declared_event_time"])),
                "rows": len(batch.get("rows", [])),
                "sha256": sha256_file(path),
            }
        )
    write_json(out_dir / "refunds" / "batches.json", {"batches": batch_manifest})
    return out_dir


def minute_base(value: str = "2026-11-01T00:00:00Z") -> int:
    return minute_start_us(parse_time_us(value))
