import unittest

import numpy as np

from kiln_glass import xrand
from kiln_glass.fringes import evaluate_fringes, generate_fringe_params


class DeterminismTests(unittest.TestCase):
    def test_splitmix_known_value(self):
        # Fixed golden value guards the hash implementation against drift.
        out = xrand.splitmix64(np.array([0], dtype=np.uint64))
        self.assertEqual(int(out[0]), 0xE220A8397B1DCDAF)

    def test_uniform_pure_function(self):
        idx = np.arange(64, dtype=np.uint64)
        first = xrand.uniform(7, 3, xrand.CH_PHASE, idx)
        second = xrand.uniform(7, 3, xrand.CH_PHASE, idx)
        np.testing.assert_array_equal(first, second)
        self.assertTrue(np.all((first >= 0.0) & (first < 1.0)))

    def test_independent_streams_and_bricks(self):
        idx = np.arange(32, dtype=np.uint64)
        a = xrand.uniform(7, 3, xrand.CH_PHASE, idx)
        b = xrand.uniform(7, 4, xrand.CH_PHASE, idx)
        c = xrand.uniform(7, 3, xrand.CH_HUE, idx)
        self.assertFalse(np.array_equal(a, b))
        self.assertFalse(np.array_equal(a, c))

    def test_fringe_byte_identical_regeneration(self):
        first = generate_fringe_params(991, 12, 8)
        second = generate_fringe_params(991, 12, 8)
        np.testing.assert_array_equal(first.bubbles, second.bubbles)
        self.assertEqual(first.to_row(), second.to_row())

    def test_fringe_differs_between_bricks(self):
        p1 = generate_fringe_params(991, 12, 8)
        p2 = generate_fringe_params(991, 13, 8)
        xy = np.stack(np.meshgrid(np.linspace(0, 1, 24),
                                  np.linspace(0, 1, 24)), axis=-1)
        m1, _ = evaluate_fringes(p1, xy)
        m2, _ = evaluate_fringes(p2, xy)
        self.assertGreater(np.abs(m1 - m2).mean(), 1e-4)

    def test_fringe_independent_of_traversal_order(self):
        params = generate_fringe_params(42, 5, 6)
        rng = np.random.default_rng(0)
        xy_a = rng.random((128, 2))
        order = rng.permutation(128)
        mod_a, bubble_a = evaluate_fringes(params, xy_a)
        mod_b, bubble_b = evaluate_fringes(params, xy_a[order])
        np.testing.assert_allclose(mod_a[order], mod_b, rtol=0, atol=1e-12)
        np.testing.assert_array_equal(bubble_a[order], bubble_b)

    def test_golden_spiral_deterministic_and_bounded(self):
        s1 = xrand.golden_spiral_samples(24)
        s2 = xrand.golden_spiral_samples(24)
        np.testing.assert_array_equal(s1, s2)
        self.assertTrue(np.all(np.linalg.norm(s1, axis=1) <= 1.0))


if __name__ == "__main__":
    unittest.main()
