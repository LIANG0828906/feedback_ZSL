from __future__ import annotations

import numpy as np

from repair_app.models import ChainConfig, Interval
from repair_app.processors import process_block


def _chain(**updates) -> ChainConfig:
    base = {
        "dc_removal": {"enabled": False, "per_channel": True},
        "noise_gate": {"enabled": False},
        "spectral_subtraction": {"enabled": False},
        "limiter": {"enabled": False},
    }
    for section, values in updates.items():
        base[section].update(values)
    return ChainConfig.from_dict(base)


def test_dc_removal_statistics_are_per_channel() -> None:
    samples = np.zeros((512, 2), dtype=np.int16)
    samples[:, 0] = 1000
    samples[:, 1] = -700
    samples[::7, 0] += 50
    samples[::11, 1] -= 30
    output = process_block(
        samples=samples,
        start_frame=0,
        sample_rate=8_000,
        chain=_chain(dc_removal={"enabled": True}),
        intervals=[Interval(1, 0, 512, gain_db=0.0, fade_in_ms=0.0, fade_out_ms=0.0)],
    )
    assert output.dtype == np.int16
    assert output.shape == samples.shape
    assert abs(float(np.mean(output[:, 0]))) <= 2.0
    assert abs(float(np.mean(output[:, 1]))) <= 2.0
    assert not np.array_equal(output[:, 0], output[:, 1])


def test_noise_gate_keeps_channels_aligned_and_independent() -> None:
    time = np.arange(4096)
    samples = np.zeros((4096, 2), dtype=np.int16)
    samples[:, 0] = (5000 * np.sin(2 * np.pi * 220 * time / 16_000)).astype(np.int16)
    samples[:, 1] = 10
    output = process_block(
        samples=samples,
        start_frame=0,
        sample_rate=16_000,
        chain=_chain(
            noise_gate={
                "enabled": True,
                "threshold_db": -30.0,
                "attack_ms": 2.0,
                "release_ms": 20.0,
                "attenuation_db": -80.0,
            }
        ),
        intervals=[Interval(1, 0, 4096, gain_db=0.0, fade_in_ms=0.0, fade_out_ms=0.0)],
    )
    assert output.shape == samples.shape
    assert np.max(np.abs(output[:, 1].astype(np.int32))) <= 2
    assert np.max(np.abs(output[:, 0].astype(np.int32))) > 3000


def test_spectral_subtraction_stereo_length_and_finiteness() -> None:
    rng = np.random.default_rng(42)
    samples = (rng.standard_normal((4096, 2)) * 2500).astype(np.int16)
    output = process_block(
        samples=samples,
        start_frame=0,
        sample_rate=16_000,
        chain=_chain(spectral_subtraction={"enabled": True, "n_fft": 512, "hop_length": 128}),
        intervals=[Interval(1, 0, 4096, gain_db=0.0, fade_in_ms=0.0, fade_out_ms=0.0)],
    )
    assert output.shape == samples.shape
    assert output.dtype == np.int16
    assert np.isfinite(output).all()


def test_limiter_is_linked_across_channels() -> None:
    samples = np.zeros((256, 2), dtype=np.int16)
    samples[:, 0] = 30000
    samples[:, 1] = -30000
    output = process_block(
        samples=samples,
        start_frame=0,
        sample_rate=8_000,
        chain=_chain(limiter={"enabled": True, "threshold_db": -6.0}),
        intervals=[Interval(1, 0, 256, gain_db=6.0, fade_in_ms=0.0, fade_out_ms=0.0)],
    )
    left_peak = int(np.max(np.abs(output[:, 0].astype(np.int32))))
    right_peak = int(np.max(np.abs(output[:, 1].astype(np.int32))))
    assert abs(left_peak - right_peak) <= 2
    assert left_peak <= int(32767 * 10 ** (-6.0 / 20.0)) + 2

