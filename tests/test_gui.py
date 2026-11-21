from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from loudness_archive.config import load_config


class GuiSmokeTests(unittest.TestCase):
    def test_workbench_can_be_constructed(self) -> None:
        try:
            import tkinter as tk
        except ImportError:
            self.skipTest("tkinter is not installed")
        try:
            root = tk.Tk()
        except tk.TclError as exc:
            self.skipTest(f"no display available: {exc}")
        root.withdraw()
        try:
            from loudness_archive.gui import WorkbenchApp

            config = load_config(Path(__file__).resolve().parent.parent / "config" / "workbench.json")
            with tempfile.TemporaryDirectory(prefix="loudness-gui-") as temp:
                app = WorkbenchApp(root, config, temp)
                self.assertEqual(len(app.segment_tree["columns"]), 9)
                self.assertIn("Field Mapping", app.notebook.tab(1, "text"))
            root.update_idletasks()
        finally:
            root.destroy()

