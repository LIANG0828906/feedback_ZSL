from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from studio_calib.config import TARGET_PRESETS, CalibrationConfig
from studio_calib.pipeline import CalibrationPipeline
from studio_calib.report import write_diagnostic_bundle


def _stage_nodes(result, prefix: str) -> list[str]:
    return [node for node in result.affected_nodes if node.startswith(prefix + ":")]


def test_repeat_run_reuses_all_cache_nodes(sample_dataset: dict, cache_dir) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(),
        previous_result=first,
    )
    assert second.cache_summary["misses"] == 0
    assert second.affected_nodes == []
    assert second.content_hash == first.content_hash
    assert all(
        entry["cache_hit"] for entry in second.trace
    )


def test_smoothing_change_reuses_alignment_and_band_nodes(
    sample_dataset: dict, cache_dir
) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(smoothing_octaves=0.4),
        previous_result=first,
    )
    assert _stage_nodes(second, "align") == []
    assert _stage_nodes(second, "bands") == []
    assert len(_stage_nodes(second, "smooth")) > 0
    assert len(_stage_nodes(second, "filter")) > 0
    assert second.diff["content_hash_changed"] is True
    assert "smoothing_octaves" in second.diff["config_changes"]
    assert second.cache_summary["hits"] > 0


def test_target_change_skips_smoothing_but_rebuilds_filter(
    sample_dataset: dict, cache_dir
) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(target_curve=TARGET_PRESETS["house_curve"]),
        previous_result=first,
    )
    assert _stage_nodes(second, "align") == []
    assert _stage_nodes(second, "bands") == []
    assert _stage_nodes(second, "smooth") == []
    assert len(_stage_nodes(second, "correction")) > 0
    assert len(_stage_nodes(second, "filter_segment")) > 0
    assert len(_stage_nodes(second, "filter")) == len(second.channel_names)


def test_window_change_invalidates_analysis_downstream(
    sample_dataset: dict, cache_dir
) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(window="blackman"),
        previous_result=first,
    )
    assert _stage_nodes(second, "align") == []
    assert len(_stage_nodes(second, "bands")) == len(second.channel_names)
    assert len(_stage_nodes(second, "smooth")) > 0


def test_content_hashes_and_jsonl_are_byte_deterministic(
    sample_dataset: dict, tmp_path: Path
) -> None:
    pipeline_a = CalibrationPipeline(tmp_path / "cache-a", cache_enabled=False)
    pipeline_b = CalibrationPipeline(tmp_path / "cache-b", cache_enabled=False)
    config = CalibrationConfig(filter_taps=1025, smoothing_octaves=0.3)
    result_a = pipeline_a.run(sample_dataset["surround"], config)
    result_b = pipeline_b.run(sample_dataset["surround"], config)
    assert result_a.content_hash == result_b.content_hash
    assert result_a.trace == result_b.trace

    write_diagnostic_bundle(
        result_a,
        tmp_path / "a.json",
        tmp_path / "a.jsonl",
        tmp_path / "a-coeff.json",
    )
    write_diagnostic_bundle(
        result_b,
        tmp_path / "b.json",
        tmp_path / "b.jsonl",
        tmp_path / "b-coeff.json",
    )
    assert (tmp_path / "a.jsonl").read_bytes() == (tmp_path / "b.jsonl").read_bytes()
    assert (tmp_path / "a-coeff.json").read_bytes() == (
        tmp_path / "b-coeff.json"
    ).read_bytes()


def test_trace_contains_required_traceability_fields(
    sample_dataset: dict, cache_dir
) -> None:
    result = CalibrationPipeline(cache_dir).run(
        sample_dataset["surround"], CalibrationConfig()
    )
    required = {
        "node",
        "channel",
        "band",
        "estimate",
        "error_upper",
        "cache_hit",
        "content_hash",
    }
    assert result.trace
    for entry in result.trace:
        assert required <= set(entry)
        assert len(entry["content_hash"]) == 64
    serialized = "\n".join(
        json.dumps(entry, sort_keys=True, separators=(",", ":"))
        for entry in result.trace
    )
    assert "timestamp" not in serialized.lower()
    assert "file_size" not in serialized.lower()


def test_publication_hash_uses_wav_content_not_path(
    sample_dataset: dict, tmp_path: Path
) -> None:
    copied = tmp_path / "copied-surround51.wav"
    shutil.copyfile(sample_dataset["surround"], copied)
    first = CalibrationPipeline(tmp_path / "cache-a", cache_enabled=False).run(
        sample_dataset["surround"], CalibrationConfig(filter_taps=1025)
    )
    second = CalibrationPipeline(tmp_path / "cache-b", cache_enabled=False).run(
        copied, CalibrationConfig(filter_taps=1025)
    )
    assert first.measurement["path"] != second.measurement["path"]
    assert first.measurement["file_sha256"] == second.measurement["file_sha256"]
    assert first.content_hash == second.content_hash

