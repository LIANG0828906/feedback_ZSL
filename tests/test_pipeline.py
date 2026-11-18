import json
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from rosewin.model import parse_scene
from rosewin.pipeline import RenderPipeline
from rosewin.projection import WallProjector
from rosewin.rasterizer import Rasterizer
from rosewin.scene import build_scene, scene_digest
from rosewin.verify import compare_output_dirs, run_recovery_check


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.model = parse_scene(build_scene())
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self):
        self.temp.cleanup()

    def test_full_render_layer_shapes_and_no_overflow(self):
        pipeline = RenderPipeline(self.model, self.root / "render")
        artifacts = pipeline.run()
        self.assertEqual(artifacts.status, "complete")
        rgb = np.asarray(Image.open(self.root / "render" / "render.png"))
        self.assertEqual(rgb.shape, (256, 256, 3))
        self.assertLessEqual(int(rgb.max()), 255)
        owner = np.asarray(Image.open(self.root / "render" / "owner.png"))
        self.assertEqual(set(np.unique(owner)) - {0}, set(range(1, 21)))
        report = json.loads((self.root / "render" / "run_report.json").read_text())
        self.assertEqual(report["schema_version"], "cathedral-rose-report/v1")
        self.assertEqual(report["chunks"]["completed_count"], 8)

    def test_blue_and_red_remain_distinguishable(self):
        RenderPipeline(self.model, self.root / "render").run()
        rgb = np.asarray(Image.open(self.root / "render" / "render.png"))
        owner = np.asarray(Image.open(self.root / "render" / "owner.png"))
        blue = rgb[owner == 1].mean(axis=0)
        red = rgb[owner == 6].mean(axis=0)
        self.assertGreater(blue[2], blue[0])
        self.assertGreater(red[0], red[2])

    def test_reflection_and_transmission_ratios_are_separated_and_bounded(self):
        RenderPipeline(self.model, self.root / "render").run()
        report = json.loads((self.root / "render" / "run_report.json").read_text())
        for facet in report["facets"]:
            self.assertAlmostEqual(facet["transmission_ratio"] + facet["reflection_ratio"], 1.0, places=6)
            self.assertGreaterEqual(facet["reflection_ratio"], 0.0)

    def test_soft_shadow_width_grows_with_lead_depth(self):
        projector = WallProjector(self.model)
        result = projector.render()
        widths = [sample["width_world"] for sample in result.edge_width_samples]
        self.assertEqual(len(widths), 3)
        self.assertTrue(all(left < right for left, right in zip(widths, widths[1:])))

    def test_recovery_is_bytewise_identical(self):
        report = run_recovery_check(self.model, self.root / "recovery", interrupt_after=2)
        self.assertEqual(report["status"], "pass")
        diff = compare_output_dirs(self.root / "recovery" / "baseline", self.root / "recovery" / "recovery")
        self.assertTrue(diff["equal"])

    def test_corrupt_snapshot_is_discarded_and_rerun_matches(self):
        render_dir = self.root / "render"
        RenderPipeline(self.model, render_dir).run()
        baseline = np.asarray(Image.open(render_dir / "render.png"))
        snapshot = render_dir / "render.snapshot.npz"
        snapshot.write_bytes(b"not-an-npz")
        artifacts = RenderPipeline(self.model, render_dir).run()
        self.assertEqual(artifacts.status, "complete")
        rerun = np.asarray(Image.open(render_dir / "render.png"))
        np.testing.assert_array_equal(baseline, rerun)

    def test_scene_digest_mismatch_discards_snapshot(self):
        render_dir = self.root / "render"
        pipeline = RenderPipeline(self.model, render_dir)
        pipeline.run()
        changed = parse_scene(build_scene(seed=209))
        artifacts = RenderPipeline(changed, render_dir).run()
        self.assertEqual(artifacts.status, "complete")


if __name__ == "__main__":
    unittest.main()
