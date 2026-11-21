from __future__ import annotations

import os
import subprocess
import sys
import unittest


class GuiTests(unittest.TestCase):
    def test_widgets_render_offscreen_in_isolated_process(self) -> None:
        script = """
import tempfile
from pathlib import Path
from PySide6.QtCore import QTimer
from PySide6.QtWidgets import QApplication
from cover_replay.config import load_config
from cover_replay.engine import ReplayEngine
from cover_replay.generator import generate_profile
from cover_replay.gui import CreditCurve, ReviewWindow
from cover_replay.model import load_record
root = tempfile.TemporaryDirectory()
generate_profile(root.name, "small", seed=18)
sample = Path(root.name) / "small"
config = load_config(sample / "config.json")
record = load_record(sample / "match.json", config)
run = ReplayEngine(record, config).run()
app = QApplication([])
window = ReviewWindow(str(sample))
window.frames = list(run.frames)
window.slider.setRange(1, len(run.frames))
window._show_step(1)
window.show()
curve = CreditCurve()
curve.resize(320, 180)
curve.set_data(list(run.frames), 1)
curve.show()
curve.repaint()
QTimer.singleShot(30, app.quit)
app.exec()
window.close()
curve.close()
root.cleanup()
print(window.windowTitle())
"""
        environment = dict(os.environ)
        environment["QT_QPA_PLATFORM"] = "offscreen"
        result = subprocess.run(
            [sys.executable, "-c", script],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
            timeout=20,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("掩体对局确定性复核台", result.stdout)


if __name__ == "__main__":
    unittest.main()
