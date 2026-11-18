import unittest

import numpy as np

from paperlightbox.rng import PCG32, value_noise_3d
from paperlightbox.texture import apply_fiber, cutout_triangle_mask, fiber_field


class DeterminismTests(unittest.TestCase):
    def test_pcg_sequence_is_stable(self):
        a, b = PCG32(12345), PCG32(12345)
        self.assertEqual([a.next_u32() for _ in range(64)],
                         [b.next_u32() for _ in range(64)])
        self.assertNotEqual(PCG32(1).next_u32(), PCG32(2).next_u32())

    def test_cutout_mask_byte_identical_and_seed_sensitive(self):
        first = cutout_triangle_mask(16, 16, 77)
        second = cutout_triangle_mask(16, 16, 77)
        self.assertEqual(first.dtype, bool)
        self.assertTrue(np.array_equal(first, second))
        self.assertFalse(
            np.array_equal(first, cutout_triangle_mask(16, 16, 78)))
        # Both triangles of a pierced cell share the cutout state.
        self.assertTrue(np.array_equal(first[0::2], first[1::2]))

    def test_fiber_texture_is_order_independent_and_distinctive(self):
        points = np.random.default_rng(0).random((128, 3)) * 2.0 - 1.0
        forward = fiber_field(points, 42)
        perm = np.random.default_rng(1).permutation(len(points))
        reordered = fiber_field(points[perm], 42)
        self.assertTrue(
            np.allclose(forward["mottle"][perm], reordered["mottle"]))

        _, color_a = apply_fiber(0.5, np.array([0.9, 0.2, 0.2]), points, 1)
        _, color_b = apply_fiber(0.5, np.array([0.9, 0.2, 0.2]), points, 2)
        self.assertFalse(np.allclose(color_a, color_b))

        trans, _ = apply_fiber(0.5, np.array([0.9, 0.2, 0.2]), points, 1)
        self.assertTrue(np.all((trans > 0.0) & (trans <= 1.0)))

    def test_value_noise_bounds(self):
        pts = np.linspace(-3.0, 3.0, 50).reshape(-1, 1)
        pts = np.concatenate([pts, pts * 0.3, pts * 0.7], axis=1)
        noise = value_noise_3d(pts, 99)
        self.assertEqual(noise.shape, (50,))
        self.assertTrue(np.all(noise >= 0.0) and np.all(noise < 1.0))


if __name__ == "__main__":
    unittest.main()
