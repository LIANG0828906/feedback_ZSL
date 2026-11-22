from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from vector_workbench.generator import generate_samples
from vector_workbench.operations import run_acceptance


class AcceptanceTests(unittest.TestCase):
    def test_full_acceptance_report(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data = root / "data"
            artifacts = root / "artifacts"
            checkpoints = root / "checkpoints"
            generate_samples(data, 20261123)
            report = run_acceptance(data, artifacts, checkpoints)
            self.assertTrue(report["passed"], json.dumps(report, indent=2))
            self.assertTrue((artifacts / "validation_report.json").exists())
            self.assertTrue((artifacts / "hud_orbit_composite.png").exists())
            for scene in report["scenes"]:
                self.assertTrue(scene["three_paths"]["exact_pixel_equal"])
                self.assertTrue(scene["three_paths"]["commit_sequence_consistent"])
            self.assertTrue(report["anomaly_injection"]["passed"])
            self.assertTrue(report["snapshot_invalidation"]["passed"])
            self.assertTrue(report["diff_detection"]["passed"])


if __name__ == "__main__":
    unittest.main()

