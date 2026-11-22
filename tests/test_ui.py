from __future__ import annotations

import os
import gc
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from vector_workbench.ui import WorkbenchWindow

ROOT = Path(__file__).resolve().parents[1]


class UiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_window_loads_scene_and_status_table(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            window = WorkbenchWindow(ROOT / "data", Path(directory))
            self.app.processEvents()
            self.assertIsNotNone(window._scene)
            self.assertGreater(window.path_table.rowCount(), 0)
            self.assertIsNotNone(window._pipeline)
            self.assertEqual(window.progress.maximum(), 18)
            window.close()
            window.deleteLater()
            self.app.processEvents()
            del window
            gc.collect()
            self.app.quit()


if __name__ == "__main__":
    unittest.main()

