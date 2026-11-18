from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from celadon_museum.meshgen import make_scene
from celadon_museum.pipeline import render_scene
from celadon_museum.scene import validate_scene


def small_scene():
    return validate_scene(
        make_scene(
            width=128,
            height=96,
            tile_size=32,
            texture_resolution=64,
            radial_segments=20,
            rings=5,
        )
    )


class RenderTests(unittest.TestCase):
    def test_repeat_render_and_input_order_are_byte_identical(self):
        scene = small_scene()
        reordered_document = copy.deepcopy(scene.document)
        reordered_document["plates"] = list(reversed(reordered_document["plates"]))
        reordered_document["layers"] = list(reversed(reordered_document["layers"]))
        reordered_document["lights"] = list(reversed(reordered_document["lights"]))
        reordered_scene = validate_scene(reordered_document)
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second, tempfile.TemporaryDirectory() as third:
            render_scene(scene, first, install_signal_handlers=False)
            render_scene(scene, second, install_signal_handlers=False)
            render_scene(reordered_scene, third, install_signal_handlers=False)
            first_npy = (Path(first) / "rgba.npy").read_bytes()
            second_npy = (Path(second) / "rgba.npy").read_bytes()
            third_npy = (Path(third) / "rgba.npy").read_bytes()
            self.assertEqual(first_npy, second_npy)
            self.assertEqual(first_npy, third_npy)
            self.assertEqual(
                (Path(first) / "rgba.png").read_bytes(),
                (Path(second) / "rgba.png").read_bytes(),
            )
            self.assertEqual(
                (Path(first) / "optical_depth.npy").read_bytes(),
                (Path(second) / "optical_depth.npy").read_bytes(),
            )
            self.assertEqual(
                (Path(first) / "projection_map.npy").read_bytes(),
                (Path(second) / "projection_map.npy").read_bytes(),
            )
            first_report = json.loads((Path(first) / "report.json").read_text())
            third_report = json.loads((Path(third) / "report.json").read_text())
            self.assertEqual(first_report["image"]["digest"], third_report["image"]["digest"])
            self.assertEqual(
                first_report["optical_depth"]["digest"],
                third_report["optical_depth"]["digest"],
            )

    def test_shading_avoids_clipped_white_and_keeps_cyan_sides(self):
        scene = small_scene()
        with tempfile.TemporaryDirectory() as output:
            render_scene(scene, output, install_signal_handlers=False)
            rgba = np.load(Path(output) / "rgba.npy")
            background = rgba[0, 0, :3]
            mask = np.any(rgba[:, :, :3] != background, axis=2)
            self.assertGreater(int(np.count_nonzero(mask)), 100)
            rgb = rgba[mask, :3].astype(np.float64)
            self.assertLess(int(rgb.max()), 250)
            self.assertGreater(float(np.mean(rgb[:, 1] > rgb[:, 0])), 0.98)
            self.assertGreater(float(np.mean(rgb[:, 2] > rgb[:, 0])), 0.98)
            depth = np.load(Path(output) / "optical_depth.npy")
            self.assertTrue(np.all(np.isfinite(depth)))
            self.assertGreater(float(np.max(depth)), 0.0)

    def test_report_is_stable_and_path_free(self):
        scene = small_scene()
        with tempfile.TemporaryDirectory() as output:
            render_scene(scene, output, install_signal_handlers=False)
            report_path = Path(output) / "report.json"
            text = report_path.read_text(encoding="utf-8")
            report = json.loads(text)
            self.assertEqual(
                list(report.keys()),
                [
                    "schema",
                    "status",
                    "scene",
                    "image",
                    "optical_depth",
                    "projection_map",
                    "tiles",
                    "layers",
                    "projection",
                    "recovery",
                    "verification",
                ],
            )
            self.assertNotIn(str(Path(output).resolve()), text)
            self.assertNotIn("timestamp", text.lower())
            self.assertEqual(report["status"], "complete")
            self.assertEqual(report["tiles"]["completed_count"], report["tiles"]["total"])
            self.assertTrue(report["verification"] is not None)


if __name__ == "__main__":
    unittest.main()
