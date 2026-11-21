from __future__ import annotations

import importlib.util
import os
import subprocess
import sys

import pytest


def test_qt_window_can_be_constructed_offscreen(monkeypatch):
    if importlib.util.find_spec("PySide6") is None:
        pytest.skip("PySide6 is not installed")
    script = """
from PySide6.QtWidgets import QApplication
from cover_replay.gui import MainWindow
app = QApplication([])
window = MainWindow()
assert window.windowTitle() == "Cover Replay Inspector"
assert window.start_button.isEnabled()
assert window.resume_button.isEnabled()
window.close()
app.processEvents()
"""
    environment = dict(os.environ)
    environment["QT_QPA_PLATFORM"] = "offscreen"
    completed = subprocess.run(
        [sys.executable, "-c", script],
        check=False,
        env=environment,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert completed.returncode == 0, completed.stderr

