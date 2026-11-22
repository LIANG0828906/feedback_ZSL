from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from scworkbench.gui.main_window import MainWindow
from scworkbench.replaylog import inspect_replay_log

from tests.helpers import ROOT


class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_workbench_renders_completes_and_exports_log(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            window = MainWindow(
                ROOT / "samples" / "sample_session.json",
                temp,
                use_loopback=True,
                auto_start=False,
            )
            try:
                window.advance_demo(120)
                self.assertEqual(window.engine.phase.value, "disconnected")
                self.assertEqual(window.engine.stats.frames_committed, 40)
                self.assertEqual(window.frame_table.rowCount(), 40)
                self.assertGreater(window.input_table.rowCount(), 0)
                screenshot = Path(temp) / "workbench.png"
                window.show()
                self.app.processEvents()
                self.assertTrue(window.grab().save(str(screenshot)))
                self.assertGreater(screenshot.stat().st_size, 50_000)
                export_path = Path(temp) / "complete.ndjson"
                window.export_log_to(export_path)
                status = inspect_replay_log(export_path)
                self.assertTrue(status["complete"])
            finally:
                window.close()
                self.app.processEvents()


if __name__ == "__main__":
    unittest.main()
