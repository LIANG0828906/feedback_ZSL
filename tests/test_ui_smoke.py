from __future__ import annotations

import os
import tempfile
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from haptics.workbench.app import MainWindow


class WorkbenchSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_window_renders_all_diagnostic_panes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            window = MainWindow(directory, 0)
            try:
                window.controller.run_realtime()
                window._render_result(window.controller.result)
                self.assertGreater(window.event_table.rowCount(), 0)
                self.assertGreater(window.decision_table.rowCount(), 0)
                self.assertGreater(window.packet_table.rowCount(), 0)
                self.assertGreater(window.error_table.rowCount(), 0)
                self.assertGreater(window.alignment_table.rowCount(), 0)
                self.assertIn("E_MISS", window.summary_label.text())
                updated = window.controller.edit_priority("E0011_00", 9)
                window._render_packets()
                window._render_decisions()
                window._render_invalidations()
                self.assertGreater(len(updated.invalidations), 0)
                self.assertGreater(updated.reused_count, 0)
                statuses = {
                    window.packet_table.item(row, 6).text()
                    for row in range(window.packet_table.rowCount())
                }
                self.assertTrue(any("INVALIDATED" in item for item in statuses))
                self.assertTrue(any("REUSED" in item for item in statuses))
            finally:
                window.close()


if __name__ == "__main__":
    unittest.main()

