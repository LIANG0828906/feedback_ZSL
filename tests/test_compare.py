from __future__ import annotations

import json
from pathlib import Path

import pytest

from conftest import recipe_v1, recipe_v2, recipe_v3, write_test_wav
from game_audio_restorer.compare import compare_versions, write_compare_outputs
from game_audio_restorer.errors import BusinessError, ErrorCode


def test_compare_versions_has_per_segment_diagnostics(tmp_path: Path) -> None:
    source = tmp_path / "voice.wav"
    write_test_wav(source, duration=1.6)
    report = compare_versions(
        source,
        [recipe_v1(), recipe_v2(), recipe_v3()],
        tmp_path / "comparison",
        segment_seconds=0.5,
    )
    assert report["segment_count"] == 4
    assert len(report["segments"]) == 4
    assert report["hash"]
    assert "重采样" in report["sample_rate_note"]
    for row in report["segments"]:
        assert set(row["versions"]) == {"v1", "v2", "v3"}
        assert [pair["pair"] for pair in row["comparisons"]] == [
            "v1_v2",
            "v1_v3",
            "v2_v3",
        ]
        for pair in row["comparisons"]:
            assert "metric_deltas" in pair
            assert "residual_noise_dbfs" in pair["metric_deltas"]
            assert "loudness_lufs" in pair["metric_deltas"]
            assert "gain_jump_db" in pair["metric_deltas"]
            if pair["incompatible"]:
                assert pair["attribution"]
                for note in pair["attribution"]:
                    assert note["message"]
    summary = report["summary"]
    assert set(summary) == {"v1_v2", "v1_v3", "v2_v3"}
    assert summary["v1_v2"]["incompatible_segments"] >= 1

    report_path = tmp_path / "comparison.json"
    trace_path = tmp_path / "comparison_trace.jsonl"
    write_compare_outputs(report, report_path, trace_path)
    assert json.loads(report_path.read_text("utf-8"))["segment_count"] == 4
    trace_lines = trace_path.read_text("utf-8").splitlines()
    assert len(trace_lines) == 5
    assert "field_mapping" in json.loads(trace_lines[1])["versions"]["v1"]


def test_compare_requires_three_versions(tmp_path: Path) -> None:
    source = tmp_path / "voice.wav"
    write_test_wav(source, duration=0.5, channels=1)
    with pytest.raises(BusinessError) as caught:
        compare_versions(source, [recipe_v1(), recipe_v2()], tmp_path / "cmp")
    assert caught.value.code == ErrorCode.RECIPE_INVALID


def test_compare_deterministic_hash_across_directories(tmp_path: Path) -> None:
    source = tmp_path / "voice.wav"
    write_test_wav(source, duration=0.8, channels=1)
    first = compare_versions(
        source,
        [recipe_v1(), recipe_v2(), recipe_v3()],
        tmp_path / "a",
        segment_seconds=0.5,
    )
    second = compare_versions(
        source,
        [recipe_v1(), recipe_v2(), recipe_v3()],
        tmp_path / "b",
        segment_seconds=0.5,
    )
    assert first["hash"] == second["hash"]
    assert first["segments"] == second["segments"]
