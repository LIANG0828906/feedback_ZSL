import filecmp
import unittest
from pathlib import Path

import numpy as np

from space_audio.demo import build_demo_scene
from space_audio.geometry import build_scene_geometry, evaluate_all
from space_audio.synthesis import synthesize_dry


class DeterminismTest(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = build_demo_scene()

    def test_dry_signals_byte_identical(self) -> None:
        first = synthesize_dry("organ", self.scene["project"]["seed"], "organ", 8.0, 24000)
        second = synthesize_dry("organ", self.scene["project"]["seed"], "organ", 8.0, 24000)
        np.testing.assert_array_equal(first, second)

    def test_geometry_occlusion_reproducible(self) -> None:
        geometries = build_scene_geometry(self.scene["layers"], self.scene["project"]["seed"])
        source = np.array(self.scene["sources"][0]["position"], dtype=np.float64)
        # Pick a listener point known to place the sight line through a solid:
        # fence plane x=3.5, dense bars; organ is at x=-13.
        listener = np.array([8.0, 0.0, 1.7], dtype=np.float64)
        first = evaluate_all(geometries, source, listener)
        second = evaluate_all(
            build_scene_geometry(self.scene["layers"], self.scene["project"]["seed"]),
            source,
            listener,
        )
        self.assertEqual(
            [(r["occluded"], round(r["detour"], 9), r["solid"]) for r in first],
            [(r["occluded"], round(r["detour"], 9), r["solid"]) for r in second],
        )
        if not any(result["occluded"] for result in first):
            # Fall back to scanning several points so the assertion stays meaningful.
            found = False
            for y in np.linspace(-3.0, 3.0, 40):
                probe = evaluate_all(geometries, source, np.array([8.0, y, 1.7]))
                if any(r["occluded"] for r in probe):
                    found = True
                    break
            self.assertTrue(found)

    def test_source_order_independence(self) -> None:
        from space_audio.model import build_frame_table

        import copy

        reversed_scene = copy.deepcopy(self.scene)
        reversed_scene["sources"] = list(reversed(reversed_scene["sources"]))
        table_a = build_frame_table(self.scene)
        table_b = build_frame_table(reversed_scene)
        for source_id in table_a["sources"]:
            np.testing.assert_allclose(
                table_a["sources"][source_id]["transmission"],
                table_b["sources"][source_id]["transmission"],
            )


if __name__ == "__main__":
    unittest.main()

