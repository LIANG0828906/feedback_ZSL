import os
import time
import unittest
from unittest import mock

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from PySide6.QtWidgets import QApplication, QMessageBox

from syncbench.errors import E_BUSY
from syncbench.gui import MainWindow

from .base import SampleSuiteTestCase


class TestGui(SampleSuiteTestCase):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.app = QApplication.instance() or QApplication([])

    def _window(self, name):
        return MainWindow(
            self.dataset_dir("skew4"),
            self.out_dir(name),
            os.path.join(self.out_dir(name), "gui.trace.log"),
            n_channels=4,
        )

    def _wait_done(self, window, timeout=60.0):
        deadline = time.monotonic() + timeout
        while window._thread is not None and time.monotonic() < deadline:
            self.app.processEvents()
            time.sleep(0.01)
        self.app.processEvents()
        self.assertIsNone(window._thread, "sync thread did not finish")

    def test_sync_completes_in_background(self):
        window = self._window("gui1")
        window.start_sync()
        self.assertIsNotNone(window._thread)
        self._wait_done(window)
        self.assertIsNotNone(window.last_result)
        self.assertTrue(window.status.text().startswith("done"))
        self.assertTrue(window.start_btn.isEnabled())
        window.close()

    def test_busy_while_running(self):
        window = self._window("gui2")
        with mock.patch.object(QMessageBox, "warning", return_value=None):
            window._thread = object()  # simulate a running task
            window.start_sync()
            self.assertEqual(window.status.text(), E_BUSY)
            window._thread = None
        window.close()

    def test_busy_from_session(self):
        window = self._window("gui3")
        window.session.try_acquire()
        with mock.patch.object(QMessageBox, "warning", return_value=None):
            window.start_sync()
            self._wait_done(window)
            self.assertEqual(window.status.text(), E_BUSY)
        window.session.release()
        window.close()

    def test_cancel_releases_session(self):
        window = self._window("gui4")
        window.start_sync()
        window.session.cancel()
        self._wait_done(window)
        self.assertTrue(window.session.try_acquire())
        window.session.release()
        window.close()


if __name__ == "__main__":
    unittest.main()
