from __future__ import annotations

import numpy as np

from audio_sync.wavio import (
    AtomicWavSegmentWriter,
    WavReader,
    _pcm_wav_header,
    inspect_wav,
    write_wav_i16,
)


def test_pcm_wav_round_trip_and_segment_writer(tmp_path) -> None:
    data = np.arange(2 * 25, dtype=np.int16).reshape(2, 25)
    path = tmp_path / "roundtrip.wav"
    write_wav_i16(path, data, 48000)
    info = inspect_wav(path)
    assert info.container == "RIFF"
    assert info.channels == 2
    assert info.frames == 25
    with WavReader(path) as reader:
        assert np.array_equal(reader.read_i16(1), data[1])

    segmented = tmp_path / "segmented.wav"
    with AtomicWavSegmentWriter(
        segmented,
        channels=2,
        sample_rate=48000,
        total_frames=25,
    ) as writer:
        writer.write_segment(data[:, :13])
        writer.write_segment(data[:, 13:])
        writer.commit()
    with WavReader(segmented) as reader:
        assert np.array_equal(reader.read_i16(0), data[0])


def test_rf64_header_is_read_without_large_allocation(tmp_path) -> None:
    frames = 16
    channels = 2
    data = np.arange(frames * channels, dtype="<i2").reshape(frames, channels)
    path = tmp_path / "sample.rf64.wav"
    path.write_bytes(
        _pcm_wav_header(channels, 48000, frames, rf64=True)
        + data.tobytes(order="C")
    )
    info = inspect_wav(path)
    assert info.container == "RF64"
    assert info.frames == frames
    with WavReader(path) as reader:
        assert np.array_equal(reader.read_i16(0), data[:, 0])
