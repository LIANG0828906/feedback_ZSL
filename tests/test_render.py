from __future__ import annotations

import copy
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np
from PIL import Image

from serene_render.compare import compare_outputs
from serene_render.pipeline import run_render
from serene_render.scene import make_sample_scene, parse_scene, write_sample_scene


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.scene_path = cls.root / "scene.json"
        write_sample_scene(cls.scene_path)
        cls.out_full = cls.root / "full"
        run_render(cls.scene_path, cls.out_full)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_outputs_and_report_are_complete(self) -> None:
        report = json.loads((self.out_full / "report.json").read_text())
        rgba = np.load(self.out_full / "render_rgba.npy", allow_pickle=False)
        layer_map = np.load(self.out_full / "layer_map.npy", allow_pickle=False)
        scene = parse_scene(make_sample_scene())
        self.assertEqual(rgba.shape, (scene.height, scene.width, 4))
        self.assertEqual(layer_map.shape, (scene.height, scene.width))
        self.assertEqual(report["scene_digest"], scene.digest)
        self.assertEqual(len(report["layers"]), 5)
        self.assertEqual(report["shadows"]["projection_case_count"], 4)
        self.assertGreater(report["shadows"]["mullion_projection_sample_points"], 0)
        self.assertEqual(
            report["final_image_sha256"],
            __import__("hashlib").sha256(rgba.tobytes()).hexdigest(),
        )
        with Image.open(self.out_full / "render.png") as image:
            self.assertEqual(image.mode, "RGBA")
            self.assertEqual(image.size, (scene.width, scene.height))
        with Image.open(self.out_full / "layer_map.png") as image:
            self.assertEqual(image.mode, "RGBA")

    def test_no_white_highlight_and_layers_have_color(self) -> None:
        rgba = np.load(self.out_full / "render_rgba.npy", allow_pickle=False)
        layer_map = np.load(self.out_full / "layer_map.npy", allow_pickle=False)
        rendered_surface = layer_map >= 0
        self.assertGreater(int(rendered_surface.sum()), 0)
        white = np.all(rgba[..., :3] > 250, axis=2)
        self.assertEqual(int(np.sum(white & rendered_surface)), 0)
        silk_pixels = rgba[rendered_surface][:, :3].astype(np.float64)
        chroma = silk_pixels.max(axis=1) - silk_pixels.min(axis=1)
        self.assertGreater(float(np.mean(chroma)), 5.0)

    def test_second_fresh_run_is_byte_identical(self) -> None:
        second = self.root / "second"
        run_render(self.scene_path, second, fresh=True)
        self.assertEqual(compare_outputs(self.out_full, second), [])
        for relative in (
            "render_rgba.npy",
            "render.png",
            "layer_map.npy",
            "layer_map.png",
            "projection_points.json",
            "report.json",
        ):
            self.assertEqual(
                (self.out_full / relative).read_bytes(),
                (second / relative).read_bytes(),
                relative,
            )

    def test_layer_json_order_does_not_change_pixels(self) -> None:
        raw = copy.deepcopy(make_sample_scene())
        raw["layers"] = list(reversed(raw["layers"]))
        reordered_path = self.root / "reordered.json"
        reordered_path.write_text(json.dumps(raw), encoding="utf-8")
        reordered_out = self.root / "reordered"
        run_render(reordered_path, reordered_out, fresh=True)
        first = np.load(self.out_full / "render_rgba.npy", allow_pickle=False)
        second = np.load(reordered_out / "render_rgba.npy", allow_pickle=False)
        np.testing.assert_array_equal(first, second)

    def test_report_contains_contribution_ratios_and_penumbra_trend(self) -> None:
        report = json.loads((self.out_full / "report.json").read_text())
        for layer in report["layers"]:
            if layer["kind"] == "silk":
                self.assertGreater(layer["transmission_fraction"], 0.0)
                self.assertGreater(layer["reflection_fraction"], 0.0)
                self.assertAlmostEqual(
                    layer["transmission_fraction"] + layer["reflection_fraction"],
                    1.0,
                    places=12,
                )
        distance_widths = [
            item["penumbra_width"]
            for item in report["shadows"]["penumbra_trend"][
                "receiver_distance_cases"
            ]
        ]
        self.assertEqual(distance_widths, sorted(distance_widths))
        self.assertGreater(distance_widths[-1], distance_widths[0])


if __name__ == "__main__":
    unittest.main()
