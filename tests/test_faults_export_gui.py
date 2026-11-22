from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest
from scipy.io import wavfile

from spatial_workbench.audio import export_render
from spatial_workbench.errors import ErrorCode, WorkbenchError
from spatial_workbench.faults import FaultPlan
from spatial_workbench.io import load_sample
from spatial_workbench.models import AnalysisConfig
from spatial_workbench.pipeline import ReconstructionPipeline


def _run_code(path, tmp_path, suffix):
    sample = load_sample(path)
    outcome = ReconstructionPipeline(
        sample,
        AnalysisConfig(
            frame_length=512,
            hop_length=256,
            max_workers=2,
            max_buffer_bytes=4 * 1024 * 1024,
        ),
        tmp_path / suffix,
    ).run_realtime()
    return outcome.error.code if outcome.error is not None else None


def test_fault_matrix(sample_bundle, tmp_path):
    with pytest.raises(WorkbenchError) as caught:
        load_sample(sample_bundle.missing_channel)
    assert caught.value.code == ErrorCode.E_CHAN
    assert _run_code(sample_bundle.time_reversed, tmp_path, "order.json") == ErrorCode.E_ORDER
    assert _run_code(sample_bundle.gain_overflow, tmp_path, "clip.json") == ErrorCode.E_CLIP
    assert _run_code(sample_bundle.bad_frame, tmp_path, "frame.json") == ErrorCode.E_FRAME


def test_programmatic_fault_plan(normal_sample):
    missing = FaultPlan(missing_channel="RR").apply(normal_sample)
    assert missing.missing_channels == ("RR",)
    reversed_sample = FaultPlan(reverse_time=True).apply(normal_sample)
    assert np.array_equal(reversed_sample.ir, normal_sample.ir[:, ::-1])
    clipped = FaultPlan(gain=32.0).apply(normal_sample)
    assert np.max(np.abs(clipped.ir)) > 4.0
    bad = FaultPlan(bad_frames=(4,)).apply(normal_sample)
    assert bad.bad_frames == (4,)
    assert normal_sample.bad_frames == ()


def test_export_preserves_sample_count_and_writes_frames(normal_sample, config, tmp_path):
    outcome = ReconstructionPipeline(
        normal_sample,
        config,
        tmp_path / "checkpoint.json",
    ).run_realtime()
    files = export_render(outcome.result, tmp_path / "export")
    stored = np.load(files["npy"])
    assert stored.shape == outcome.result.output.shape
    rate, wav = wavfile.read(files["wav"])
    assert rate == normal_sample.sample_rate
    assert wav.shape == (normal_sample.dry.size, 4)
    assert os.path.getsize(files["frames"]) > 100
    assert os.path.getsize(files["manifest"]) > 100


def test_gui_offscreen_smoke(monkeypatch, normal_sample, tmp_path):
    env = os.environ.copy()
    env["QT_QPA_PLATFORM"] = "offscreen"
    script = (
        "from PySide6.QtWidgets import QApplication\n"
        "from spatial_workbench.gui import MainWindow\n"
        "app = QApplication([])\n"
        f"window = MainWindow({str(normal_sample.source_path)!r}, "
        f"{str(tmp_path / 'gui.checkpoint.json')!r})\n"
        "window.show()\n"
        "app.processEvents()\n"
        "assert window.sample is not None\n"
        "assert window.table.columnCount() == 8\n"
        "print('gui-ok')\n"
        "window.close()\n"
    )
    result = subprocess.run(
        [sys.executable, "-c", script],
        cwd=str(Path(__file__).resolve().parents[1]),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    assert "gui-ok" in result.stdout

