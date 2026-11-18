from __future__ import annotations

import hashlib
import unittest

import numpy as np

from fogvol.density import generate_density


class DensityTests(unittest.TestCase):
    def setUp(self) -> None:
        self.kwargs = {
            "seed": 212,
            "dimensions": (18, 12, 24),
            "origin": np.asarray([0.0, 0.0, 0.0]),
            "voxel_size": np.asarray([0.5, 0.5, 0.5]),
            "density_range": (0.0, 1.0),
            "noise": {
                "octaves": 4,
                "base_frequency": 0.11,
                "lacunarity": 2.0,
                "gain": 0.5,
            },
        }

    def test_seeded_density_is_byte_identical(self) -> None:
        first = generate_density(**self.kwargs)
        np.random.seed(1)
        second = generate_density(**self.kwargs)
        np.random.seed(999)
        third = generate_density(**self.kwargs)
        digest = hashlib.sha256(first.tobytes()).hexdigest()
        self.assertEqual(digest, hashlib.sha256(second.tobytes()).hexdigest())
        self.assertEqual(digest, hashlib.sha256(third.tobytes()).hexdigest())
        self.assertTrue(np.array_equal(first, second))
        self.assertTrue(np.array_equal(first, third))

    def test_field_is_finite_and_has_floor_to_top_profile(self) -> None:
        density = generate_density(**self.kwargs)
        self.assertTrue(np.all(np.isfinite(density)))
        self.assertGreaterEqual(float(density.min()), 0.0)
        self.assertLessEqual(float(density.max()), 1.0)
        floor_mean = float(density[:, :3, :].mean())
        top_mean = float(density[:, -3:, :].mean())
        self.assertGreater(floor_mean, top_mean)
        self.assertGreater(floor_mean, 0.1)
        self.assertLess(top_mean, 0.35)


if __name__ == "__main__":
    unittest.main()

