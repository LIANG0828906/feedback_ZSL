from __future__ import annotations

import os
import tempfile
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtTest import QTest
from PySide6.QtWidgets import QApplication

from tests.helpers import SAMPLES
from vp_pipeline.ui import MainWindow


class UiTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_operator_workflow_smoke(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            window = MainWindow(
                SAMPLES,
                directory,
                source_version=3,
                auto_run=True,
            )
            try:
                self.assertEqual(window.layer_table.rowCount(), 5)
                self.assertEqual(len(window._window_buttons), 6)
                self.assertIn("6/6", window.progress_label.text())
                self.assertIn("vpfc/3", window.source_badge.text())
                window.layer_table.selectRow(1)
                window.opacity_spin.setValue(0.31)
                window._apply_layer_edits()
                self.assertEqual(window.invalidated_windows, {0, 1, 2})
                self.assertIn("3/6", window.progress_label.text())

                window._start_recompute(False)
                for _ in range(300):
                    QTest.qWait(10)
                    if window.last_ui_result is not None:
                        break
                self.assertIsNotNone(window.last_ui_result)
                self.assertIsNone(window.last_ui_error)
                self.assertIn("6/6", window.progress_label.text())
                self.assertEqual(window.reused_windows, {3, 4, 5})
                self.assertFalse(window.invalidated_windows)

                window._select_window(3)
                window._verify_current_window()
                self.assertTrue(
                    {9, 10, 11}.issubset(set(window.heatmaps))
                )
                self.assertTrue(
                    all(
                        int(value.max()) == 0
                        for value in window.heatmaps.values()
                    )
                )
                self.assertTrue(window.heatmap_checkbox.isChecked())

                with tempfile.TemporaryDirectory() as export_directory:
                    result = window.export_to(
                        export_directory, window_indices=[3]
                    )
                    self.assertEqual(result.target_version, "vpfc/1")
                    self.assertTrue(result.scene_path.exists())
                    self.assertTrue(result.frames_path.exists())

                window.source_version_combo.setCurrentIndex(0)
                QTest.qWait(50)
                self.assertIn("vpfc/1", window.source_badge.text())
                self.assertIn("6/6", window.progress_label.text())
            finally:
                window.close()


if __name__ == "__main__":
    unittest.main()
