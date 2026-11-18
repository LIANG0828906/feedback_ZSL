from __future__ import annotations

import unittest

import numpy as np

from gallery_louvers.patterns import build_patterns
from gallery_louvers.projection import ProjectionSystem

from .helpers import small_scene


class ProjectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = small_scene()
        cls.patterns = build_patterns(cls.scene)
        cls.projection = ProjectionSystem(cls.scene, cls.patterns)

    def test_energy_is_non_negative_and_order_independent(self) -> None:
        points = np.asarray(
            [
                [0.0, 2.0, -0.68],
                [-0.8, 1.4, -0.68],
                [0.0, 0.0, 0.8],
                [0.9, 0.0, 1.4],
            ]
        )
        normals = np.asarray(
            [[0, 0, 1], [0, 0, 1], [0, 1, 0], [0, 1, 0]], dtype=np.float64
        )
        canonical = self.projection.light_contributions(points, normals)
        reversed_order = self.projection.light_contributions(
            points,
            normals,
            light_order=list(reversed([light.id for light in self.scene.lights])),
        )
        self.assertTrue(np.all(canonical.total >= 0.0))
        self.assertTrue(
            np.array_equal(canonical.total, reversed_order.total)
        )
        for light_id in canonical.by_light:
            self.assertTrue(
                np.array_equal(
                    canonical.by_light[light_id],
                    reversed_order.by_light[light_id],
                )
            )
        individual_sum = sum(canonical.by_light.values())
        self.assertTrue(np.all(canonical.total <= individual_sum + 1e-12))
        positive = canonical.total > 1e-12
        ratios = np.stack(
            [
                canonical.by_light[light_id][positive] / canonical.total[positive]
                for light_id in sorted(canonical.by_light)
            ],
            axis=1,
        )
        self.assertLessEqual(
            float(np.max(np.abs(np.sum(ratios, axis=1) - 1.0))),
            1e-6,
        )

    def test_repeat_grayscale_is_byte_identical(self) -> None:
        points = np.asarray([[0.0, 2.0, -0.68], [0.7, 1.2, -0.68]])
        normals = np.tile(np.asarray([0.0, 0.0, 1.0]), (2, 1))
        first = self.projection.light_contributions(points, normals)
        second = self.projection.light_contributions(points, normals)
        self.assertEqual(first.total.tobytes(), second.total.tobytes())

    def test_softness_width_monotonic_in_both_distances(self) -> None:
        base = float(self.projection.softness_width(2.0, 1.0))
        farther_receiver = float(self.projection.softness_width(2.0, 1.5))
        farther_light = float(self.projection.softness_width(2.5, 1.0))
        self.assertGreaterEqual(farther_receiver, base)
        self.assertGreaterEqual(farther_light, base)

    def test_landing_coordinates_are_camera_independent(self) -> None:
        first = self.projection.landing_points()
        digest = self.projection.landing_digest(first)
        second = self.projection.landing_points()
        self.assertEqual(digest, self.projection.landing_digest(second))
        self.assertGreater(len(first), 0)
        for record in first:
            for receiver in record["receivers"]:
                self.assertGreaterEqual(receiver["contribution"], 0.0)


if __name__ == "__main__":
    unittest.main()
