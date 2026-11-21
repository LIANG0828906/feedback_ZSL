from __future__ import annotations

from pathlib import Path

from mcsync.incremental import IncrementalSyncEngine
from mcsync.models import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
)
from mcsync.pipeline import SyncPipeline, export_result
from mcsync.synth import SyntheticConfig, generate_sample_bundle
from mcsync.utils import canonical_json_bytes


def _config() -> PipelineConfig:
    return PipelineConfig(
        marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
        segmentation=SegmentationConfig(segment_length_samples=48000),
        compensation=CompensationConfig(mode="linear"),
    )


def test_generator_wav_is_byte_identical_for_same_seed(tmp_path):
    config = SyntheticConfig(channels=2, duration_seconds=2.0, seed=77)
    _, first = generate_sample_bundle(tmp_path / "first", config)
    _, second = generate_sample_bundle(tmp_path / "second", config)
    first_bytes = Path(first["wav"]).read_bytes()
    second_bytes = Path(second["wav"]).read_bytes()
    assert first_bytes == second_bytes


def test_pipeline_export_and_trace_are_byte_identical(tmp_path):
    config = SyntheticConfig(channels=3, duration_seconds=3.0, seed=91)
    generated = generate_sample_bundle(tmp_path / "source", config)
    from mcsync.wavio import read_multichannel_wav

    audio = read_multichannel_wav(generated[1]["wav"])
    first = SyncPipeline(_config()).analyze(audio)
    second = SyncPipeline(_config()).analyze(audio)
    first_export = export_result(
        first,
        tmp_path / "first" / "compensated.wav",
        tmp_path / "first" / "report_v3.json",
        target_version="v3",
    )
    second_export = export_result(
        second,
        tmp_path / "second" / "compensated.wav",
        tmp_path / "second" / "report_v3.json",
        target_version="v3",
    )
    assert (
        Path(first_export.wav_path).read_bytes()
        == Path(second_export.wav_path).read_bytes()
    )
    assert (
        Path(first_export.report_path).read_bytes()
        == Path(second_export.report_path).read_bytes()
    )
    assert canonical_json_bytes(first.trace) == canonical_json_bytes(second.trace)


def test_incremental_trace_is_deterministic(synthetic):
    first = IncrementalSyncEngine(synthetic.audio, _config())
    first.update_parameters(mode="linear")
    second = IncrementalSyncEngine(synthetic.audio, _config())
    second.update_parameters(mode="linear")
    assert canonical_json_bytes(first.snapshot().trace) == canonical_json_bytes(
        second.snapshot().trace
    )
    assert (
        first.snapshot().compensated_audio.content_hash()
        == second.snapshot().compensated_audio.content_hash()
    )

