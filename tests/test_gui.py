from __future__ import annotations

import os
import unittest


class GuiSmokeTests(unittest.TestCase):
    def test_offscreen_workbench_lifecycle(self) -> None:
        try:
            import PyQt5  # noqa: F401
        except ImportError:
            self.skipTest("PyQt5 is not installed in this interpreter")
        os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
        from tactical_pick.gui import run_gui

        code = run_gui("samples", "baselines", smoke_ms=120)
        self.assertEqual(code, 0)


if __name__ == "__main__":
    unittest.main()
