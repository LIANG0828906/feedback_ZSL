import unittest

import numpy as np

from terrain_tool.binary_io import HeightField, MaterialMap
from terrain_tool.engine import TerrainEngine
from terrain_tool.pipeline import close_seams
from terrain_tool.errors import ErrorCode, TerrainError


class SeamTest(unittest.TestCase):
    def _ridge_engine(self, tolerance: float) -> TerrainEngine:
        width, height = 96, 80
        xs = np.arange(width)
        ridge = 30.0 * np.exp(-((xs - width * 0.5) ** 2) / (2 * 6.0 ** 2))
        elev = np.broadcast_to(ridge, (height, width)).copy().astype(np.float32)
        mat = np.zeros((height, width), np.uint8)
        return TerrainEngine(
            HeightField(elev, 1.0, (0, 0, width, height), 0),
            MaterialMap(mat, 0),
            chunk_size=16,
            lod_threshold=0.5,
            max_lod_level=6,
            crack_tolerance=tolerance,
        )

    def test_loose_tolerance_closes_seams(self) -> None:
        engine = self._ridge_engine(tolerance=1e9)
        report = engine.full_rebuild(seq=0)
        self.assertIsNone(report.crack)
        self.assertTrue(report.seam_report)
        for entry in report.seam_report:
            self.assertTrue(entry["closed"])
            self.assertEqual(len(entry["world_xy"]), 2)

    def test_strict_tolerance_raises_crack_with_location(self) -> None:
        engine = self._ridge_engine(tolerance=1e-6)
        report = engine.full_rebuild(seq=0)
        # 引擎不中断流程，而是把超差定位写入 report.crack
        self.assertIsNotNone(report.crack)
        self.assertIn("world_xy", report.crack)
        self.assertGreater(report.crack["max_gap"], 1e-6)

    def test_pipeline_directly_raises_on_gap(self) -> None:
        engine = self._ridge_engine(tolerance=1e-9)
        engine.full_rebuild(seq=0)
        with self.assertRaises(TerrainError) as ctx:
            close_seams(
                engine.root,
                engine.elevations,
                engine.chunk_size,
                engine.cell_size,
                1e-9,
            )
        self.assertEqual(ctx.exception.code, ErrorCode.E_CRACK_UNRESOLVED)
        self.assertIn("world_xy", ctx.exception.detail)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
