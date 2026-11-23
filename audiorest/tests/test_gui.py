"""PySide6 界面冒烟测试：QT_QPA_PLATFORM=offscreen 下可加载工程。"""

from __future__ import annotations

import os
import shutil
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

SAMPLES = Path(__file__).resolve().parents[1] / "samples"


class GuiSmokeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if not (SAMPLES / "manifest.json").exists():
            from audiorest.generate_samples import generate

            generate(SAMPLES)

    def test_main_window_loads_project(self) -> None:
        from PySide6.QtWidgets import QApplication

        from audiorest.gui.main_window import MainWindow

        app = QApplication.instance() or QApplication([])
        workdir = Path(tempfile.mkdtemp(prefix="audiorest_gui_"))
        try:
            window = MainWindow(workdir)
            doc = SAMPLES / "voice_mono" / "voice_mono.v1.0.json"
            window.path_edit.setText(str(doc))
            window._load_project()
            self.assertIsNotNone(window.project)
            self.assertEqual(window.project.source_version, "1.0")
            self.assertIn("已加载", window.log_view.toPlainText())
            window.close()
        finally:
            app.processEvents()
            shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
