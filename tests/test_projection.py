from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import json

from celadon_museum.meshgen import make_scene
from celadon_museum.pipeline import render_scene
from celadon_museum.scene import validate_scene


def projection_scene(gap: float):
    return validate_scene(
        make_scene(
            width=160,
            height=112,
            tile_size=32,
            plate_gap=gap,
            texture_resolution=64,
            radial_segments=20,
            rings=5,
        )
    )


class ProjectionTests(unittest.TestCase):
    def test_projection_centroid_moves_with_stack_gap(self):
        near_scene = projection_scene(0.19)
        far_scene = projection_scene(0.32)
        with tempfile.TemporaryDirectory() as near_dir, tempfile.TemporaryDirectory() as far_dir:
            render_scene(near_scene, near_dir, install_signal_handlers=False)
            render_scene(far_scene, far_dir, install_signal_handlers=False)
            near_report = json.loads((Path(near_dir) / "report.json").read_text())
            far_report = json.loads((Path(far_dir) / "report.json").read_text())
            near_projection = near_report["projection"]
            far_projection = far_report["projection"]
            self.assertGreater(near_projection["spot_count"], 0)
            self.assertGreater(far_projection["spot_count"], 0)
            near_centroid = near_projection["spots"][0]["centroid"]
            far_centroid = far_projection["spots"][0]["centroid"]
            distance = (
                (near_centroid[0] - far_centroid[0]) ** 2
                + (near_centroid[1] - far_centroid[1]) ** 2
            ) ** 0.5
            self.assertGreater(distance, 1.0)

    def test_projection_centroid_moves_with_light_height(self):
        scene = projection_scene(0.19)
        changed_document = json.loads(json.dumps(scene.document))
        for light in changed_document["lights"]:
            light["position"][2] -= 1.0
        changed_scene = validate_scene(changed_document)
        with tempfile.TemporaryDirectory() as base_dir, tempfile.TemporaryDirectory() as high_dir:
            render_scene(scene, base_dir, install_signal_handlers=False)
            render_scene(changed_scene, high_dir, install_signal_handlers=False)
            base = json.loads((Path(base_dir) / "report.json").read_text())
            high = json.loads((Path(high_dir) / "report.json").read_text())
            base_centroid = base["projection"]["spots"][0]["centroid"]
            high_centroid = high["projection"]["spots"][0]["centroid"]
            distance = (
                (base_centroid[0] - high_centroid[0]) ** 2
                + (base_centroid[1] - high_centroid[1]) ** 2
            ) ** 0.5
            self.assertGreater(distance, 0.5)

    def test_penumbra_width_grows_with_gap(self):
        scene = projection_scene(0.19)
        with tempfile.TemporaryDirectory() as output:
            render_scene(scene, output, install_signal_handlers=False)
            report = json.loads((Path(output) / "report.json").read_text())
            trend = report["projection"]["penumbra_trend"]
            self.assertEqual(len(trend), 3)
            widths = [entry["width_10_90"] for entry in trend]
            self.assertLess(widths[0], widths[1])
            self.assertLess(widths[1], widths[2])
            for entry in trend:
                self.assertEqual(entry["soft_shadow_samples"], scene.render["soft_shadow_samples"])
                self.assertGreaterEqual(len(entry["samples"]), 3)


if __name__ == "__main__":
    unittest.main()
