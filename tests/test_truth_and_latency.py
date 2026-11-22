from __future__ import annotations

import json
import statistics
import tempfile
import time
import unittest

import numpy as np

from npr_workbench.engine import NprEngine
from npr_workbench.samples import truth_params, verify_truth

from tests.helpers import ROOT, sample_scene


class TruthAndLatencyTests(unittest.TestCase):
    def test_generated_truth_samples_match_render(self) -> None:
        scene = sample_scene("orb_and_crate")
        truth = json.loads(
            (ROOT / "data" / "truth" / "orb_and_crate.truth.json").read_text(
                encoding="utf-8"
            )
        )
        report = verify_truth(scene, truth)
        self.assertTrue(report["passed"], report)
        self.assertGreaterEqual(report["checked_samples"], 100)

    def test_preview_p95_under_16ms_at_default_samples(self) -> None:
        scene = sample_scene("orb_and_crate")
        with tempfile.TemporaryDirectory() as directory:
            engine = NprEngine(
                scene,
                truth_params(),
                scene.camera_poses[0],
                persist_dir=directory,
                diagnostics_dir=directory + "/diagnostics",
            )
            for _ in range(5):
                engine.render_preview()
            rounds: list[tuple[float, float]] = []
            for _ in range(3):
                durations = [engine.render_preview().duration_ms for _ in range(25)]
                rounds.append(
                    (
                        float(statistics.median(durations)),
                        float(np.percentile(durations, 95)),
                    )
                )
                time.sleep(0.05)
        best_median, best_p95 = min(rounds, key=lambda item: item[1])
        self.assertLessEqual(
            best_p95,
            16.0,
            f"best_median={best_median:.3f} best_p95={best_p95:.3f} rounds={rounds}",
        )


if __name__ == "__main__":
    unittest.main()

