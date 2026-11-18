import unittest

import numpy as np

from paperlightbox.geometry import (perspective_project_z, ray_triangles,
                                    triangle_area)


class GeometryTests(unittest.TestCase):
    def test_perspective_projection_onto_plane(self):
        light = np.array([0.0, 0.0, -1.0])
        point = np.array([0.5, 0.0, 0.0])
        projected = perspective_project_z(point, light, 1.0)
        self.assertEqual(projected[2], 1.0)
        self.assertAlmostEqual(projected[0], 1.0)
        self.assertAlmostEqual(projected[1], 0.0)

    def test_generic_ray_triangle_intersection(self):
        verts = np.array([
            [0.0, 0.0, 0.0], [1.0, 0.0, 0.0], [0.0, 1.0, 0.0],
            [1.0, 1.0, 0.0],
        ], dtype=np.float64)
        tris = np.array([[0, 1, 2], [1, 3, 2]], dtype=np.int64)
        origins = np.array([[0.25, 0.25, 1.0], [2.0, 2.0, 1.0]])
        dirs = np.array([[0.0, 0.0, -1.0], [0.0, 0.0, -1.0]])
        result = ray_triangles(origins, dirs, verts, tris)
        self.assertTrue(bool(result["hit"][0]))
        self.assertFalse(bool(result["hit"][1]))
        self.assertAlmostEqual(float(result["t"][0]), 1.0)

    def test_triangle_area_detects_degenerate(self):
        a = np.zeros(3)
        b = np.array([1.0, 0.0, 0.0])
        self.assertEqual(triangle_area(a, b, b), 0.0)
        self.assertGreater(triangle_area(a, b, np.array([0.0, 1.0, 0.0])), 0.0)


if __name__ == "__main__":
    unittest.main()
