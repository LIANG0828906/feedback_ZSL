from __future__ import annotations

import importlib.util
import tempfile
import unittest
from pathlib import Path

from motion_workbench.generator import SampleSpec, generate_sample


class GeneratorTests(unittest.TestCase):
    def test_generator_and_baseline_are_deterministic(self):
        with tempfile.TemporaryDirectory() as first_dir, tempfile.TemporaryDirectory() as second_dir:
            first = generate_sample(
                first_dir,
                SampleSpec(frames=10, joints=6, conflict="none", seed=51),
                keep_baseline_session=False,
            )
            second = generate_sample(
                second_dir,
                SampleSpec(frames=10, joints=6, conflict="none", seed=51),
                keep_baseline_session=False,
            )
            self.assertEqual(
                Path(first["bvh_path"]).read_bytes(),
                Path(second["bvh_path"]).read_bytes(),
            )
            self.assertEqual(
                Path(first["constraints_path"]).read_bytes(),
                Path(second["constraints_path"]).read_bytes(),
            )
            self.assertEqual(
                first["baseline"]["residual_digest"],
                second["baseline"]["residual_digest"],
            )
            self.assertEqual(first["baseline"]["status"], "complete")
            self.assertTrue(first["baseline"]["frame_hashes"])

    def test_generator_emits_conflict_baseline(self):
        with tempfile.TemporaryDirectory() as directory:
            result = generate_sample(
                directory,
                SampleSpec(
                    frames=6, joints=5, conflict="joint_limit", seed=52
                ),
                keep_baseline_session=False,
            )
            self.assertEqual(result["baseline"]["status"], "conflict")
            self.assertIsNotNone(result["baseline"]["conflict"])


class GuiAvailabilityTests(unittest.TestCase):
    def test_gui_source_is_present(self):
        module = Path(__file__).resolve().parents[1] / "motion_workbench" / "gui.py"
        self.assertTrue(module.exists())
        self.assertIn("SkeletonViewport", module.read_text(encoding="utf-8"))
        self.assertIn("ResidualHeatmap", module.read_text(encoding="utf-8"))
        self.assertIn("resume_session", module.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
