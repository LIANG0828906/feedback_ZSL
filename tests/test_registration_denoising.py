from __future__ import annotations

import numpy as np
import pytest

from sciseq.config import DenoiseConfig, RegistrationConfig
from sciseq.denoising import adaptive_denoise, estimate_noise
from sciseq.errors import SciSeqError
from sciseq.metrics import quality_metrics
from sciseq.registration import estimate_translation


def test_subpixel_registration_accuracy(tiny_sequence) -> None:
    reference = tiny_sequence.frame(0)
    config = RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0)
    errors = []
    for index in range(1, tiny_sequence.frame_count):
        estimate = estimate_translation(
            reference,
            tiny_sequence.frame(index),
            config,
            frame_index=index,
        )
        truth = tiny_sequence.truth.shifts[index]
        errors.append(float(np.hypot(estimate.offset[0] - truth[0], estimate.offset[1] - truth[1])))
    assert max(errors) < 0.2
    assert float(np.mean(errors)) < 0.08


def test_registration_residual_monotonic_with_tighter_precision(tiny_sequence) -> None:
    reference = tiny_sequence.frame(0)
    means = []
    for factor in (1, 2, 3):
        errors = []
        for index in range(1, tiny_sequence.frame_count):
            estimate = estimate_translation(
                reference,
                tiny_sequence.frame(index),
                RegistrationConfig(window="none", upsample_factor=factor, max_shift=6.0),
                frame_index=index,
            )
            truth = tiny_sequence.truth.shifts[index]
            errors.append(
                float(np.hypot(estimate.offset[0] - truth[0], estimate.offset[1] - truth[1]))
            )
        means.append(float(np.mean(errors)))
    assert means[0] >= means[1] >= means[2]


def test_registration_window_choices(tiny_sequence) -> None:
    reference = tiny_sequence.frame(0)
    moving = tiny_sequence.frame(1)
    estimates = [
        estimate_translation(
            reference,
            moving,
            RegistrationConfig(window=window, upsample_factor=5, max_shift=6.0),
            frame_index=1,
        ).offset
        for window in ("none", "hann", "hamming", "blackman")
    ]
    assert all(np.all(np.isfinite(item)) for item in estimates)


def test_search_range_error_reports_frame_and_displacement(tiny_sequence) -> None:
    target = next(
        index
        for index in range(1, tiny_sequence.frame_count)
        if np.hypot(*tiny_sequence.truth.shifts[index]) > 0.8
    )
    with pytest.raises(SciSeqError) as caught:
        estimate_translation(
            tiny_sequence.frame(0),
            tiny_sequence.frame(target),
            RegistrationConfig(window="none", upsample_factor=5, max_shift=0.3),
            frame_index=target,
        )
    assert caught.value.code == "E_REG"
    assert caught.value.frame == target
    assert "actual_displacement" in caught.value.details


def test_noise_estimate_and_denoise_quality(tiny_sequence) -> None:
    from sciseq.registration import translate_to_reference

    reference = tiny_sequence.frame(0)
    clean = tiny_sequence.clean_base()
    for index in range(1, tiny_sequence.frame_count):
        moving = tiny_sequence.frame(index)
        estimate = estimate_translation(
            reference,
            moving,
            RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0),
            frame_index=index,
        )
        aligned = translate_to_reference(moving, estimate.offset)
        noise = estimate_noise(aligned, 16)
        truth_noise = tiny_sequence.truth.noise_sigmas[index]
        assert abs(noise.sigma - truth_noise) / truth_noise < 0.2
        denoised, details = adaptive_denoise(
            aligned,
            DenoiseConfig(filter="gaussian", block_size=16, strength=1.0),
            noise,
        )
        metrics = quality_metrics(denoised, clean, tiny_sequence.truth.features)
        assert metrics["residual_std"] < 2.5
        assert metrics["edge_fidelity"] > 0.85
        assert details["filter"] == "gaussian"


def test_second_filter_and_parameter_error(tiny_sequence) -> None:
    from sciseq.registration import translate_to_reference

    reference = tiny_sequence.frame(0)
    moving = tiny_sequence.frame(2)
    estimate = estimate_translation(
        reference,
        moving,
        RegistrationConfig(window="none", upsample_factor=20, max_shift=6.0),
        frame_index=2,
    )
    aligned = translate_to_reference(moving, estimate.offset)
    noise = estimate_noise(aligned, 16)
    denoised, details = adaptive_denoise(
        aligned,
        DenoiseConfig(filter="guided", block_size=16, strength=1.0),
        noise,
    )
    assert denoised.shape == aligned.shape
    assert details["filter"] == "guided"
    with pytest.raises(SciSeqError) as caught:
        DenoiseConfig(block_size=2).validate(tiny_sequence.shape)
    assert caught.value.code == "E_PARA"
    assert caught.value.field == "denoise.block_size"
    assert "allowed" in caught.value.details


def test_engine_supports_nonzero_reference_frame(tiny_sequence, tiny_config) -> None:
    from dataclasses import replace

    from sciseq.engine import SequenceProcessor

    reference_index = 4
    config = replace(tiny_config, reference_frame=reference_index)
    result = SequenceProcessor(tiny_sequence, config).run_full(parallel=True)
    truth_relative = (
        tiny_sequence.truth.shifts - tiny_sequence.truth.shifts[reference_index]
    )
    errors = np.hypot(
        result.offsets[:, 0] - truth_relative[:, 0],
        result.offsets[:, 1] - truth_relative[:, 1],
    )
    assert result.offsets[reference_index].tolist() == [0.0, 0.0]
    assert float(errors.max()) < 0.25
    assert result.stats["frames"][0]["registration"]["reference_frame"] == reference_index

