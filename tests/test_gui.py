from __future__ import annotations

import json
import os
import unittest
from pathlib import Path


os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


try:
    from PySide6.QtWidgets import QApplication

    from levelworkbench.gui import WorkbenchWindow

    PYSIDE_AVAILABLE = True
except Exception:  # pragma: no cover - optional environment
    PYSIDE_AVAILABLE = False


@unittest.skipUnless(PYSIDE_AVAILABLE, "PySide6 is not installed")
class GuiBoundaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.app = QApplication.instance() or QApplication([])

    def test_second_generation_returns_busy(self) -> None:
        document = json.loads((SAMPLES / "small.json").read_text(encoding="utf-8"))
        window = WorkbenchWindow(document)
        self.assertTrue(window.gate.try_acquire())
        code = window.start_generation()
        self.assertEqual(code, "E_BUSY")
        self.assertIn("E_BUSY", window.status_label.text())
        window.gate.release()
        window.close()

    def test_cancel_failure_keeps_last_committed_trace(self) -> None:
        from levelworkbench.errors import E_CANCELLED, WorkbenchError

        document = json.loads((SAMPLES / "small.json").read_text(encoding="utf-8"))
        window = WorkbenchWindow(document)
        committed = {"meta": {"trace_version": "LEVEL_TRACE_V1"}}
        window.last_committed_trace = committed
        window._on_failure(WorkbenchError(E_CANCELLED, "cancelled"))
        self.assertIs(window.last_committed_trace, committed)
        self.assertIn("E_CANCELLED", window.status_label.text())
        window.close()


if __name__ == "__main__":
    unittest.main()
