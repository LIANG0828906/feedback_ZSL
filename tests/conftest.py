from __future__ import annotations

from pathlib import Path

import pytest

from broadcast_loudness.params import AnalysisParameters
from broadcast_loudness.synth import synthesize_signal
from broadcast_loudness.wavio import AudioBuffer, read_wav, write_wav


@pytest.fixture
def make_audio(tmp_path: Path):
    def factory(
        *,
        name: str = "sample.wav",
        seed: int = 17,
        channels: int = 6,
        duration: float = 4.0,
        bit_depth: int = 24,
    ) -> tuple[AudioBuffer, AnalysisParameters]:
        path = tmp_path / name
        samples = synthesize_signal(seed, channels, duration)
        write_wav(path, samples, bit_depth)
        audio = read_wav(path)
        params = AnalysisParameters.for_channels(channels)
        return audio, params

    return factory
