from __future__ import annotations

from dataclasses import replace

import numpy as np
import pytest

from acoustic_workbench.analysis import FrameAnalyzer
from acoustic_workbench.codes import E_PARA, E_TOL
from acoustic_workbench.config import AnalysisConfig
from acoustic_workbench.errors import WorkbenchError
from acoustic_workbench.models import RenderConfig
from acoustic_workbench.render import ConvolutionRenderer
from acoustic_workbench.strategy import (
    compare_preview_calibration,
    resolve_analysis_config,
)


class PerturbingAnalyzer(FrameAnalyzer):
    def analyze(self, sample, config, **kwargs):
        frames = super().analyze(sample, config, **kwargs)
        if config.mode == "preview" and frames:
            frames[0] = replace(
                frames[0],
                delay_samples=tuple(value + 100.0 for value in frames[0].delay_samples),
            )
        return frames


def test_delay_truth_and_mode_tolerance(short_sample) -> None:
    analyzer = FrameAnalyzer()
    effective, decision = resolve_analysis_config(short_sample, AnalysisConfig())
    frames = analyzer.analyze(short_sample, effective)
    truth_delays = [
        channel["direct_delay_samples"] for channel in short_sample.truth["channels"]
    ]
    truth_early = [
        channel["early_window_energy"] for channel in short_sample.truth["channels"]
    ]
    assert list(frames[0].delay_samples) == pytest.approx(truth_delays, abs=1e-6)
    assert list(frames[0].early_energy) == pytest.approx(truth_early, rel=1e-6, abs=1e-12)
    assert decision.effective_mode == "calibration"
    assert decision.estimation_window == 4096
    comparison = compare_preview_calibration(
        short_sample, AnalysisConfig(), analyzer=analyzer
    )
    assert comparison["ok"] is True
    assert comparison["max_delay_delta_samples"] == 0.0
    assert comparison["tolerance"]["delay_samples"] == 1.0

    with pytest.raises(WorkbenchError) as raised:
        compare_preview_calibration(
            short_sample, AnalysisConfig(), analyzer=PerturbingAnalyzer()
        )
    assert raised.value.code == E_TOL
    assert raised.value.frame_index == 0


def test_full_ir_frame_recovers_drr_truth(short_sample) -> None:
    config = AnalysisConfig(
        frame_length=32768,
        hop_length=4096,
        estimation_window=32768,
        mode="calibration",
        auto_tradeoff=False,
    )
    frame = FrameAnalyzer().analyze(short_sample, config)[0]
    truth_drr = [channel["drr_db"] for channel in short_sample.truth["channels"]]
    assert list(frame.drr_db) == pytest.approx(truth_drr, abs=1e-5)


def test_render_length_and_configurable_channel_map(short_sample) -> None:
    analyzer = FrameAnalyzer()
    config, decision = resolve_analysis_config(short_sample, AnalysisConfig())
    frames = analyzer.analyze(short_sample, config)
    render_config = RenderConfig(
        convolution_window=1024,
        render_block_size=512,
        channel_map=(("L", "SL"), ("R", "SR"), ("C", "FL"), ("LFE", "FR")),
    )
    result = ConvolutionRenderer().render(short_sample, frames, render_config)
    assert result.output.shape == (4, short_sample.dry.shape[0])
    assert result.stats.sample_count == short_sample.dry.shape[0]
    assert result.stats.output_channels == ("L", "R", "C", "LFE")
    assert np.isfinite(result.output).all()


def test_bad_render_parameter_has_no_partial_result(short_sample) -> None:
    analyzer = FrameAnalyzer()
    config, _ = resolve_analysis_config(short_sample, AnalysisConfig())
    frames = analyzer.analyze(short_sample, config)
    with pytest.raises(WorkbenchError) as raised:
        ConvolutionRenderer().render(
            short_sample,
            frames,
            RenderConfig(convolution_window=8, render_block_size=512),
        )
    assert raised.value.code == E_PARA
    assert raised.value.field == "convolution_window"
