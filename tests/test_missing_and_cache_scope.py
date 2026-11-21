from __future__ import annotations

from audio_sync.pipeline import run_pipeline
from audio_sync.models import SyncConfig
from audio_sync.synthesizer import synthesize_dataset


def test_marker_missing_is_not_silently_skipped(tmp_path) -> None:
    dataset = synthesize_dataset(
        tmp_path,
        duration_seconds=10.0,
        channels=4,
        seed=31,
        missing_markers={2: [5, 9]},
    )
    report = run_pipeline(
        dataset["wav"],
        dataset["markers"],
        tmp_path / "out",
        SyncConfig(strategy="segmented"),
    )
    errors = [error for error in report["errors"] if error["code"] == "E_MARKER_MISSING"]
    assert {(error["details"]["channel"], error["details"]["marker_index"]) for error in errors} == {
        (2, 5),
        (2, 9),
    }
    channel = next(
        item for item in report["marker_detection"]["channels"] if item["channel"] == 2
    )
    assert channel["missing_indices"] == [5, 9]
    assert channel["channel"] == 2


def test_cache_scope_tracks_channel_and_reference_changes(drift_dataset, tmp_path) -> None:
    first = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(
            strategy="segmented",
            enabled_channels=(0, 1, 2),
            reference_channel=0,
        ),
    )
    expanded = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(
            strategy="segmented",
            enabled_channels=(0, 1, 2, 3),
            reference_channel=0,
        ),
        previous_payload=first,
    )
    assert expanded["cache"]["stats"]["marker_hits"] == 3
    assert expanded["cache"]["stats"]["marker_misses"] == 1
    assert expanded["cache"]["stats"]["delay_hits"] == 24
    assert expanded["compensation"]["channel_count"] == 4

    changed_reference = run_pipeline(
        drift_dataset["wav"],
        drift_dataset["markers"],
        tmp_path,
        SyncConfig(
            strategy="segmented",
            enabled_channels=(0, 1, 2, 3),
            reference_channel=1,
        ),
        previous_payload=expanded,
    )
    assert changed_reference["cache"]["stats"]["marker_hits"] == 4
    assert changed_reference["cache"]["stats"]["delay_hits"] == 0
    assert changed_reference["cache"]["stats"]["delay_misses"] == 32
    assert changed_reference["delay_estimation"]["change_summary"]["compared_channels"] == 4
    assert changed_reference["compensation"]["tolerance_ok"]

