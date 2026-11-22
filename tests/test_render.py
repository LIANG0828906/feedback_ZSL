from __future__ import annotations

import copy
import json
import unittest

import numpy as np

from volrender.errors import BusinessError, ErrorCode
from volrender.hashing import canonical_json_bytes
from volrender.renderer import _check_seams, render_scene

from tests.helpers import config, tiny_scene


class RenderTests(unittest.TestCase):
    def test_volume_render_and_whole_screen_seam_truth(self) -> None:
        result = render_scene(tiny_scene(), config(), validate_seams=True)
        self.assertGreater(float(result.scatter.max()), 0.0)
        self.assertLess(float(result.transmittance.min()), 1.0)
        self.assertTrue(result.summary["seam_check"]["enabled"])
        self.assertEqual(result.summary["seam_check"]["max_scatter_delta"], 0.0)
        self.assertEqual(result.summary["seam_check"]["max_transmittance_delta"], 0.0)
        self.assertIn(b"P6\n", result.image_ppm)

    def test_two_identical_renders_are_byte_identical(self) -> None:
        scene = tiny_scene()
        cfg = config()
        first = render_scene(scene, cfg, validate_seams=False)
        second = render_scene(scene, cfg, validate_seams=False)
        self.assertEqual(first.image_ppm, second.image_ppm)
        self.assertEqual(first.summary["pixel_hash"], second.summary["pixel_hash"])
        self.assertEqual(first.summary["summary_hash"], second.summary["summary_hash"])
        self.assertEqual(
            canonical_json_bytes(first.trace), canonical_json_bytes(second.trace)
        )

    def test_fixed_and_float_paths_report_error_bound(self) -> None:
        scene = tiny_scene()
        cfg = config(precision="fixed")
        result = render_scene(
            scene,
            cfg,
            validate_seams=False,
            compare_precision=True,
        )
        stats = result.summary["precision_comparison"]
        self.assertEqual(result.summary["precision"], "fixed")
        self.assertGreaterEqual(stats["error_upper_bound"], stats["max_abs"])
        self.assertGreaterEqual(stats["max_abs"], 0.0)
        self.assertEqual(len(stats["per_channel_max_abs"]), 3)
        self.assertTrue(np.isfinite(result.scatter).all())

    def test_seam_mismatch_reports_pair_and_pixel(self) -> None:
        result = render_scene(tiny_scene(), config(), validate_seams=False)
        truth = copy.deepcopy(result.tile_results[0])
        truth.tile = result.tile_results[0].tile.__class__(
            0, "truth", 0, 0, tiny_scene().width, tiny_scene().height
        )
        height, width = truth.scatter.shape[:2]
        truth.scatter = np.zeros(
            (tiny_scene().height, tiny_scene().width, 3), dtype=np.float64
        )
        truth.transmittance = np.ones_like(truth.scatter)
        truth.scatter[:] = result.scatter
        truth.transmittance[:] = result.transmittance
        truth.scatter[7, 7, 0] += 1.0
        with self.assertRaises(BusinessError) as caught:
            _check_seams(tiny_scene(), result.tile_results, truth, config())
        self.assertEqual(caught.exception.code, ErrorCode.SEAM_UNRESOLVED)
        details = caught.exception.details
        self.assertEqual(len(details["tile_pair"]), 2)
        self.assertEqual(len(details["pixel"]), 2)
        self.assertTrue(details["scatter_delta"] > 0.0)


if __name__ == "__main__":
    unittest.main()
