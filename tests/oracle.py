"""Independent in-memory reference for the documented window semantics."""

from __future__ import annotations

import calendar
import json
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from typing import Any

import pyarrow.parquet as pq

MICROS_PER_MINUTE = 60_000_000


def _us(value: Any) -> int:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=timezone.utc)
        value = value.astimezone(timezone.utc)
        return calendar.timegm(value.utctimetuple()) * 1_000_000 + value.microsecond
    if isinstance(value, int):
        return value
    text = str(value)
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    parsed = parsed.astimezone(timezone.utc)
    return calendar.timegm(parsed.utctimetuple()) * 1_000_000 + parsed.microsecond


def _minor(value: Any) -> int:
    return int(Decimal(str(value)) * 100)


def _window(value: int) -> int:
    return value - value % MICROS_PER_MINUTE


def reference_run(data_dir: Path, allowed_lateness_seconds: int) -> dict[str, Any]:
    manifest = json.loads((data_dir / "manifest.json").read_text(encoding="utf-8"))
    shard_manifest = json.loads(
        (data_dir / manifest["payments"]["manifest"]).read_text(encoding="utf-8")
    )
    batch_manifest = json.loads(
        (data_dir / manifest["refunds"]["manifest"]).read_text(encoding="utf-8")
    )
    canonical: dict[tuple[str, str, int], dict[str, Any]] = {}
    key_windows: set[tuple[str, int]] = set()
    events_seen = 0

    def canonicalize(event: dict[str, Any], watermark: int | None) -> tuple[int, int, int]:
        nonlocal events_seen
        events_seen += 1
        side = event["side"]
        payment_id = event["payment_id"]
        event_time = event["event_time"]
        window_start = _window(event_time)
        key = (side, payment_id, window_start)
        existing = canonical.get(key)
        closed = watermark is not None and window_start + MICROS_PER_MINUTE <= watermark
        if closed:
            if existing is not None and event_time >= existing["event_time"]:
                return 1, 1, 0
            return 1, 0, 0
        if existing is None:
            canonical[key] = event
            key_windows.add((payment_id, window_start))
            return 0, 0, 0
        if event_time < existing["event_time"]:
            canonical[key] = event
            return 0, 1, 0
        return 0, 1, 0

    late_total = 0
    dedup_dropped = 0
    for shard in shard_manifest["shards"]:
        table = pq.read_table(data_dir / shard["file"])
        for row in table.to_pylist():
            late, dropped, _ = canonicalize(
                {
                    "side": "PAYMENT",
                    "payment_id": row["payment_id"],
                    "event_time": _us(row["event_time"]),
                    "amount": _minor(row["amount"]),
                },
                None,
            )
            late_total += late
            dedup_dropped += dropped

    outputs: dict[int, dict[str, Any]] = {}
    closed_minutes: set[int] = set()
    watermark: int | None = None
    batches = sorted(batch_manifest["batches"], key=lambda item: item["batch_index"])

    def close(limit: int | None, reason: str) -> None:
        candidates = {}
        for payment_id, window_start in key_windows:
            if window_start in closed_minutes:
                continue
            if limit is not None and window_start + MICROS_PER_MINUTE > limit:
                continue
            candidates.setdefault(window_start, set()).add(payment_id)
        for window_start in sorted(candidates):
            pay_count = pay_amount = refund_count = refund_amount = 0
            for payment_id in candidates[window_start]:
                pay = canonical.get(("PAYMENT", payment_id, window_start))
                refund = canonical.get(("REFUND", payment_id, window_start))
                if pay is None:
                    continue
                pay_count += 1
                pay_amount += int(pay["amount"])
                if refund is not None and int(refund["event_time"]) >= int(pay["event_time"]):
                    refund_count += 1
                    refund_amount += int(refund["amount"])
            if pay_count:
                outputs[window_start] = {
                    "window_start": window_start,
                    "pay_count": pay_count,
                    "pay_amount_minor": pay_amount,
                    "refund_count": refund_count,
                    "refund_amount_minor": refund_amount,
                    "net_minor": pay_amount - refund_amount,
                    "close_reason": reason,
                }
            closed_minutes.add(window_start)

    lateness = allowed_lateness_seconds * 1_000_000
    for batch in batches:
        path = data_dir / batch["file"]
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            raw = json.loads(line)
            late, dropped, _ = canonicalize(
                {
                    "side": "REFUND",
                    "payment_id": raw["payment_id"],
                    "event_time": _us(raw["event_time"]),
                    "amount": _minor(raw["amount"]),
                },
                watermark,
            )
            late_total += late
            dedup_dropped += dropped
        declared = _us(batch["declared_event_time"])
        candidate = declared - lateness
        watermark = candidate if watermark is None else max(watermark, candidate)
        close(watermark, "watermark")
    close(None, "end_of_input")

    return {
        "windows": [outputs[key] for key in sorted(outputs)],
        "dedup_dropped": dedup_dropped,
        "late_total": late_total,
        "watermark": watermark,
        "canonical_events": len(canonical),
        "closed_watermark_windows": sum(
            1 for output in outputs.values() if output["close_reason"] == "watermark"
        ),
        "closed_end_windows": sum(
            1 for output in outputs.values() if output["close_reason"] == "end_of_input"
        ),
    }
