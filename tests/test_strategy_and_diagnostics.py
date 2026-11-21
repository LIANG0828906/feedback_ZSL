from __future__ import annotations

from audio_sync.drift import fit_drift_model
from audio_sync.pipeline import run_pipeline
from audio_sync.models import SegmentDelay, SyncConfig
from audio_sync.synthesizer import synthesize_dataset


def test_full_and_segmented_are_comparable(drift_dataset, tmp_path) -> None:
    segmented = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(strategy="segmented", segment_seconds=2.0),
    )
    full = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(strategy="full"),
    )
    assert segmented["compensation"]["total_samples"] == full["compensation"]["total_samples"]
    assert (
        segmented["marker_detection"]["marker_description"]["signature"]
        == full["marker_detection"]["marker_description"]["signature"]
    )
    assert segmented["strategy"]["segment_count"] == 8
    assert full["strategy"]["segment_count"] == 1
    assert segmented["compensation"]["tolerance_ok"]
    assert not full["compensation"]["tolerance_ok"]
    assert (
        full["diagnostics"]["measured_worst_residual_samples"]
        > segmented["diagnostics"]["measured_worst_residual_samples"]
    )
    assert (
        abs(
            full["strategy"]["estimated_residual_bound_samples"]
            - full["diagnostics"]["measured_worst_residual_samples"]
        )
        < 5.0
    )


def test_step_is_distinguished_from_linear_drift(tmp_path) -> None:
    dataset = synthesize_dataset(
        tmp_path,
        duration_seconds=16.0,
        channels=4,
        seed=21,
        step_samples={1: 12.0},
    )
    report = run_pipeline(
        dataset["wav"],
        dataset["markers"],
        tmp_path / "out",
        SyncConfig(strategy="segmented", segment_seconds=2.0),
    )
    models = {item["channel"]: item for item in report["drift"]["channels"]}
    assert models[1]["model_kind"] == "step_jump"
    assert abs(models[1]["step_samples"] - 12.0) < 1.0
    assert models[1]["evidence_segments"]
    assert models[2]["model_kind"] == "linear_drift"
    assert report["compensation"]["tolerance_ok"]


def test_unknown_drift_lists_candidates() -> None:
    values = [0.0, 20.0, -20.0, 30.0, -30.0, 10.0, 0.0, 25.0, -25.0, 5.0]
    segments = []
    for index, value in enumerate(values):
        center = index * 48000 + 24000
        segments.append(
            SegmentDelay(
                segment_index=index,
                start_sample=center - 24000,
                end_sample=center + 24000,
                center_sample=float(center),
                delay_samples=value,
                peak_correlation=0.9,
                ci_low_samples=value - 2,
                ci_high_samples=value + 2,
                valid=True,
                method="segmented",
            )
        )
    model = fit_drift_model(1, tuple(segments), 48000, SyncConfig())
    assert model.error_code == "E_DRIFT_UNKNOWN"
    assert model.model_kind == "uncertain"
    assert len(model.candidates) >= 3

