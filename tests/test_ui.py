from __future__ import annotations

import os
import subprocess
import sys
import unittest


class UiSmokeTests(unittest.TestCase):
    def test_window_paints_atlas_and_tile_grid(self) -> None:
        script = r"""
import sys
from PySide6.QtWidgets import QApplication
from glyphforge.config import WorkbenchConfig
from glyphforge.parser import load_font
from glyphforge.pipeline import run_pipeline
from glyphforge.ui import WorkbenchWindow

app = QApplication.instance() or QApplication([])
font = load_font("samples/fonts/mixed.json")
config = WorkbenchConfig(pixel_size=20, tile_size=6)
result = run_pipeline(font, "AO\u00c1", config)
window = WorkbenchWindow(
    font_path="samples/fonts/mixed.json",
    config_path="samples/configs/mixed-24-nonzero-q255.json",
    text="AO\u00c1",
)
window.show()
app.processEvents()
window._on_result(result)
window.glyph_selector.setCurrentIndex(1)
app.processEvents()
pixmap = window.grab()
assert not pixmap.isNull()
assert pixmap.width() > 500
assert window.last_result is result
window.close()
app.processEvents()
print("UI_OK")
"""
        environment = dict(os.environ)
        environment.setdefault("QT_QPA_PLATFORM", "offscreen")
        completed = subprocess.run(
            [sys.executable, "-c", script],
            cwd=os.getcwd(),
            env=environment,
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        self.assertEqual(
            completed.returncode,
            0,
            msg=f"stdout={completed.stdout}\nstderr={completed.stderr}",
        )
        self.assertIn("UI_OK", completed.stdout)


if __name__ == "__main__":
    unittest.main()
