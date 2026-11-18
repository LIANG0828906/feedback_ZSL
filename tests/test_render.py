from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from glass_verify.raster import _compose_transmission_reflection

from tests.helpers import run_cli, write_json
from glass_verify.sample import make_sample_scene


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.scene_path = cls.root / "scene.json"
        cls.output = cls.root / "render"
        write_json(cls.scene_path, make_sample_scene())
        result = run_cli(
            "render",
            "--scene",
            str(cls.scene_path),
            "--out",
            str(cls.output),
            "--fresh",
        )
        if result.returncode != 0:
            raise AssertionError(result.stdout + "\n" + result.stderr)
        with Image.open(cls.output / "color.png") as image:
            cls.rgb = np.asarray(image.convert("RGB"), dtype=np.uint8)
        with Image.open(cls.output / "attribution.png") as image:
            cls.labels = np.asarray(image, dtype=np.uint16)
        cls.stats = json.loads((cls.output / "stats.json").read_text(encoding="utf-8"))
        cls.attribution = json.loads(
            (cls.output / "attribution.json").read_text(encoding="utf-8")
        )
        cls.projections = json.loads(
            (cls.output / "projection_hits.json").read_text(encoding="utf-8")
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_images_and_attribution_cover_every_pixel(self) -> None:
        self.assertEqual(self.rgb.shape, (120, 180, 3))
        self.assertEqual(self.labels.shape, (120, 180))
        self.assertEqual(self.labels.dtype, np.uint16)
        mapping = {
            entry["value"]: (entry["sector_id"], entry["pane_id"])
            for entry in self.attribution["labels"]
        }
        for value in np.unique(self.labels):
            if value >= 2:
                self.assertIn(int(value), mapping)

    def test_blue_and_red_bands_preserve_hue(self) -> None:
        mapping = {
            (entry["sector_id"], entry["pane_id"]): entry["value"]
            for entry in self.attribution["labels"]
        }
        blue_label = mapping[(0, 1)]
        red_label = mapping[(1, 1)]
        blue_pixels = self.rgb[self.labels == blue_label].astype(np.float64)
        red_pixels = self.rgb[self.labels == red_label].astype(np.float64)
        self.assertGreater(blue_pixels.shape[0], 20)
        self.assertGreater(red_pixels.shape[0], 20)
        blue_mean = blue_pixels.mean(axis=0)
        red_mean = red_pixels.mean(axis=0)
        self.assertGreater(blue_mean[2], blue_mean[1] * 1.25)
        self.assertGreater(blue_mean[2], blue_mean[0] * 2.0)
        self.assertLess(float(np.max(blue_pixels.mean(axis=0))), 250.0)
        self.assertGreater(red_mean[0], red_mean[2] * 1.8)
        self.assertGreater(red_mean[0], 100.0)
        self.assertFalse(np.any(np.all(self.rgb[self.labels >= 2] >= 250, axis=1)))
        self.assertLess(int(self.rgb.max()), 255)

    def test_reflection_and_transmission_are_separate(self) -> None:
        for pane in self.stats["panes"]:
            total = pane["reflection_fraction"] + pane["transmission_fraction"]
            self.assertAlmostEqual(total, 1.0, places=10)
            self.assertGreater(pane["reflection_contribution"], 0.0)
            self.assertGreater(pane["transmission_contribution"], 0.0)
        transmission = np.asarray([[0.4, 0.3, 0.2]])
        reflection = np.asarray([[0.2, 0.1, 0.05]])
        first = _compose_transmission_reflection(
            transmission, reflection, "transmission_then_reflection"
        )
        second = _compose_transmission_reflection(
            transmission, reflection, "reflection_then_transmission"
        )
        self.assertFalse(np.array_equal(first, second))
        self.assertTrue(np.all(first >= 0.0) and np.all(first <= 1.0))
        self.assertTrue(np.all(second >= 0.0) and np.all(second <= 1.0))

    def test_projection_points_are_recomputable(self) -> None:
        direction = np.asarray(self.projections["light_direction"], dtype=np.float64)
        self.assertGreater(self.projections["hit_count"], 0)
        for hit in self.projections["hits"][:20]:
            source = np.asarray(hit["source_point"], dtype=np.float64)
            expected = source + hit["light_distance"] * direction
            actual = np.asarray(hit["wall_point"], dtype=np.float64)
            self.assertTrue(np.allclose(expected, actual, atol=1.0e-9))

    def test_stats_are_stable_and_complete(self) -> None:
        self.assertEqual(
            list(self.stats),
            [
                "schema_version",
                "renderer_version",
                "scene_digest",
                "image",
                "chunks",
                "recovery",
                "texture",
                "projection",
                "sectors",
                "panes",
            ],
        )
        self.assertTrue(self.stats["chunks"]["complete"])
        self.assertEqual(self.stats["chunks"]["completed_count"], 8)
        self.assertGreater(self.stats["projection"]["soft_shadow_sample_count"], 0)
        self.assertGreater(self.stats["sectors"][0]["visible_fragments"], 0)
        self.assertGreater(self.stats["sectors"][0]["mean_transmittance"], 0.0)
        stats_text = (self.output / "stats.json").read_text(encoding="utf-8")
        self.assertNotIn("/home/", stats_text)
        self.assertNotIn("timestamp", stats_text.lower())
        trend = json.loads(
            (self.output / "shadow_profiles.json").read_text(encoding="utf-8")
        )["depth_trend"]
        widths = [entry["transition_width_world"] for entry in trend]
        self.assertEqual(widths, sorted(widths))

    def test_compare_reports_chunk_coordinate_and_channel(self) -> None:
        altered = self.root / "altered"
        altered.mkdir()
        for name in ("color.png", "attribution.png", "projection_hits.json"):
            (altered / name).write_bytes((self.output / name).read_bytes())
        image = self.rgb.copy()
        image[66, 90] = np.asarray([255, 0, 127], dtype=np.uint8)
        Image.fromarray(image).save(altered / "color.png")
        result = run_cli(
            "compare",
            "--scene",
            str(self.scene_path),
            "--reference",
            str(self.output),
            "--candidate",
            str(altered),
            "--max-differences",
            "4",
        )
        self.assertEqual(result.returncode, 3)
        payload = json.loads(result.stdout)
        self.assertFalse(payload["ok"])
        self.assertTrue(payload["differences"])
        difference = payload["differences"][0]
        self.assertIn("chunk_id", difference)
        self.assertIn("x", difference)
        self.assertIn("y", difference)
        self.assertIn("channel", difference)


if __name__ == "__main__":
    unittest.main()
