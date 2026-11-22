from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")

from volrender.samples import generate_samples, make_sample_scenes  # noqa: E402


class SampleTests(unittest.TestCase):
    def test_sample_generation_is_reproducible(self) -> None:
        with tempfile.TemporaryDirectory(prefix="volrender-s1-") as first:
            with tempfile.TemporaryDirectory(prefix="volrender-s2-") as second:
                a = generate_samples(first, seed=77, render_baselines=False)
                b = generate_samples(second, seed=77, render_baselines=False)
                self.assertEqual(a["manifest_hash"], b["manifest_hash"])
                for entry in a["samples"]:
                    self.assertEqual(
                        (Path(first) / entry["scene_file"]).read_bytes(),
                        (Path(second) / entry["scene_file"]).read_bytes(),
                    )

    def test_sample_set_varies_resolution_and_media_count(self) -> None:
        specs = make_sample_scenes(123)
        resolutions = {(spec["scene"].width, spec["scene"].height) for spec in specs}
        counts = {len(spec["scene"].media) for spec in specs}
        self.assertGreaterEqual(len(resolutions), 3)
        self.assertEqual(counts, {1, 2, 3})

    def test_gui_constructs_offscreen(self) -> None:
        try:
            from PyQt5.QtWidgets import QApplication
        except Exception as exc:  # pragma: no cover - environment dependent
            self.skipTest(f"PyQt5 unavailable: {exc}")
        from volrender.gui import MainWindow
        from volrender.samples import default_sample_config

        app = QApplication.instance() or QApplication([])
        scene = make_sample_scenes()[0]["scene"]
        window = MainWindow(scene, default_sample_config(scene))
        self.assertIn("Volume Light Preview", window.windowTitle())
        self.assertIsNotNone(window.preview)
        self.assertIsNotNone(window.budget_curve)
        window.close()
        self.assertIsNotNone(app)


if __name__ == "__main__":
    unittest.main()
