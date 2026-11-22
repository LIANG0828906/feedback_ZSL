from __future__ import annotations

import hashlib
import os
import time
import unittest
from pathlib import Path

from live_transcode.engine import TranscodeEngine

from .support import Workspace, file_sha

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")


class DeterminismTests(unittest.TestCase):
    def test_fixture_bytes_and_trace_are_stable(self) -> None:
        with Workspace("lt-det-a-") as first, Workspace("lt-det-b-") as second:
            fixtures_a = first.fixtures(seed=1001, duration=1.1)
            fixtures_b = second.fixtures(seed=1001, duration=1.1)
            for a, b in zip(fixtures_a, fixtures_b):
                self.assertEqual(
                    file_sha(Path(a["path"])),
                    file_sha(Path(b["path"])),
                )

            def sequence(ctx: Workspace, wav: str) -> tuple[str, str]:
                engine = TranscodeEngine(ctx.store)
                tier_a = engine.catalog.configure("t", 24000, 32, 16, 256)
                engine.generate_tier(wav, tier_a)
                tier_b = engine.catalog.configure("t", 24000, 48, 16, 256)
                engine.generate_tier(wav, tier_b)
                tier_c = engine.catalog.configure("t", 24000, 48, 24, 256)
                engine.generate_tier(wav, tier_c)
                return (
                    file_sha(ctx.store / "trace.jsonl"),
                    file_sha(ctx.store / "published.json"),
                )

            trace_a, manifest_a = sequence(first, fixtures_a[0]["path"])
            trace_b, manifest_b = sequence(second, fixtures_b[0]["path"])
            self.assertEqual(trace_a, trace_b)
            self.assertEqual(manifest_a, manifest_b)


class GuiSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        from PySide6.QtWidgets import QApplication

        cls.app = QApplication.instance() or QApplication([])

    def test_review_grid_start_and_audit(self) -> None:
        os.environ["QT_QPA_PLATFORM"] = "offscreen"
        from live_transcode.gui import ReviewWindow

        with Workspace("lt-gui-test-") as ctx:
            fixtures = ctx.fixtures(seed=2024, duration=1.0)
            window = ReviewWindow(str(ctx.store), str(ctx.root / "samples"))
            window.wav_path.setText(fixtures[3]["path"])
            window._load_current()
            panel = window.panels["review44"]
            panel.rate.setValue(24000)
            panel.filter_length.setValue(32)
            panel.quant.setCurrentText("24")
            panel.slice_samples.setValue(256)
            panel.start()
            deadline = time.time() + 30
            while (
                panel.worker is not None
                and panel.worker.isRunning()
                and time.time() < deadline
            ):
                self.app.processEvents()
                time.sleep(0.005)
            self.app.processEvents()
            self.assertIsNotNone(panel.last_version_hash)
            self.assertTrue(panel.cells)
            self.assertIn("Complete", panel.status.text())

            panel.audit()
            deadline = time.time() + 30
            while (
                panel.audit_worker is not None
                and panel.audit_worker.isRunning()
                and time.time() < deadline
            ):
                self.app.processEvents()
                time.sleep(0.005)
            self.app.processEvents()
            self.assertIn("Audit passed", panel.status.text())
            window.close()
            self.app.processEvents()


if __name__ == "__main__":
    unittest.main()

