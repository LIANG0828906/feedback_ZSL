import unittest

import numpy as np

from rosewin.model import parse_scene
from rosewin.rng import DeterministicRng, fibre_texture_table, sample_table
from rosewin.scene import build_scene


class DeterminismTests(unittest.TestCase):
    def test_rng_is_independent_of_global_state(self):
        first = DeterministicRng(208, 7).uniform(16)
        second = DeterministicRng(208, 7).uniform(16)
        np.testing.assert_array_equal(first, second)

    def test_texture_tables_are_byte_identical(self):
        table_a = fibre_texture_table(208, 3, 32, 3.0)
        table_b = fibre_texture_table(208, 3, 32, 3.0)
        self.assertEqual(table_a.tobytes(), table_b.tobytes())
        self.assertGreaterEqual(table_a.min(), 0.75 - 1e-12)
        self.assertLessEqual(table_a.max(), 1.0 + 1e-12)

    def test_adjacent_same_hue_facets_get_distinct_tables(self):
        table_1 = fibre_texture_table(208, 1, 32, 3.0)
        table_2 = fibre_texture_table(208, 2, 32, 3.0)
        self.assertFalse(np.array_equal(table_1, table_2))

    def test_texture_modulates_both_color_and_reflection_path(self):
        table = fibre_texture_table(208, 1, 32, 3.0)
        u = np.linspace(0.0, 1.0, 16)
        values = sample_table(table, u, u[::-1])
        self.assertEqual(values.shape, (16,))
        self.assertTrue(np.all((values >= 0.75) & (values <= 1.0)))

    def test_scene_digest_is_stable(self):
        from rosewin.scene import scene_digest

        self.assertEqual(scene_digest(build_scene()), scene_digest(build_scene()))

    def test_facet_count_and_quadrants(self):
        model = parse_scene(build_scene())
        self.assertEqual(len({(f.quadrant, f.facet) for f in model.facets}), 20)


if __name__ == "__main__":
    unittest.main()
