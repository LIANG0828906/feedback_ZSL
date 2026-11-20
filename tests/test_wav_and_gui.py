from __future__ import annotations

import os

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
os.environ.setdefault("QT_OPENGL", "software")

import numpy as np
import pytest

from studio_calib.config import CalibrationConfig
from studio_calib.errors import CalibrationError
from studio_calib.pipeline import CalibrationPipeline
from studio_calib.samplegen import generate_dataset
from studio_calib.wavio import read_pcm16_wav


def test_mono_and_multichannel_wav_readers_close_cleanly(sample_dataset: dict) -> None:
    mono = read_pcm16_wav(str(sample_dataset["mono"]))
    surround = read_pcm16_wav(str(sample_dataset["surround"]))
    assert mono.sample_rate == 48000
    assert mono.layout == "mono"
    assert list(mono.channels) == ["M"]
    assert surround.layout == "5.1"
    assert list(surround.channels) == ["FL", "FR", "FC", "LFE", "SL", "SR"]
    assert mono.quantization_step == pytest.approx(1.0 / 32768.0)


def test_mono_pipeline_runs(sample_dataset: dict, cache_dir) -> None:
    result = CalibrationPipeline(cache_dir).run(
        sample_dataset["mono"], CalibrationConfig(reference_channel="M")
    )
    assert result.channel_names == ["M"]
    assert result.alignments["M"]["offset_samples"] == 0


def test_sample_generator_is_byte_deterministic_for_same_seed(tmp_path) -> None:
    first = generate_dataset(tmp_path / "a", seed=20261120)
    second = generate_dataset(tmp_path / "b", seed=20261120)
    assert first["files"]["surround51"]["sha256"] == second["files"]["surround51"][
        "sha256"
    ]
    assert first["files"]["mono"]["sha256"] == second["files"]["mono"]["sha256"]


def test_gui_constructs_with_offscreen_software_rendering(
    sample_dataset: dict, cache_dir
) -> None:
    from PySide6.QtWidgets import QApplication

    from studio_calib.gui import WorkbenchWindow

    app = QApplication.instance() or QApplication([])
    window = WorkbenchWindow(str(sample_dataset["surround"]), str(cache_dir))
    assert window.alignment_table.columnCount() == 6
    assert window.energy_plot is not None
    window._timer.stop()
    window.close()
    app.processEvents()

