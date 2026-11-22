from __future__ import annotations

import os
import unittest


os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")


def _qt_available() -> bool:
    try:
        from mediaforge.gui.qt_compat import QT_API

        return QT_API in {"PySide6", "PyQt5"}
    except ImportError:
        return False


@unittest.skipUnless(_qt_available(), "PySide6/PyQt5 is not installed")
class GuiSmokeTests(unittest.TestCase):
    def test_window_loads_sample_and_tables(self) -> None:
        from mediaforge.gui.main_window import MainWindow
        from mediaforge.gui.qt_compat import QtWidgets

        app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
        window = MainWindow()
        self.assertGreaterEqual(window.sample_combo.count(), 3)
        window.load_current_sample()
        self.assertIsNotNone(window.model)
        self.assertEqual(window.track_table.rowCount(), 2)
        self.assertEqual(window.chapter_table.rowCount(), 3)
        window.apply_track_edits()
        window.apply_chapter_edits()
        window.preview_metadata()
        self.assertGreater(len(window.metadata_text.toPlainText()), 100)
        window.close()
        app.processEvents()


if __name__ == "__main__":
    unittest.main()
