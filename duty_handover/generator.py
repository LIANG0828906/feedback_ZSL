"""Deterministic sample and handover generator."""

from __future__ import annotations

import copy
import csv
import hashlib
import json
import random
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from .policy import DEFAULT_POLICY


UTC = timezone.utc
START = datetime(2026, 11, 18, 0, 0, tzinfo=UTC)


def _json_write(path: Path, payload: Any) -> None:
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
        newline="\n",
    )


def _iso(moment: datetime) -> str:
    return moment.astimezone(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def _services() -> list[dict[str, Any]]:
    return [
        {
            "name": "payment-gateway",
            "total": 1000,
            "target": 0.999,
            "latency_threshold_ms": 300.0,
            "latency_base": 185.0,
            "failure_period": 4,
            "failure_offset": 0,
        },
        {
            "name": "order-api",
            "total": 1000,
            "target": 0.999,
            "latency_threshold_ms": 260.0,
            "latency_base": 155.0,
            "failure_period": 5,
            "failure_offset": 1,
        },
        {
            "name": "checkout-ui",
            "total": 800,
            "target": 0.999,
            "latency_threshold_ms": 220.0,
            "latency_base": 95.0,
            "failure_period": 5,
            "failure_offset": 2,
        },
    ]


def generate_samples(seed: int = 304) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    rows: list[dict[str, Any]] = []
    services = _services()
    for minute in range(1440):
        timestamp = START + timedelta(minutes=minute)
        for service in services:
            name = str(service["name"])
            total = int(service["total"]) + rng.randint(-12, 12)
            failed = (
                1 if minute % int(service["failure_period"]) == int(service["failure_offset"]) else 0
            )
            p99 = float(service["latency_base"]) + rng.uniform(-8.0, 8.0)
            if name == "payment-gateway" and 870 <= minute <= 899:
                failed = 3
                p99 = 252.0 + rng.uniform(-3.0, 3.0)
            if name == "payment-gateway" and 930 <= minute <= 959:
                failed = 250
                p99 = 288.0 + rng.uniform(-4.0, 4.0)
            if name == "order-api" and 990 <= minute <= 1009:
                failed = 0
                p99 = 640.0 + rng.uniform(-10.0, 10.0)
            success = max(0, total - failed)
            rows.append(
                {
                    "timestamp": _iso(timestamp),
                    "service": name,
                    "total_requests": total,
                    "success_requests": success,
                    "p99_latency_ms": f"{p99:.2f}",
                    "availability_target": f"{float(service['target']):.6f}",
                    "latency_threshold_ms": f"{float(service['latency_threshold_ms']):.1f}",
                }
            )
    rows.sort(key=lambda row: (row["timestamp"], row["service"]))
    return rows


def _base_handover(handover_id: str, handover_at: datetime) -> dict[str, Any]:
    return {
        "handover_id": handover_id,
        "from_operator": "oncall-a",
        "to_operator": "oncall-b",
        "handover_at": _iso(handover_at),
        "notes": "上一班备注：支付链路 SLI 有点抖；未附预算快照。",
        "actual_alert_level": "P2",
        "open_incidents": [
            {
                "incident_id": "INC-2026-1118-01",
                "title": "支付链路 SLI 抖动",
                "summary": "支付链路 SLI 有点抖",
                "service": "payment-gateway",
                "services": ["payment-gateway"],
                "severity": "P2",
                "status": "open",
                "started_at": "2026-11-18T14:30:00Z",
            }
        ],
        "recent_changes": [
            {
                "change_id": "CHG-2026-1118-01",
                "services": ["payment-gateway"],
                "risk": "high",
                "changed_at": "2026-11-18T14:42:00Z",
                "description": "支付网关发布",
            },
            {
                "change_id": "CHG-2026-1118-02",
                "services": ["order-api"],
                "risk": "medium",
                "changed_at": "2026-11-18T13:50:00Z",
                "description": "订单路由参数调整",
            },
        ],
        "silences": [
            {
                "silence_id": "SIL-2026-1118-PAY-01",
                "services": ["payment-gateway"],
                "severity_scope": ["P2", "P3"],
                "starts_at": "2026-11-18T14:30:00Z",
                "ends_at": "2026-11-18T15:30:00Z",
                "reason": "支付接口临时降噪",
                "created_by": "oncall-a",
            },
            {
                "silence_id": "SIL-2026-1118-GLOBAL-01",
                "services": ["*"],
                "severity_scope": ["P1", "P2", "P3", "P4"],
                "starts_at": "2026-11-18T14:00:00Z",
                "ends_at": "2026-11-18T18:00:00Z",
                "reason": "计划维护窗口",
                "created_by": "oncall-a",
            },
        ],
    }


def generate_handovers() -> dict[str, dict[str, Any]]:
    primary = _base_handover(
        "HO-2026-1118-1500",
        datetime(2026, 11, 18, 15, 0, tzinfo=UTC),
    )
    duplicate_timestamp = copy.deepcopy(primary)
    duplicate_timestamp["handover_id"] = "HO-2026-1118-1500-DUP"
    duplicate_id = copy.deepcopy(primary)
    duplicate_id["handover_at"] = "2026-11-18T15:01:00Z"
    late = _base_handover(
        "HO-2026-1118-2359",
        datetime(2026, 11, 18, 23, 59, tzinfo=UTC),
    )
    late["actual_alert_level"] = "P3"
    late["open_incidents"][0]["severity"] = "P3"
    return {
        "handover.json": primary,
        "handover_duplicate_timestamp.json": duplicate_timestamp,
        "handover_duplicate_id.json": duplicate_id,
        "handover_late.json": late,
    }


def generate_all(out_dir: str | Path, seed: int = 304) -> dict[str, Any]:
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    samples_path = output / "samples.csv"
    rows = generate_samples(seed)
    fieldnames = [
        "timestamp",
        "service",
        "total_requests",
        "success_requests",
        "p99_latency_ms",
        "availability_target",
        "latency_threshold_ms",
    ]
    with samples_path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fieldnames, lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)
    handovers = generate_handovers()
    for filename, handover in handovers.items():
        _json_write(output / filename, handover)
    _json_write(output / "policy.json", copy.deepcopy(DEFAULT_POLICY))
    sample_digest = hashlib.sha256(samples_path.read_bytes()).hexdigest()
    manifest = {
        "seed": seed,
        "start": _iso(START),
        "minutes_per_service": 1440,
        "services": [item["name"] for item in _services()],
        "sample_rows": len(rows),
        "samples_sha256": sample_digest,
        "anomaly_windows": {
            "payment-gateway_availability_degradation": {
                "start": "2026-11-18T15:30:00Z",
                "end": "2026-11-18T15:59:00Z",
                "minutes": 30,
            },
            "order-api_p99_exceedance": {
                "start": "2026-11-18T16:30:00Z",
                "end": "2026-11-18T16:49:00Z",
                "minutes": 20,
            },
        },
        "primary_handover": "handover.json",
        "handover_files": sorted(handovers),
    }
    _json_write(output / "manifest.json", manifest)
    return manifest
