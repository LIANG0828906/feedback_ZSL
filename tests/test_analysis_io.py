from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from spatial_workbench.analysis import analyze_sample
from spatial_workbench.errors import ErrorCode, WorkbenchError
from spatial_workbench.io import load_sample
from spatial_workbench.models import AnalysisConfig
from spatial_workbench.samplegen import generate_sample_bundle, write_corrupt_sample


def test_sample_loads_metadata_and_truth(normal_sample):
    assert normal_sample.sample_rate == 16_000
    assert normal_sample.layout.channels == ("FL", "FR", "RL", "RR")
    assert normal_sample.ir.shape[0] == 4
    assert normal_sample.dry.ndim == 1
    assert len(normal_sample.truth["direct_delay_samples"]) == 4
    assert len(normal_sample.truth["energy_decay_coeff"]) == 4


def test_missing_sample_rate_or_layout_is_meta(sample_bundle, tmp_path):
    with np.load(sample_bundle.normal, allow_pickle=False) as archive:
        ir = archive["ir"]
        dry = archive["dry"]
        metadata = archive["metadata"].item()
    import json

    parsed = json.loads(metadata)
    del parsed["sample_rate"]
    missing_rate = tmp_path / "missing_rate.npz"
    np.savez_compressed(
        missing_rate,
        ir=ir,
        dry=dry,
        metadata=np.array(json.dumps(parsed)),
    )
    with pytest.raises(WorkbenchError) as caught:
        load_sample(missing_rate)
    assert caught.value.code == ErrorCode.E_META

    parsed = json.loads(metadata)
    del parsed["channel_layout"]
    missing_layout = tmp_path / "missing_layout.npz"
    np.savez_compressed(
        missing_layout,
        ir=ir,
        dry=dry,
        metadata=np.array(json.dumps(parsed)),
    )
    with pytest.raises(WorkbenchError) as caught:
        load_sample(missing_layout)
    assert caught.value.code == ErrorCode.E_META


def test_missing_metadata_blob_is_meta(sample_bundle, tmp_path):
    with np.load(sample_bundle.normal, allow_pickle=False) as archive:
        path = tmp_path / "no_metadata.npz"
        np.savez_compressed(path, ir=archive["ir"], dry=archive["dry"])
    with pytest.raises(WorkbenchError) as caught:
        load_sample(path)
    assert caught.value.code == ErrorCode.E_META


def test_corrupt_sample_is_decode(tmp_path):
    path = write_corrupt_sample(tmp_path / "bad.npz")
    with pytest.raises(WorkbenchError) as caught:
        load_sample(path)
    assert caught.value.code == ErrorCode.E_DECODE


def test_missing_channel_is_chan(sample_bundle):
    with pytest.raises(WorkbenchError) as caught:
        load_sample(sample_bundle.missing_channel)
    assert caught.value.code == ErrorCode.E_CHAN
    assert caught.value.details["missing_channels"]


def test_analysis_matches_analytic_direct_delays(analyzed):
    _, _, truth = analyzed
    assert truth["max_abs_direct_delay_error_samples"] == 0
    assert truth["max_abs_energy_decay_error"] < 0.001
    assert all(value > 0 for value in truth["estimated_energy_decay_coeff"])


def test_frame_parameters_have_reconstruction_fields(analyzed):
    context, frames, _ = analyzed
    assert len(frames) == 32
    first = frames[0]
    assert first.direct_delays_samples == list(context.global_direct_delays)
    assert len(first.energy_ratio) == 4
    assert len(first.delay_differences_seconds) == 4
    assert first.direction_confidence >= 0.0
    assert len(first.reflection_delays_samples) == 4
    assert all(len(values) <= 8 for values in first.reflection_delays_samples)
    assert first.output_channels == ("FL", "FR", "RL", "RR")


def test_parameter_bounds_return_para():
    with pytest.raises(WorkbenchError) as caught:
        AnalysisConfig(frame_length=8)
    assert caught.value.code == ErrorCode.E_PARA
    with pytest.raises(WorkbenchError) as caught:
        AnalysisConfig(frame_length=512, hop_length=1024)
    assert caught.value.code == ErrorCode.E_PARA
    with pytest.raises(WorkbenchError) as caught:
        AnalysisConfig(energy_threshold=2.0)
    assert caught.value.code == ErrorCode.E_PARA


def test_invalid_output_layout_returns_para(normal_sample):
    config = AnalysisConfig(
        frame_length=512,
        hop_length=256,
        output_layout="not-a-layout",
    )
    with pytest.raises(WorkbenchError) as caught:
        analyze_sample(normal_sample, config)
    assert caught.value.code == ErrorCode.E_PARA


def test_generator_supports_six_channel_layout(tmp_path):
    bundle = generate_sample_bundle(
        tmp_path,
        seed=99,
        sample_rate=16_000,
        length=2_048,
        layout_name="5.1",
    )
    sample = load_sample(bundle.normal)
    assert sample.ir.shape[0] == 6
    assert sample.layout.channels == ("FL", "FR", "C", "LFE", "RL", "RR")
    config = AnalysisConfig(
        frame_length=256,
        hop_length=128,
        max_workers=2,
        max_buffer_bytes=4 * 1024 * 1024,
    )
    _, _, truth = analyze_sample(sample, config)
    assert truth["max_abs_direct_delay_error_samples"] == 0

