from __future__ import annotations

import numpy as np

from mcsync.errors import E_MARKER_MISSING
from mcsync.markers import detect_all_markers
from mcsync.models import MarkerConfig
from mcsync.synth import SyntheticConfig, generate_synthetic_audio
from mcsync.wavio import read_multichannel_wav, write_multichannel_wav


def test_marker_detection_all_channels(synthetic):
    config = MarkerConfig(period_seconds=0.5, pulse_duration_samples=96)
    detection = detect_all_markers(
        synthetic.audio, config, reference_channel=0
    )
    assert not detection.issues
    assert set(detection.channels) == set(range(8))
    for channel in range(8):
        markers = detection.markers_for(channel)
        assert len(markers) == 8
        assert all(marker.confidence > 0.5 for marker in markers)
        assert all(
            later.marker_index == earlier.marker_index + 1
            for earlier, later in zip(markers, markers[1:])
        )


def test_missing_marker_is_located_without_silent_skip():
    generated = generate_synthetic_audio(
        SyntheticConfig(
            channels=3,
            duration_seconds=2.0,
            missing_markers={2: [2]},
        )
    )
    detection = detect_all_markers(
        generated.audio,
        MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
        reference_channel=0,
    )
    missing = [
        issue for issue in detection.issues if issue.code == E_MARKER_MISSING
    ]
    assert len(missing) == 1
    assert missing[0].channel == 2
    assert missing[0].marker_index == 2
    assert all(
        marker.marker_index != 2 for marker in detection.markers_for(2)
    )
    assert len(detection.markers_for(0)) == 4


def test_wav_roundtrip_is_16_bit(tmp_path, synthetic):
    path = tmp_path / "roundtrip.wav"
    write_multichannel_wav(path, synthetic.audio.samples, 48000)
    loaded = read_multichannel_wav(path)
    assert loaded.sample_rate == 48000
    assert loaded.channel_count == 8
    assert loaded.frame_count == synthetic.audio.frame_count
    maximum_error = float(
        np.max(np.abs(loaded.samples - synthetic.audio.samples))
    )
    assert maximum_error <= 1.0 / 32768.0 + 1.0e-12

