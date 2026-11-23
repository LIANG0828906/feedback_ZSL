from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication

from gamepad_studio.ui.main_window import MainWindow
from gamepad_studio.ui.theme import apply_theme

from tests.helpers import ROOT


class UiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])
        apply_theme(cls.app)

    def test_dashboard_renders_and_incremental_edit_updates_views(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="padweave-ui-")
        window = MainWindow(
            samples_root=ROOT / "samples",
            initial_sample=ROOT / "samples" / "dual_stick",
            snapshot_root=Path(temporary.name) / "snapshots",
            async_initial_load=False,
        )
        window._suppress_dialogs = True
        window.resize(1500, 960)
        window.show()
        self.app.processEvents()
        self.assertIsNotNone(window.session)
        self.assertEqual(window.session.bundle.device.id, "dual_stick")
        self.assertTrue(window.truth_badge.text().endswith("PASS"))
        self.assertEqual(window.rule_table.rowCount(), 8)
        self.assertEqual(window.node_table.rowCount(), len(window.session.pipeline.order))
        screenshot = Path(temporary.name) / "dashboard.png"
        self.assertTrue(window.capture(screenshot))
        self.assertTrue(screenshot.exists())

        update = window.session.update_axis(
            "left_x",
            deadzone=0.2,
            curve_exponent=2.0,
            verify=True,
        )
        window._on_update_complete(update)
        self.app.processEvents()
        self.assertIn("29 reuse", window.cache_badge.text())
        self.assertIn("4 calc", window.cache_badge.text())
        self.assertEqual(window.equality_heatmap.badge, "IDENTICAL")
        self.assertEqual(window.truth_badge.text(), "TRUTH · stale")
        window.close()
        self.app.processEvents()
        temporary.cleanup()


if __name__ == "__main__":
    unittest.main()
