import unittest

import numpy as np

from brocade_embroidery.sampling import (
    build_embroidery_table,
    build_hole_table,
)
from brocade_embroidery.scene_gen import generate_scene
from brocade_embroidery.rng import splitmix64, uniform_grid


class SamplingDeterminismTests(unittest.TestCase):
    def test_rng_is_order_independent_and_seeded(self):
        first = splitmix64(np.arange(1000, dtype=np.uint64))
        second = splitmix64(np.arange(1000, dtype=np.uint64))
        np.testing.assert_array_equal(first, second)
        grid_a = uniform_grid(123, 4, 64)
        grid_b = uniform_grid(123, 4, 64)
        self.assertEqual(grid_a.tobytes(), grid_b.tobytes())
        grid_c = uniform_grid(124, 4, 64)
        self.assertNotEqual(grid_a.tobytes(), grid_c.tobytes())

    def test_embroidery_table_byte_reproducible(self):
        table_a = build_embroidery_table(99, 2, 128)
        table_b = build_embroidery_table(99, 2, 128)
        self.assertEqual(table_a.to_bytes(), table_b.to_bytes())
        table_c = build_embroidery_table(100, 2, 128)
        self.assertNotEqual(table_a.to_bytes(), table_c.to_bytes())
        self.assertTrue(np.all((table_a.modulation >= 0)
                               & (table_a.modulation <= 1)))

    def test_hole_table_depends_only_on_seed_layer_resolution(self):
        table_a = build_hole_table(55, 1, 128, 0.32)
        table_b = build_hole_table(55, 1, 128, 0.32)
        self.assertEqual(table_a.to_bytes(), table_b.to_bytes())
        table_c = build_hole_table(55, 3, 128, 0.32)
        self.assertNotEqual(table_a.to_bytes(), table_c.to_bytes())
        self.assertTrue(np.all((table_a.coverage >= 0)
                               & (table_a.coverage <= 1)))

    def test_scene_generation_is_deterministic(self):
        scene_a = generate_scene(small=True, seed=7)
        scene_b = generate_scene(small=True, seed=7)
        self.assertEqual(scene_a, scene_b)


if __name__ == "__main__":
    unittest.main()
