from __future__ import annotations

from pathlib import Path

from minute_backfill.generator import GeneratorConfig, generate_dataset

from .helpers import business_signature, run_engine


def test_hot_key_is_split_and_results_are_identical(tmp_path: Path) -> None:
    config = GeneratorConfig(
        out_dir=tmp_path / "skew-data",
        seed=31,
        payment_rows=90,
        refund_rows=400,
        batches=30,
        minutes=60,
        duplicate_rate=0.05,
        delay_rate=0.08,
        out_of_order_rate=0.15,
        skew_share=0.8,
        force=True,
    )
    generate_dataset(config)
    one, _, _ = run_engine(
        tmp_path, config.out_dir, "slots1", slots=1, workers=0, lateness=60
    )
    six, _, _ = run_engine(
        tmp_path, config.out_dir, "slots6", slots=6, workers=2, lateness=60
    )
    assert business_signature(one)["windows"] == business_signature(six)["windows"]
    assert one["dedup"] == six["dedup"]
    assert one["late"] == six["late"]
    assert six["skew"]["splits"], six["skew"]
    assert six["skew"]["max_share"] <= 0.40
    assert six["skew"]["threshold_satisfied"] is True
    assert six["skew"]["actual_threshold_satisfied"] is True
    assert all(load["share"] <= 0.40 for load in six["skew"]["slot_loads"])
    assert all(load["share"] <= 0.40 for load in six["skew"]["actual_slot_loads"])
