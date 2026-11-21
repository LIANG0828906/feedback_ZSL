from __future__ import annotations

import json

from lightprobe.bake import Baker
from lightprobe.samples import generate_samples
from lightprobe.scene import load_scene
from lightprobe.verify import compare_to_baseline, verify_two_levels

from conftest import SAMPLES, fast_config


def test_two_level_strategy_selection_and_baseline():
    report = verify_two_levels(SAMPLES)
    assert report["ok"]
    room = report["levels"]["street_corner_room"]
    city = report["levels"]["city_district"]
    assert room["strategy"] == "dense"
    assert city["strategy"] == "sparse"
    for level in (room, city):
        audit = level["strategy_audit"]
        assert audit["sampled_probe_count"] + audit["interpolated_probe_count"] == audit["probe_count"]
        assert audit["dense_rms_error"] >= 0.0
        assert audit["sparse_rms_error"] >= 0.0
        assert audit["error_upper_bound"] >= audit["selected_max_error"]
        assert level["baseline"]["compared_probe_count"] == level["probe_count"]
        assert level["baseline"]["missing_probe_count"] == 0
    assert room["baseline"]["rmse_absolute"] < 0.1
    assert city["baseline"]["rmse_absolute"] < 0.5


def test_forced_strategies_keep_every_probe_and_comparable_error_metric(room):
    dense = Baker().bake(room, fast_config(room, strategy="dense"))
    sparse = Baker().bake(room, fast_config(room, strategy="sparse"))
    assert dense.report.probe_count == sparse.report.probe_count
    assert len(dense.state.records) == len(sparse.state.records)
    assert sparse.report.strategy_audit["sampled_probe_count"] < sparse.report.probe_count
    assert (
        sparse.report.strategy_audit["sampled_probe_count"]
        + sparse.report.strategy_audit["interpolated_probe_count"]
        == sparse.report.probe_count
    )
    assert sparse.report.strategy_audit["validation_probe_ids"] == dense.report.strategy_audit["validation_probe_ids"]
    assert sparse.report.strategy_audit["truth_source"] == dense.report.strategy_audit["truth_source"]


def test_sample_generation_is_reproducible(tmp_path):
    suite = generate_samples(tmp_path)
    baseline = json.loads((tmp_path / "baseline_irradiance.json").read_text())
    original = json.loads((SAMPLES / "baseline_irradiance.json").read_text())
    assert baseline["content_hash"] == original["content_hash"]
    assert suite["baseline_content_hash"] == baseline["content_hash"]

