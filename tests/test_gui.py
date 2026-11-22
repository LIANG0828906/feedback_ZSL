from __future__ import annotations

import os
from pathlib import Path
import tempfile
import time
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PyQt5.QtWidgets import QApplication

from edgeforge.gui import MainWindow


class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_window_runs_pipeline_and_refreshes_views(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            window = MainWindow(
                samples_dir=Path(temp) / "samples",
                run_dir=Path(temp) / "runs",
            )
            window.show()
            self.assertEqual(window.sample_combo.count(), 4)
            self.assertEqual(window.sample.image.shape, (240, 320))
            window.tile_spin.setValue(64)
            window._start_run(False, True)
            deadline = time.time() + 30.0
            while window.worker is not None and time.time() < deadline:
                self.app.processEvents()
                time.sleep(0.01)
            self.app.processEvents()
            self.assertIsNotNone(window.pipeline)
            self.assertEqual(window.progress_bar.value(), window.progress_bar.maximum())
            self.assertEqual(window.metrics_table.rowCount(), 6)
            self.assertEqual(window.state_table.rowCount(), 20)
            self.assertEqual(
                len(window.pipeline.stage_arrays),
                6,
            )
            window.close()


if __name__ == "__main__":
    unittest.main()
