import os
import unittest

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication, QMessageBox

from scoreworkbench.errors import LossyError
from scoreworkbench.gui.main_window import MainWindow
from scoreworkbench.gui.timeline import TimelineWidget
from scoreworkbench.playback import FakeClock, PlaybackEngine
from scoreworkbench.versioning import export_score, load_score

from .base import SampleBackedTestCase

_app = QApplication.instance() or QApplication([])


class GuiTests(SampleBackedTestCase):
    def setUp(self):
        self.window = MainWindow()

    def tearDown(self):
        self.window.close()

    def test_window_builds_and_loads_score(self):
        self.window.action_open(str(self.v3))
        self.assertIsNotNone(self.window.session)
        self.assertEqual(self.window.note_table.rowCount(), 12)
        self.assertGreater(self.window.event_table.rowCount(), 0)

    def test_timeline_renders_without_error(self):
        self.window.action_open(str(self.v3))
        widget = self.window.timeline
        self.assertIsInstance(widget, TimelineWidget)
        widget.resize(800, 300)
        widget.set_playhead(2.0)
        widget.repaint()

    def test_play_pause_updates_frame_label(self):
        self.window.action_open(str(self.v3))
        clock = FakeClock()
        self.window.engine = PlaybackEngine(
            self.window.session.committed, clock
        )
        self.window.timeline.set_engine(self.window.engine)
        self.window.action_play()
        clock.advance(0.5)
        self.window._tick()
        self.assertIn("beat=", self.window.frame_label.text())
        self.window.action_pause()
        self.assertFalse(self.window.engine.is_playing)

    def test_lossy_export_logs_warning_then_confirmed_export(self):
        self.window.action_open(str(self.v3))
        self.window.version_combo.setCurrentText("1.0")
        with self.assertRaises(LossyError):
            export_score(
                self.window.session.committed, "1.0",
                self.window.project.provenance,
            )
        document = export_score(
            self.window.session.committed, "1.0",
            self.window.project.provenance, confirm_lossy=True,
        )
        self.assertEqual(document["schema_version"], "1.0")
        self.assertTrue(document["_lossy_warnings"])

    def test_broken_file_logs_error_without_crash(self):
        self.window.action_open(str(self.broken / "corrupted.json"))
        self.assertIn("E_DECODE", self.window.log.toPlainText())

    def test_snapshot_save_restore_cycle(self):
        self.window.action_open(str(self.v3))
        self.window.action_save_snapshot()
        self.window.action_restore_snapshot()
        self.assertIsNotNone(self.window.session)
        self.assertEqual(self.window.note_table.rowCount(), 12)

    def test_seek_signal_accepts_valid_beat(self):
        self.window.action_open(str(self.v3))
        self.window.action_seek(1.5)
        self.assertAlmostEqual(self.window.engine.current_beat(), 1.5)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
