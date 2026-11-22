from __future__ import annotations

import os
import tempfile
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

try:
    from PyQt5 import QtWidgets

    from space_audio.gui import MainWindow
except Exception:
    QtWidgets = None
    MainWindow = None


@unittest.skipIf(MainWindow is None, "PyQt5 is not installed")
class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])

    def test_window_constructs_and_renders_without_frame_data(self) -> None:
        temp = tempfile.TemporaryDirectory(prefix="spatial-gui-")
        window = MainWindow()
        window.workdir = __import__("pathlib").Path(temp.name)
        window._refresh_visuals()
        self.assertIsNotNone(window.table)
        self.assertIsNotNone(window.delay_plot)
        window.close()
        temp.cleanup()


if __name__ == "__main__":
    unittest.main()
