import unittest

import numpy as np

from pcwb.cloud import PointCloud
from pcwb.diff import diff_clouds
from pcwb.format import content_hash, encode_cloud


def cloud(xyz, rgb, version: int) -> PointCloud:
    xyz = np.asarray(xyz, dtype=np.float64)
    rgb = np.asarray(rgb, dtype=np.uint8)
    sha = content_hash(encode_cloud(xyz, rgb))
    return PointCloud.from_arrays(xyz, rgb, sha, version)


class DiffTests(unittest.TestCase):
    def test_added_deleted_and_color_changes(self):
        old = cloud(
            [[0, 0, 0], [1, 0, 0], [2, 0, 0]],
            [[10, 10, 10], [20, 20, 20], [30, 30, 30]], 1)
        new = cloud(
            [[1, 0, 0], [2, 0, 0], [3, 0, 0]],
            [[20, 20, 20], [99, 99, 99], [40, 40, 40]], 2)
        result = diff_clouds(old, new)
        self.assertEqual(result.counts,
                         {"added": 1, "deleted": 1, "color_changed": 1})
        self.assertEqual(new.xyz[result.added].tolist(), [[3.0, 0.0, 0.0]])
        self.assertEqual(old.xyz[result.deleted].tolist(), [[0.0, 0.0, 0.0]])
        self.assertEqual(old.rgb[result.color_changed_old].tolist(),
                         [[30, 30, 30]])
        self.assertEqual(new.rgb[result.color_changed_new].tolist(),
                         [[99, 99, 99]])

    def test_order_independent_identity(self):
        old = cloud([[0, 0, 0], [1, 1, 1]], [[1, 1, 1], [2, 2, 2]], 1)
        new = cloud([[1, 1, 1], [0, 0, 0]], [[2, 2, 2], [1, 1, 1]], 2)
        result = diff_clouds(old, new)
        self.assertEqual(result.counts,
                         {"added": 0, "deleted": 0, "color_changed": 0})

    def test_identical_clouds_have_no_diff(self):
        c1 = cloud([[0, 0, 0], [1, 2, 3]], [[5, 5, 5], [6, 6, 6]], 1)
        c2 = cloud([[0, 0, 0], [1, 2, 3]], [[5, 5, 5], [6, 6, 6]], 2)
        result = diff_clouds(c1, c2)
        self.assertEqual(sum(result.counts.values()), 0)


if __name__ == "__main__":
    unittest.main()
