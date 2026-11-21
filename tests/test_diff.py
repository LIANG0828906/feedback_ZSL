from __future__ import annotations

import unittest

import numpy as np

from pcwb.diff import diff_clouds
from pcwb.model import PointCloud


class DiffTests(unittest.TestCase):
    def test_added_removed_and_color_changed(self) -> None:
        old = PointCloud.from_arrays(
            np.asarray([[0, 0, 0], [1, 0, 0], [2, 0, 0]], dtype=np.float32),
            np.asarray([[10, 10, 10], [20, 20, 20], [30, 30, 30]], dtype=np.uint8),
        )
        new = PointCloud.from_arrays(
            np.asarray([[0, 0, 0], [1, 0, 0], [3, 0, 0]], dtype=np.float32),
            np.asarray([[10, 10, 10], [21, 21, 21], [40, 40, 40]], dtype=np.uint8),
        )
        result = diff_clouds(old, new)
        self.assertEqual(result.unchanged, 1)
        self.assertEqual(result.color_changed, 1)
        self.assertEqual(result.added, 1)
        self.assertEqual(result.removed, 1)
        self.assertEqual(result.color_changed_indices, (1,))
        self.assertEqual(result.added_indices, (2,))
        self.assertEqual(result.removed_indices, (2,))

    def test_duplicate_multiset_counts(self) -> None:
        old = PointCloud.from_arrays(
            np.asarray([[0, 0, 0], [0, 0, 0]], dtype=np.float32),
            np.asarray([[1, 1, 1], [1, 1, 1]], dtype=np.uint8),
        )
        new = PointCloud.from_arrays(
            np.asarray([[0, 0, 0]], dtype=np.float32),
            np.asarray([[1, 1, 1]], dtype=np.uint8),
        )
        result = diff_clouds(old, new)
        self.assertEqual(result.unchanged, 1)
        self.assertEqual(result.removed, 1)
        self.assertEqual(result.added, 0)
        self.assertEqual(result.color_changed, 0)

    def test_identical_clouds_have_no_difference(self) -> None:
        cloud = PointCloud.from_arrays(
            np.asarray([[0, 0, 0]], dtype=np.float32),
            np.asarray([[1, 2, 3]], dtype=np.uint8),
        )
        result = diff_clouds(cloud, cloud)
        self.assertEqual(
            (result.added, result.removed, result.color_changed, result.unchanged),
            (0, 0, 0, 1),
        )


if __name__ == "__main__":
    unittest.main()
