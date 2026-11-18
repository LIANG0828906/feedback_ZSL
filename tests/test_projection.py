from __future__ import annotations

import tempfile
import unittest
import json
from pathlib import Path

import numpy as np

from leather_raster.projection import build_projection_data, penumbra_stats
from leather_raster.runner import run_render
from leather_raster.scene import validate_scene_object

from .support import write_scene


class ProjectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.scene_path, self.raw = write_scene(
            self.root,
            width=96,
            height=72,
            block_width=32,
            block_height=24,
        )
        self.scene = validate_scene_object(self.raw)
        self.projection = build_projection_data(self.scene)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_every_light_has_adjacent_and_screen_cases(self) -> None:
        for light_index, light in enumerate(self.scene.lights):
            cases = [case for case in self.projection.cases if case.light_index == light_index]
            self.assertTrue(cases)
            receivers = {case.receiver_key for case in cases if case.status == "ok"}
            self.assertIn("screen", receivers)
            self.assertTrue(any(key.startswith("layer:") for key in receivers))
        report = self.projection.report_dict(self.scene)
        self.assertEqual(len(report["lights"]), 3)
        self.assertTrue(all(light["projections"] for light in report["lights"]))

    def test_three_light_overlap_and_out_of_bounds(self) -> None:
        coverage = self.projection.screen_light_coverage
        active = np.sum(coverage >= 0.1, axis=0)
        self.assertGreater(int(np.count_nonzero(active >= 2)), 0)
        statuses = {case.status for case in self.projection.cases}
        self.assertIn("outside_receiver", statuses)
        for light_index in range(3):
            self.assertGreater(float(np.max(coverage[light_index])), 0.2)

    def test_penumbra_grows_with_receiver_depth_and_light_distance(self) -> None:
        stats = penumbra_stats(self.projection, self.scene)
        checks = stats["trend_checks"]
        self.assertGreater(checks["receiver_gap_pairs_checked"], 0)
        self.assertTrue(checks["receiver_gap_non_decreasing"])
        self.assertTrue(checks["light_distance_non_decreasing_raw_world_width"])
        self.assertTrue(checks["light_distance_non_decreasing_radius_normalized_width"])
        comparison = stats["comparisons"][0]
        self.assertGreater(comparison["screen_width_output_px"], comparison["adjacent_width_output_px"])
        profile = comparison["profiles"][0]["samples"]
        values = [sample["coverage"] for sample in profile]
        self.assertLess(values[0], values[-1])
        self.assertAlmostEqual(values[2], 0.5, places=6)

    def test_projection_outputs_are_repeatable_at_same_sampling_density(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, raw = write_scene(root, width=64, height=48, block_width=32, block_height=24)
            scene = validate_scene_object(raw)
            run_render(scene, root / "a", fresh=True)
            run_render(scene, root / "b", fresh=True)
            self.assertEqual(
                (root / "a" / "projection_points.json").read_bytes(),
                (root / "b" / "projection_points.json").read_bytes(),
            )
            stats_a = json.loads((root / "a" / "stats.json").read_text())
            stats_b = json.loads((root / "b" / "stats.json").read_text())
            self.assertEqual(stats_a["projection"]["penumbra"], stats_b["projection"]["penumbra"])


if __name__ == "__main__":
    unittest.main()

