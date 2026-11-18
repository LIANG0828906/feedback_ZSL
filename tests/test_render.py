from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from rosewindow.pngio import encode_rgb_png
from rosewindow.renderer import render_scene
from rosewindow.scene import parse_scene
from rosewindow.verify import compare_outputs

from tests.helpers import small_scene_raw


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.output = Path(cls.temporary.name) / "render"
        scene = parse_scene(small_scene_raw())
        cls.result = render_scene(scene, cls.output, fresh=True)
        cls.scene = scene
        cls.report = json.loads((cls.output / "report.json").read_text())
        cls.rgb = np.load(cls.output / "rgb.npy", allow_pickle=False)
        cls.owner = np.load(cls.output / "ownership.npy", allow_pickle=False)

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def test_transmission_and_reflection_checks(self):
        transmission = self.report["transmission"]
        self.assertTrue(transmission["forward_blue_not_pink"])
        self.assertTrue(transmission["lateral_red_hue_preserved"])
        forward = transmission["forward_blue_probe"]["mean_rgb_0_255"]
        lateral = transmission["lateral_red_probe"]["mean_rgb_0_255"]
        self.assertGreater(forward[2], forward[0])
        self.assertLess(forward[0], 180)
        self.assertGreater(lateral[0], lateral[1])
        self.assertGreater(lateral[0], lateral[2])
        self.assertGreater(max(lateral), 24)

    def test_reflection_contributions_and_no_overflow(self):
        self.assertTrue(self.report["image"]["channels_within_uint8"])
        self.assertLessEqual(int(self.rgb.max()), 255)
        self.assertEqual(self.rgb.dtype, np.uint8)
        pieces = self.report["reflection"]["piece_contributions"]
        self.assertEqual(len(pieces), len(self.scene.pieces))
        visible = [entry for entry in pieces if entry["pixels"] > 0]
        self.assertGreater(len(visible), 0)
        for entry in visible:
            self.assertAlmostEqual(
                entry["transmission_share"] + entry["reflection_share"],
                1.0,
                places=6,
            )
            self.assertGreaterEqual(entry["reflection_energy"], 0.0)

    def test_ownership_is_stable_serial_map(self):
        self.assertEqual(self.owner.shape, (self.scene.height, self.scene.width))
        self.assertEqual(int(self.owner.min()), 0)
        self.assertEqual(int(self.owner.max()), len(self.scene.pieces))
        mapping = json.loads((self.output / "ownership_map.json").read_text())
        self.assertEqual(len(mapping["pieces"]), len(self.scene.pieces))
        for entry in mapping["pieces"]:
            self.assertGreater(entry["serial"], 0)

    def test_shadow_report_has_profiles_and_landings(self):
        profile = self.report["lead_shadow"]["profile"]
        widths = [entry["transition_width"] for entry in profile["depths"]]
        self.assertEqual(widths, sorted(widths))
        for depth_entry in profile["depths"]:
            self.assertGreater(len(depth_entry["samples"]), 8)
        projections = json.loads(
            (self.output / "shadow_projections.json").read_text()
        )
        self.assertEqual(projections["count"], len(projections["landings"]))
        self.assertGreater(projections["count"], 0)

    def test_png_bytes_are_deterministic(self):
        first = encode_rgb_png(self.rgb)
        second = encode_rgb_png(self.rgb.copy())
        self.assertEqual(first, second)
        self.assertEqual(
            first,
            (self.output / "rgb.png").read_bytes(),
        )

    def test_verify_detects_structured_pixel_difference(self):
        with tempfile.TemporaryDirectory() as temporary:
            candidate = Path(temporary)
            for name in (
                "report.json",
                "rgb.npy",
                "ownership.npy",
                "shadow_projections.json",
                "ownership_map.json",
            ):
                (candidate / name).write_bytes((self.output / name).read_bytes())
            changed = self.rgb.copy()
            changed[10, 12, 1] = (int(changed[10, 12, 1]) + 1) % 256
            np.save(candidate / "rgb.npy", changed, allow_pickle=False)
            result = compare_outputs(self.output, candidate)
            self.assertFalse(result["ok"])
            first = result["differences"][0]
            self.assertEqual(first["pixel"], [12, 10])
            self.assertIn("G", first["channels"])
            self.assertIsInstance(first["chunk_id"], int)


if __name__ == "__main__":
    unittest.main()
