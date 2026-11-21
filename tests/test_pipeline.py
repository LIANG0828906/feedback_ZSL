from __future__ import annotations

import copy
from dataclasses import replace

import pytest

from mcsync.diagnostics import diagnose_all_channels
from mcsync.errors import E_MODE_INVALID, E_VERSION_STALE
from mcsync.fitting import fit_all_channels
from mcsync.models import (
    CompensationConfig,
    DiagnosisConfig,
    FitConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentDelay,
    SegmentationConfig,
)
from mcsync.pipeline import SyncPipeline, export_result
from mcsync.resample import compare_interpolation_modes, compensate_audio
from mcsync.synth import SyntheticConfig, generate_synthetic_audio
from mcsync.versioning import load_report


def test_drift_fit_recovers_known_clock_offsets(synthetic, processed):
    for fit in processed.report.drift_fits:
        truth = synthetic.truth["channels"][fit.channel]
        assert abs(fit.drift_ppm - truth["drift_ppm"]) < 2.0
    for diagnosis in processed.report.diagnoses:
        if diagnosis.channel == 0:
            assert diagnosis.conclusion == "reference_channel"
        else:
            assert diagnosis.conclusion == "clock_frequency_offset"
            assert diagnosis.confidence > 0.8


def test_linear_compensation_is_within_tolerance(processed):
    summary = processed.residual_summary
    assert summary is not None
    assert summary.max_abs_delay_samples < 2.0
    assert all(channel.within_tolerance for channel in summary.channels)


def test_event_jump_is_not_misclassified_as_pure_clock_drift():
    generated = generate_synthetic_audio(
        SyntheticConfig(
            channels=2,
            duration_seconds=5.0,
            drift_ppm=(0.0, 0.0),
            initial_offset_samples=(0.0, 0.0),
            event_jumps={1: [{"time_s": 2.0, "jump_samples": 120.0}]},
        )
    )
    result = SyncPipeline(
        PipelineConfig(
            marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
            segmentation=SegmentationConfig(segment_length_samples=48000),
            compensation=CompensationConfig(mode="linear"),
        )
    ).analyze(generated.audio)
    diagnosis = result.report.diagnoses[1]
    assert diagnosis.conclusion in {
        "event_jump",
        "mixed_clock_drift_and_event_jump",
    }
    assert 2 in diagnosis.jump_segments


def test_mode_comparison_returns_both_residuals(processed):
    comparison = compare_interpolation_modes(
        processed.source_audio,
        processed.report.drift_fits,
        reference_channel=0,
        marker_config=MarkerConfig(
            period_seconds=0.5, pulse_duration_samples=96
        ),
        tolerance_samples=2.0,
    )
    assert set(comparison["modes"]) == {"linear", "windowed_sinc"}
    for mode in ("linear", "windowed_sinc"):
        summary = comparison["modes"][mode]["residual_summary"]
        assert summary["max_abs_delay_samples"] < 2.0
    assert comparison["comparison"]["best_mode"] in {
        "linear",
        "windowed_sinc",
        "tie",
    }


def test_invalid_interpolation_mode(processed):
    with pytest.raises(Exception) as raised:
        compensate_audio(
            processed.source_audio,
            processed.report.drift_fits,
            mode="bogus",
            reference_channel=0,
            marker_config=MarkerConfig(
                period_seconds=0.5, pulse_duration_samples=96
            ),
            tolerance_samples=2.0,
            channels=(0, 1),
        )
    assert raised.value.code == E_MODE_INVALID


def test_atomic_export_hash_and_versions(tmp_path, processed):
    wav_path = tmp_path / "compensated.wav"
    report_path = tmp_path / "report_v3.json"
    exported = export_result(
        processed, wav_path, report_path, target_version="v3"
    )
    assert wav_path.is_file()
    assert report_path.is_file()
    loaded = load_report(report_path)
    assert loaded.content_hash() == loaded.integrity["content_sha256"]
    assert loaded.integrity["wav_sha256"] == exported.wav_sha256
    assert loaded.residual_summary.max_abs_delay_samples < 2.0

    v2 = export_result(
        processed,
        tmp_path / "compensated_v2.wav",
        tmp_path / "report_v2.json",
        target_version="v2",
    )
    assert any(issue.code == "E_VERSION_LOSS" for issue in v2.losses)
    v1 = export_result(
        processed,
        tmp_path / "compensated_v1.wav",
        tmp_path / "report_v1.json",
        target_version="v1",
    )
    assert any(issue.code == "E_VERSION_LOSS" for issue in v1.losses)


def test_stale_parameter_version_rejected(tmp_path, processed):
    stale = replace(processed, parameter_version="stale-version")
    with pytest.raises(Exception) as raised:
        export_result(
            stale,
            tmp_path / "stale.wav",
            tmp_path / "stale.json",
            target_version="v3",
        )
    assert raised.value.code == E_VERSION_STALE
    mismatched = copy.deepcopy(processed)
    mismatched.report.compensation = replace(
        mismatched.report.compensation,
        parameter_version="different-version",
    )
    with pytest.raises(Exception) as raised:
        export_result(
            mismatched,
            tmp_path / "mismatch.wav",
            tmp_path / "mismatch.json",
            target_version="v3",
        )
    assert raised.value.code == E_VERSION_STALE


def test_unknown_drift_is_reported_with_candidates():
    segments = tuple(
        SegmentDelay(
            channel=1,
            segment_index=index,
            start_sample=index * 48000,
            end_sample=(index + 1) * 48000,
            delay_samples=value,
            peak_correlation=0.2,
            ci_low_samples=value - 10.0,
            ci_high_samples=value + 10.0,
            marker_count=1,
        )
        for index, value in enumerate((0.0, 75.0))
    )
    fits = fit_all_channels(
        segments,
        channels=(0, 1),
        reference_channel=0,
        sample_rate=48000,
        config=FitConfig(),
    )
    diagnoses, issues = diagnose_all_channels(
        segments, fits, DiagnosisConfig()
    )
    channel_one = next(item for item in diagnoses if item.channel == 1)
    assert channel_one.conclusion == "unknown"
    assert channel_one.candidates
    assert any(issue.code == "E_DRIFT_UNKNOWN" for issue in issues)

