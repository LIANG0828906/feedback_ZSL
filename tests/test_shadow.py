from __future__ import annotations

import hashlib
import unittest

import numpy as np

from rosewindow.geometry import build_geometry
from rosewindow.scene import parse_scene
from rosewindow.shadow import build_shadow

from tests.helpers import small_scene_raw


class ShadowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scene = parse_scene(small_scene_raw())
        cls.geometry = build_geometry(cls.scene)
        cls.shadow = build_shadow(cls.scene, cls.geometry.edges)

    def test_projection_list_contains_depth_offsets(self):
        self.assertGreater(len(self.shadow.projections), 0)
        for projection in self.shadow.projections:
            self.assertEqual(len(projection["landing_a"]), 2)
            self.assertEqual(len(projection["landing_a_at_depth"]), 2)
            self.assertEqual(len(projection["depth_offset"]), 2)

    def test_edge_transition_is_soft_and_deterministic(self):
        profile = self.shadow.profile
        intermediate = 0
        for depth_entry in profile["depths"]:
            values = [sample["gray"] for sample in depth_entry["samples"]]
            intermediate += sum(0.08 < value < 0.92 for value in values)
        self.assertGreater(intermediate, 0)
        second = build_shadow(self.scene, self.geometry.edges)
        np.testing.assert_array_equal(self.shadow.coverage, second.coverage)
        self.assertEqual(self.shadow.profile["profile_digest"], second.profile["profile_digest"])
        self.assertEqual(
            hashlib.sha256(np.asarray(self.shadow.coverage, dtype=np.float32).tobytes()).hexdigest(),
            hashlib.sha256(np.asarray(second.coverage, dtype=np.float32).tobytes()).hexdigest(),
        )

    def test_transition_width_grows_with_lead_depth(self):
        widths = [
            entry["transition_width"] for entry in self.shadow.profile["depths"]
        ]
        self.assertGreaterEqual(len(widths), 3)
        self.assertTrue(
            all(widths[index] < widths[index + 1] for index in range(len(widths) - 1)),
            widths,
        )
        self.assertTrue(
            self.shadow.profile["transition_width_monotonic_with_depth"]
        )


if __name__ == "__main__":
    unittest.main()
