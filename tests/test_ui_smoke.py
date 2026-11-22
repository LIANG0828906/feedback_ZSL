from __future__ import annotations

import os
import tempfile
import time
import unittest

from tests.helpers import ROOT


@unittest.skipUnless(os.environ.get("DISPLAY"), "Tk desktop requires DISPLAY")
class UiSmokeTests(unittest.TestCase):
    def test_window_reaches_preview_or_exact(self) -> None:
        import tkinter as tk

        from npr_workbench.ui import WorkbenchApp

        with tempfile.TemporaryDirectory() as directory:
            root = tk.Tk()
            app = WorkbenchApp(
                root,
                data_dir=ROOT / "data",
                scene_name="orb_and_crate",
                persist_dir=__import__("pathlib").Path(directory),
            )
            deadline = time.time() + 8.0
            while time.time() < deadline and app.current_frame is None:
                root.update()
                time.sleep(0.02)
            self.assertIsNotNone(app.current_frame)
            self.assertTrue(app.images)
            self.assertIn("STYLIZED", app.notebook.tab(app.notebook.select(), "text"))
            app._close()


if __name__ == "__main__":
    unittest.main()

