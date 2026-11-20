from __future__ import annotations

from broadcast_loudness.analyzer import analyze_audio
from broadcast_loudness.params import AnalysisParameters
from broadcast_loudness.synth import generate_sample_file
from broadcast_loudness.wavio import read_wav


def test_generator_reference_matches_core_offline(tmp_path) -> None:
    path = tmp_path / "reference.wav"
    manifest = generate_sample_file(
        path,
        seed=444,
        channels=6,
        duration_seconds=2.5,
        bit_depth=24,
    )
    audio = read_wav(path)
    result = analyze_audio(audio, AnalysisParameters.for_channels(6))
    reference = manifest["reference"]
    assert abs(result.integrated_lufs - reference["integrated_lufs"]) < 1e-9
    block_peaks = [
        block.true_peak_dbtp
        for block in result.blocks
        if block.true_peak_dbtp is not None
    ]
    assert (
        abs(max(block_peaks) - reference["true_peak_dbtp"])
        < reference["expected_agreement"]["true_peak_db"]
    )
