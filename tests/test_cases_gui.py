from __future__ import annotations

import json
import os
import time
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

import numpy as np
from PyQt5.QtGui import QImage
from PyQt5.QtWidgets import QApplication

from volrender import (
    E_MODE_INVALID,
    TransferFunction,
    VolRenderError,
)
from volrender.app import VolumeWindow, heatmap_to_qimage, rgba_to_qimage


ROOT = Path(__file__).resolve().parents[1]


class ScriptedCaseTests(unittest.TestCase):
    def test_tf_and_precision_case_codes(self) -> None:
        cases = json.loads(
            (ROOT / "cases" / "cases.json").read_text(encoding="utf-8")
        )
        for case in cases["transfer_functions"]:
            with self.subTest(tf=case["name"]):
                if case["expected_code"] == "OK":
                    transfer = TransferFunction(case["points"])
                    self.assertGreaterEqual(len(transfer.points), 2)
                else:
                    with self.assertRaises(VolRenderError) as raised:
                        TransferFunction(case["points"])
                    self.assertEqual(raised.exception.code, case["expected_code"])
        for case in cases["precision_modes"]:
            if case["expected_code"] == "OK":
                self.assertIn(case["name"], ("float", "fixed"))
            else:
                self.assertEqual(case["expected_code"], E_MODE_INVALID)


class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_qimage_constructs(self) -> None:
        rgba = np.zeros((8, 9, 4), dtype=np.uint8)
        rgba[..., 3] = 255
        image = rgba_to_qimage(rgba)
        self.assertFalse(image.isNull())
        self.assertEqual(image.width(), 9)
        self.assertEqual(image.height(), 8)
        heat = heatmap_to_qimage(np.zeros((8, 9)), 1.0e-3)
        self.assertFalse(heat.isNull())
        self.assertEqual(QImage.Format_RGBA8888_Premultiplied, image.format())

    def test_window_renders_offscreen_and_retains_result(self) -> None:
        window = VolumeWindow(
            ROOT / "samples" / "density_64.vol",
            ROOT / "samples" / "segmentation_64.vol",
        )
        self._wait_for_task(window)
        self.assertIsNotNone(window.last_result)
        self.assertIsNotNone(window.display.pixmap())
        self.assertFalse(window.display.pixmap().isNull())

        old_transfer = window.renderer.transfer
        window.tf_table.item(2, 0).setText("0.1")
        window.request_tf_render()
        self._wait_for_task(window)
        self.assertIn("E_TF_INVALID", window.status.toPlainText())
        self.assertIs(window.renderer.transfer, old_transfer)
        self.assertAlmostEqual(float(window.tf_table.item(2, 0).text()), 0.42)

        window.width_slider.setValue(300)
        window.level_slider.setValue(100)
        window.request_window_render()
        self._wait_for_task(window)
        self.assertEqual(window.volume.window_width, 300.0)
        self.assertEqual(window.volume.window_level, 100.0)
        self.assertIn("max|delta|", window.status.toPlainText())

        window.request_precision_switch("fixed")
        self._wait_for_task(window)
        self.assertEqual(window.renderer.mode, "fixed")
        self.assertEqual(window.last_result.mode, "fixed")

        window.close()

    def _wait_for_task(self, window: VolumeWindow) -> None:
        deadline = time.time() + 15.0
        while window.task is not None and time.time() < deadline:
            self.app.processEvents()
            time.sleep(0.01)
        self.app.processEvents()
        self.assertLess(time.time(), deadline, "GUI render task timed out")


if __name__ == "__main__":
    unittest.main()
