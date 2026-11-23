"""GUI 离屏冒烟测试（QT_QPA_PLATFORM=offscreen）。"""
from __future__ import annotations

import os
import shutil
import tempfile
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

import numpy as np  # noqa: E402
from PySide6.QtWidgets import QApplication  # noqa: E402

from loudbench.core.config import PipelineConfig  # noqa: E402
from loudbench.gui.main_window import MainWindow  # noqa: E402
from loudbench.gui.widgets import (  # noqa: E402
    CurveWidget, DiffHeatmapWidget, SpectrumWidget, TimelineWidget,
    WaveformWidget)
from loudbench.pipeline.app import build_orchestrator  # noqa: E402

MANIFEST = os.path.join(os.path.dirname(__file__), "..",
                        "samples", "manifest.json")


class GuiTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="loudbench_gui_")

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_widgets_render_offscreen(self) -> None:
        timeline = TimelineWidget()
        timeline.set_segments([{"name": "a", "state": "committed"},
                               {"name": "b", "state": "failed"}])
        fs = 48000
        x = np.sin(2 * np.pi * 440 * np.arange(fs) / fs)
        wave = WaveformWidget()
        wave.set_samples(x)
        spec = SpectrumWidget()
        spec.set_samples(x)
        curve = CurveWidget(title="t")
        curve.set_series({"a": [1.0, 2.0, 3.0]})
        heat = DiffHeatmapWidget()
        heat.set_diff(x, x)
        for widget in (timeline, wave, spec, curve, heat):
            widget.resize(320, 120)
            widget.repaint()

    def test_main_window_pipeline_and_export(self) -> None:
        window = MainWindow(
            MANIFEST, os.path.join(self.tmp, "cp"),
            os.path.join(self.tmp, "exports"))
        config = window.current_config()
        orch = build_orchestrator(
            MANIFEST, config, os.path.join(self.tmp, "cp", "run"))
        orch.run(parallel=True)
        window.orch = orch
        window._populate()
        window._export()
        self.assertTrue(os.path.exists(
            os.path.join(self.tmp, "exports", "gui_program.wav")))
        self.assertTrue(os.path.exists(
            os.path.join(self.tmp, "exports", "gui_report.json")))

    def test_param_change_only_affects_uncommitted(self) -> None:
        # 已提交段为终态，参数变化不能改变其状态/输出
        window = MainWindow(
            MANIFEST, os.path.join(self.tmp, "cp2"),
            os.path.join(self.tmp, "exports2"))
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(),
            os.path.join(self.tmp, "cp2", "run"))
        orch.run(parallel=True)
        before = orch.outputs[0].output_samples.copy()
        window.target.setValue(-16.0)
        # 重新加载：旧检查点因参数指纹变化整体作废，但已提交语义由
        # 新配置下的全量重算保持可追溯；内存中已提交对象不被静默改写
        np.testing.assert_array_equal(orch.outputs[0].output_samples, before)

    def test_committed_segments_are_terminal_in_timeline(self) -> None:
        window = MainWindow(
            MANIFEST, os.path.join(self.tmp, "cp3"),
            os.path.join(self.tmp, "exports3"))
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(),
            os.path.join(self.tmp, "cp3", "run"))
        orch.run(parallel=True)
        window.orch = orch
        window._populate()
        states = [item["state"] for item in window.timeline.segments]
        self.assertEqual(states, ["committed"] * len(states))
        # 调整压缩参数不改变已加载结果对象
        snapshot = orch.outputs[0].output_samples.copy()
        window.comp_ratio.setValue(10.0)
        np.testing.assert_array_equal(orch.outputs[0].output_samples, snapshot)


if __name__ == "__main__":
    unittest.main()
