from __future__ import annotations

import json

from audio_sync.markers import detect_markers_reader
from audio_sync.models import parse_marker_spec
from audio_sync.synthesizer import synthesize_dataset
from audio_sync.wavio import WavReader


def test_marker_detection_reports_every_expected_marker(drift_dataset) -> None:
    payload = json.load(open(drift_dataset["markers"], encoding="utf-8"))
    spec = parse_marker_spec(payload)
    with WavReader(drift_dataset["wav"]) as reader:
        result = detect_markers_reader(reader, 3, spec, min_snr=5.0)
    assert len(result.markers) == spec.count
    assert result.missing_indices == ()
    assert all(hit.accepted for hit in result.markers)
    assert all(hit.confidence >= 0.3 for hit in result.markers)
    assert [hit.marker_index for hit in result.markers] == list(range(spec.count))


def test_missing_marker_is_explicit(tmp_path) -> None:
    dataset = synthesize_dataset(
        tmp_path,
        duration_seconds=8.0,
        channels=4,
        seed=33,
        missing_markers={2: [3, 7]},
    )
    payload = json.load(open(dataset["markers"], encoding="utf-8"))
    spec = parse_marker_spec(payload)
    with WavReader(dataset["wav"]) as reader:
        result = detect_markers_reader(reader, 2, spec, min_snr=5.0)
    assert result.missing_indices == (3, 7)
    assert [hit.marker_index for hit in result.markers if not hit.accepted] == [3, 7]
    assert all(hit.arrival_sample is None for hit in result.markers if not hit.accepted)

