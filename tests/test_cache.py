from __future__ import annotations

import unittest

from pcwb.cache import OperationCache
from pcwb.operators import run_operator
from tests.helpers import all_selection, sample_cloud, snapshot


class CacheLineageTests(unittest.TestCase):
    def test_invalidate_after_version_removes_later_entries(self) -> None:
        cloud = sample_cloud(5)
        v1 = snapshot(cloud, version=1)
        v2 = snapshot(cloud, version=2)
        selection = all_selection(cloud, version=1)
        result = run_operator(v1, selection, "color_overwrite", {"rgb": [7, 8, 9]})
        cache = OperationCache()
        cache.put(v1, "color_overwrite", {"rgb": [7, 8, 9]}, result.cloud)
        cache.put(v2, "color_overwrite", {"rgb": [7, 8, 9]}, result.cloud)
        self.assertEqual(cache.invalidate_after_version(1), 1)
        self.assertIsNotNone(cache.get(v1, "color_overwrite", {"rgb": [7, 8, 9]}))
        self.assertIsNone(cache.get(v2, "color_overwrite", {"rgb": [7, 8, 9]}))


if __name__ == "__main__":
    unittest.main()
