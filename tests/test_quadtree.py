import unittest

import numpy as np

from terrain_tool.quadtree import (
    build_chunks,
    build_quadtree,
    chunk_grid_dims,
    target_level_for_roughness,
)


class QuadtreeTest(unittest.TestCase):
    def test_chunk_coverage_covers_full_grid(self) -> None:
        chunks = build_chunks(height=33, width=31, chunk_size=16)
        # 2 列 x 3 行，边缘块被截断
        self.assertEqual(len(chunks), 6)
        last = max(chunks, key=lambda c: (c.cy, c.cx))
        self.assertEqual((last.x1, last.y1), (31, 33))
        cols, rows = chunk_grid_dims(33, 31, 16)
        self.assertEqual((cols, rows), (2, 3))

    def test_target_level_bounded(self) -> None:
        self.assertEqual(target_level_for_roughness(0.0, 1.0, 4), 0)
        self.assertEqual(target_level_for_roughness(100.0, 1.0, 4), 4)
        self.assertEqual(target_level_for_roughness(2.5, 1.0, 4), 2)

    def test_quadtree_subdivides_only_rough_region(self) -> None:
        # 8x8 网格，仅左上角 chunk 需要最高 LOD；max_level 足够细分到单 chunk
        levels = np.zeros((8, 8), dtype=np.int32)
        levels[0, 0] = 4
        root = build_quadtree(levels, grid_cols=8, grid_rows=8, max_level=4)
        leaves = root.leaves()
        # 沿左上路径细分到单 chunk 叶子（target_level=4，最深 depth=3）
        deep = [n for n in leaves if (n.cx1 - n.cx0) == 1]
        self.assertTrue(deep)
        for node in deep:
            self.assertEqual((node.cx1 - node.cx0, node.cy1 - node.cy0), (1, 1))
        # 远离粗糙区的 chunk 停在浅层，粗糙 chunk 定位到最深叶子
        self.assertLessEqual(root.leaf_at(7, 7).depth, 2)
        self.assertEqual(root.leaf_at(0, 0).depth, 3)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
