from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PyQt5.QtWidgets import QApplication  # noqa: E402

from artmask.gui.app import WorkbenchWindow  # noqa: E402
from artmask.samplegen import DEFAULT_SAMPLES, generate_suite  # noqa: E402


class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])
        cls.temp = tempfile.TemporaryDirectory()
        cls.workspace = Path(cls.temp.name) / "workspace_data"
        generate_suite(cls.workspace / "samples", (DEFAULT_SAMPLES[0],))

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_window_is_created_and_sample_loads(self) -> None:
        window = WorkbenchWindow(self.workspace)
        self.assertIn("ArtMask", window.windowTitle())
        self.assertGreater(window.sample_list.count(), 0)
        self.assertIsNotNone(window.current_sample)
        self.assertIn("判定", window.mode_label.text())
        window.close()


if __name__ == "__main__":
    unittest.main()
