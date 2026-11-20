from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from conftest import recipe_v1, recipe_v2, recipe_v3
from game_audio_restorer.errors import BusinessError, ErrorCode
from game_audio_restorer.processing import process_segment
from game_audio_restorer.restore import RestorationEngine


def _stereo_fixture() -> tuple[np.ndarray, int]:
    sample_rate = 16000
    frames = sample_rate
    time = np.arange(frames) / sample_rate
    envelope = ((time % 0.5) < 0.3).astype(np.float64)
    left = 0.04 + 0.2 * np.sin(2 * np.pi * 200 * time) * envelope
    right = -0.03 + 0.15 * np.sin(2 * np.pi * 300 * time) * envelope
    return np.stack([left, right], axis=1), sample_rate


def test_dc_statistics_are_channel_independent() -> None:
    source, sample_rate = _stereo_fixture()
    output, metrics, parameters = process_segment(
        source, sample_rate, recipe_v2(), segment_index=3
    )
    dc_stage = next(
        stage for stage in parameters["stages"] if stage["stage"] == "dc_offset"
    )
    assert dc_stage["statistics"] == "per_channel"
    assert dc_stage["mean_before"][0] == pytest.approx(float(np.mean(source[:, 0])), abs=1e-9)
    assert dc_stage["mean_before"][1] == pytest.approx(float(np.mean(source[:, 1])), abs=1e-9)
    assert abs(float(np.mean(output[:, 0]))) < 1e-3
    assert abs(float(np.mean(output[:, 1]))) < 1e-3
    assert metrics["channel_rms_dbfs"][0] != metrics["channel_rms_dbfs"][1]


def test_dc_disabled_preserves_recipe_semantics() -> None:
    source, sample_rate = _stereo_fixture()
    output, _, parameters = process_segment(source, sample_rate, recipe_v1())
    dc_stage = next(
        stage for stage in parameters["stages"] if stage["stage"] == "dc_offset"
    )
    assert dc_stage["enabled"] is False
    assert dc_stage["removed"] == [0.0, 0.0]
    # v1's spectral stage must not smuggle in an implicit DC removal.
    assert abs(float(np.mean(output[:, 0]))) > 0.01


def test_channel_alignment_and_serializable_parameters() -> None:
    source, sample_rate = _stereo_fixture()
    output, _, parameters = process_segment(source, sample_rate, recipe_v3())
    spectral = next(
        stage for stage in parameters["stages"] if stage["stage"] == "spectral_subtraction"
    )
    assert output.shape == source.shape
    assert spectral["stereo_alignment"] == "same frame boundaries per channel"
    assert parameters["stages"][0]["output_channels"] == 2
    serialized = json.dumps(parameters, ensure_ascii=False, allow_nan=False)
    assert "effective_alpha" in serialized

    mono, _, mono_params = process_segment(
        source, sample_rate, recipe_v3(mode="mono_mix")
    )
    assert mono.shape == (source.shape[0], 1)
    assert mono_params["stages"][0]["output_channels"] == 1


def test_loudness_target_and_limiter() -> None:
    source, sample_rate = _stereo_fixture()
    _output, metrics, parameters = process_segment(source, sample_rate, recipe_v3())
    gain_stage = parameters["stages"][-1]
    assert abs(metrics["loudness_lufs"] - (-20.0)) <= 1.0
    assert metrics["peak_dbfs"] <= -0.15
    assert gain_stage["limiter_ceiling_dbfs"] == -0.18
    assert gain_stage["limiter_applied_db"] <= 0.0


def test_processing_failure_commits_no_segment(
    tmp_path: Path, stereo_wav: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def explode(*_args: object, **_kwargs: object) -> None:
        raise RuntimeError("injected DSP failure")

    monkeypatch.setattr("game_audio_restorer.restore.process_segment", explode)
    work = tmp_path / "work"
    engine = RestorationEngine(stereo_wav, recipe_v2(), work)
    with pytest.raises(BusinessError) as caught:
        engine.run()
    assert caught.value.code == ErrorCode.PROCESSING
    manifest = json.loads((work / "snapshot.json").read_text("utf-8"))
    assert manifest["segments"] == []
    assert not (work / "restored.wav").exists()
    assert not (work / "report.json").exists()
    assert list((work / "segments").glob("*.wav")) == []
