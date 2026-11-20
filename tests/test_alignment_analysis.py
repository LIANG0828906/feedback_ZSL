from __future__ import annotations

import math

import numpy as np
import pytest

from studio_calib.config import CalibrationConfig
from studio_calib.pipeline import CalibrationPipeline


def test_adaptive_alignment_matches_known_arrivals(
    sample_dataset: dict, cache_dir
) -> None:
    result = CalibrationPipeline(cache_dir).run(
        sample_dataset["surround"], CalibrationConfig()
    )
    truth = sample_dataset["truth"]["arrival_samples"]
    for channel, expected in truth.items():
        alignment = result.alignments[channel]
        assert alignment["arrival_sample"] == expected
        assert alignment["threshold"] > 0.0
        assert alignment["offset_samples"] == expected - truth["FL"]
        assert alignment["normalization_gain"] > 0.0


def test_reference_change_recomputes_and_reports_offset_delta(
    sample_dataset: dict, cache_dir
) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(reference_channel="FR"),
        previous_result=first,
    )
    arrivals = sample_dataset["truth"]["arrival_samples"]
    for channel in first.channel_names:
        old_offset = arrivals[channel] - arrivals["FL"]
        new_offset = arrivals[channel] - arrivals["FR"]
        alignment = second.alignments[channel]
        assert alignment["offset_samples"] == new_offset
        assert alignment["offset_delta_samples"] == new_offset - old_offset
        trace = next(
            item
            for item in second.trace
            if item["node"] == "align" and item["channel"] == channel
        )
        assert trace["detail"]["old_offset_samples"] == old_offset
        assert trace["detail"]["offset_delta_samples"] == new_offset - old_offset
    assert any(node.startswith("align:") for node in second.affected_nodes)


def test_fft_bands_estimate_known_relative_gain(
    sample_dataset: dict, cache_dir
) -> None:
    config = CalibrationConfig(band_fraction=3)
    result = CalibrationPipeline(cache_dir).run(sample_dataset["surround"], config)
    centers = np.asarray([band["center_hz"] for band in result.bands])
    index = int(np.argmin(np.abs(centers - 1000.0)))
    assert math.isclose(centers[index], 1000.0, rel_tol=0.03)

    truth_f = np.asarray(sample_dataset["truth"]["gain_control_frequencies_hz"])
    fr_gain = np.interp(
        np.log2(centers[index]),
        np.log2(truth_f),
        sample_dataset["truth"]["gain_curves_db"]["FR"],
    )
    fl_gain = np.interp(
        np.log2(centers[index]),
        np.log2(truth_f),
        sample_dataset["truth"]["gain_curves_db"]["FL"],
    )
    measured = float(result.analysis["FR"]["power_db"][index]) - float(
        result.analysis["FL"]["power_db"][index]
    )
    assert measured == pytest.approx(float(fr_gain - fl_gain), abs=1.25)


def test_alignment_deterministic_across_reference(
    sample_dataset: dict, cache_dir
) -> None:
    pipeline = CalibrationPipeline(cache_dir)
    first = pipeline.run(sample_dataset["surround"], CalibrationConfig())
    second = pipeline.run(
        sample_dataset["surround"],
        CalibrationConfig(reference_channel="FC"),
        previous_result=first,
    )
    for channel in first.channel_names:
        assert second.alignments[channel]["arrival_sample"] == first.alignments[channel][
            "arrival_sample"
        ]

