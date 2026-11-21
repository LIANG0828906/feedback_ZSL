from __future__ import annotations

import os
import tempfile
import time
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

try:
    from PySide6.QtWidgets import QApplication

    from acoustic_calibration.gui import CalibrationWindow
except ImportError:  # pragma: no cover - optional environment
    QApplication = None  # type: ignore[assignment]
    CalibrationWindow = None  # type: ignore[assignment]


@unittest.skipIf(QApplication is None, "PySide6 is not installed")
class GuiSmokeTests(unittest.TestCase):
    def test_offscreen_workbench_populates_state_and_plots(self) -> None:
        app = QApplication.instance() or QApplication([])
        with tempfile.TemporaryDirectory() as temporary:
            window = CalibrationWindow(workspace_root=Path(temporary))
            deadline = time.monotonic() + 20.0
            while window._state is None and time.monotonic() < deadline:
                app.processEvents()
                time.sleep(0.02)
            app.processEvents()
            self.assertIsNotNone(window._state)
            self.assertEqual(window.channel_table.rowCount(), 6)
            self.assertGreater(window.version_table.rowCount(), 0)
            self.assertGreater(window.response_canvas.minimumWidth(), 0)
            window.close()
            app.processEvents()


if __name__ == "__main__":
    unittest.main()
