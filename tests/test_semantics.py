from __future__ import annotations

import json
import sqlite3
from pathlib import Path

from minute_backfill.generator import GeneratorConfig, generate_dataset
from minute_backfill.timeutil import parse_time_us

from .factory import minute_base, write_dataset
from .helpers import normalized_windows, run_engine
from .oracle import reference_run


def _generated(tmp_path: Path, **overrides) -> Path:
    config = GeneratorConfig(
        out_dir=tmp_path / "data",
        seed=11,
        payment_rows=500,
        refund_rows=300,
        batches=12,
        minutes=20,
        duplicate_rate=0.05,
        delay_rate=0.12,
        out_of_order_rate=0.25,
        force=True,
    )
    for key, value in overrides.items():
        setattr(config, key, value)
    generate_dataset(config)
    return config.out_dir


def test_generated_data_matches_independent_oracle(tmp_path: Path) -> None:
    data = _generated(tmp_path)
    for lateness in (0, 300):
        report, _, _ = run_engine(
            tmp_path, data, f"oracle-{lateness}", lateness=lateness
        )
        oracle = reference_run(data, lateness)
        report_windows = normalized_windows(report)
        for window in report_windows:
            window["window_start"] = parse_time_us(window["window_start"])
        assert report_windows == oracle["windows"]
        assert report["dedup"]["dropped"] == oracle["dedup_dropped"]
        assert report["dedup"]["dropped"] == (
            report["dedup"]["dropped_payment_events"]
            + report["dedup"]["dropped_refund_events"]
        )
        assert report["late"]["total"] == oracle["late_total"]
        assert report["dedup"]["canonical_events"] == oracle["canonical_events"]
        for window in report["windows"]:
            assert window["net_minor"] == (
                window["pay_amount_minor"] - window["refund_amount_minor"]
            )

    late_zero = json.loads((tmp_path / "oracle-0-out" / "report.json").read_text())
    late_large = json.loads((tmp_path / "oracle-300-out" / "report.json").read_text())
    assert late_zero["late"]["total"] > late_large["late"]["total"]
    zero_watermark = sum(
        1 for window in late_zero["windows"] if window["close_reason"] == "watermark"
    )
    large_watermark = sum(
        1 for window in late_large["windows"] if window["close_reason"] == "watermark"
    )
    assert zero_watermark > large_watermark


def test_dedup_tie_earliest_and_first_arrival(tmp_path: Path) -> None:
    base = minute_base()
    data = write_dataset(
        tmp_path / "dedup-data",
        payments=[
            {"payment_id": "P1", "event_time": base + 1_000_000, "amount": "10.00"},
            {"payment_id": "P3", "event_time": base + 2_000_000, "amount": "8.00"},
        ],
        batches=[
            {
                "declared_event_time": base + 60_000_000,
                "rows": [
                    {"refund_id": "R1", "payment_id": "P1", "event_time": base + 10_000_000, "amount": "5.00"},
                    {"refund_id": "R2", "payment_id": "P1", "event_time": base + 10_000_000, "amount": "9.00"},
                    {"refund_id": "R3", "payment_id": "P1", "event_time": base + 5_000_000, "amount": "3.00"},
                    {"refund_id": "S1", "payment_id": "P3", "event_time": base + 15_000_000, "amount": "2.00"},
                    {"refund_id": "S2", "payment_id": "P3", "event_time": base + 15_000_000, "amount": "6.00"},
                ],
            },
            {
                "declared_event_time": base + 120_000_000,
                "rows": [
                    {"refund_id": "L1", "payment_id": "P2", "event_time": base + 30_000_000, "amount": "1.00"},
                    {"refund_id": "R4", "payment_id": "P1", "event_time": base + 8_000_000, "amount": "1.00"},
                ],
            },
        ],
    )
    report, _, _ = run_engine(tmp_path, data, "dedup", lateness=0)
    assert report["dedup"]["dropped"] == 4
    assert report["late"]["total"] == 2
    assert len(report["windows"]) == 1
    window = report["windows"][0]
    assert window["pay_count"] == 2
    assert window["pay_amount"] == "18.00"
    assert window["refund_count"] == 2
    assert window["refund_amount"] == "5.00"
    assert window["net"] == "13.00"


def test_watermark_never_regresses(tmp_path: Path) -> None:
    base = minute_base()
    data = write_dataset(
        tmp_path / "wm-data",
        payments=[{"payment_id": "P1", "event_time": base + 1_000_000, "amount": "3.00"}],
        batches=[
            {"declared_event_time": base + 60_000_000, "rows": []},
            {"declared_event_time": base + 120_000_000, "rows": []},
            {"declared_event_time": base + 90_000_000, "rows": []},
            {"declared_event_time": base + 180_000_000, "rows": []},
        ],
    )
    report, state, _ = run_engine(tmp_path, data, "watermark", lateness=0)
    with sqlite3.connect(state) as conn:
        watermarks = [
            row[0]
            for row in conn.execute(
                "SELECT watermark FROM commits WHERE watermark IS NOT NULL ORDER BY seq"
            ).fetchall()
        ]
    assert watermarks == [
        base + 60_000_000,
        base + 120_000_000,
        base + 120_000_000,
        base + 180_000_000,
    ]
    assert report["processing"]["watermark_us"] == base + 180_000_000
