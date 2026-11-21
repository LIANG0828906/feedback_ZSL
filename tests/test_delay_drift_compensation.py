from __future__ import annotations

import numpy as np
import pytest

import audio_sync.pipeline as pipeline_module
from audio_sync.errors import E_COMPENSATION_FAILED, SyncError
from audio_sync.pipeline import run_pipeline
from audio_sync.models import SyncConfig
from audio_sync.wavio import WavReader


def test_segmented_drift_fit_and_compensation(
    drift_dataset,
    marker_ground_truth,
    tmp_path,
) -> None:
    config = SyncConfig(segment_seconds=2.0, interpolation="linear")
    report = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        config,
    )
    assert report["strategy"]["selected"] == "segmented"
    assert report["strategy"]["segment_count"] == 8
    assert report["compensation"]["total_samples"] == report["input"]["frames"]
    assert report["compensation"]["tolerance_ok"]
    assert report["diagnostics"]["measured_worst_residual_samples"] <= config.tolerance_samples

    truth = marker_ground_truth["drift_ppm"]
    for index, model in enumerate(report["drift"]["channels"]):
        assert model["model_kind"] in {"reference", "linear_drift"}
        assert model["error_code"] is None
        if index == 0:
            assert model["drift_ppm"] == 0.0
        else:
            assert abs(model["drift_ppm"] - truth[index]) < 1.0
            assert model["fit_residual_rms"] < 0.2

    for channel in report["compensation"]["channels"]:
        assert len(channel["segment_hashes"]) == 8
        before = [value for value in channel["after_max"] if value is not None]
        assert before
        assert max(before) <= config.tolerance_samples

    with WavReader(report["outputs"]["aligned_wav"]) as aligned:
        assert aligned.frames == report["input"]["frames"]
        assert aligned.channels == 8
        assert aligned.sample_rate == report["input"]["sample_rate"]
    with np.load(report["outputs"]["residual_heatmap_npz"], allow_pickle=False) as archive:
        assert archive["before_rms"].shape == (8, 8)
        assert archive["after_rms"].shape == (8, 8)


def test_windowed_sinc_and_cache_reuse(drift_dataset, tmp_path) -> None:
    base = SyncConfig(segment_seconds=2.0, interpolation="linear")
    first = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        base,
    )
    second = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        base,
        previous_payload=first,
    )
    stats = second["cache"]["stats"]
    assert stats["marker_hits"] == 8
    assert stats["delay_hits"] == 64
    assert stats["fit_hits"] == 8
    assert stats["segment_hits"] == 64
    assert stats["bytes_reused"] > 0
    assert second["compensation"]["tolerance_ok"]
    assert all(
        left["segment_hashes"] == right["segment_hashes"]
        for left, right in zip(
            first["compensation"]["channels"],
            second["compensation"]["channels"],
        )
    )

    sinc = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(segment_seconds=2.0, interpolation="windowed_sinc"),
        previous_payload=second,
    )
    assert sinc["cache"]["stats"]["delay_hits"] == 64
    assert sinc["cache"]["stats"]["fit_hits"] == 8
    assert sinc["cache"]["stats"]["segment_misses"] == 64
    assert sinc["compensation"]["tolerance_ok"]
    comparison = sinc["compensation"]["interpolation_comparison"]
    assert comparison["available"]
    assert comparison["previous_mode"] == "linear"
    assert comparison["current_mode"] == "windowed_sinc"
    assert len(comparison["channels"]) == 8


def test_failed_segment_is_transactional(drift_dataset, tmp_path, monkeypatch) -> None:
    original = pipeline_module.compensate_segment

    def fail_one_segment(signal, model, global_start_sample, **kwargs):
        if model.channel == 2 and global_start_sample >= 96000:
            raise RuntimeError("injected interpolation failure")
        return original(
            signal,
            model,
            global_start_sample=global_start_sample,
            **kwargs,
        )

    monkeypatch.setattr(pipeline_module, "compensate_segment", fail_one_segment)
    with pytest.raises(SyncError) as failure:
        run_pipeline(
            drift_dataset["wav"],
            drift_dataset["markers"],
            tmp_path,
            SyncConfig(segment_seconds=2.0),
        )
    assert failure.value.code == E_COMPENSATION_FAILED
    assert list((tmp_path / "runs").glob("*/aligned.wav")) == []
    assert list((tmp_path / "cache" / "segments").glob("*.npz")) == []

