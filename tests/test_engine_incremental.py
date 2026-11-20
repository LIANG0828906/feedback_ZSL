import unittest

import numpy as np

from terrain_tool.binary_io import (
    KIND_HEIGHT,
    KIND_MATERIAL,
    HeightField,
    MaterialMap,
    Patch,
)
from terrain_tool.dependency import NodeKind
from terrain_tool.engine import TerrainEngine
from terrain_tool.errors import ErrorCode, TerrainError
from terrain_tool.pipeline import normal_angle_degrees
from terrain_tool.render import OffscreenRenderer


def _engine(seed: int = 0, chunk_size: int = 8, threshold: float = 1.0) -> TerrainEngine:
    rng = np.random.default_rng(seed)
    elev = (
        10.0
        + 5.0 * np.sin(np.arange(64) * 0.2)[None, :]
        + rng.normal(0, 0.3, (48, 64))
    ).astype(np.float32)
    mat = (rng.integers(0, 8, size=(48, 64))).astype(np.uint8)
    return TerrainEngine(
        HeightField(elev, 1.0, (0, 0, 64, 48), 0),
        MaterialMap(mat, 0),
        chunk_size=chunk_size,
        lod_threshold=threshold,
        crack_tolerance=1e9,
    )


class IncrementalConsistencyTest(unittest.TestCase):
    def test_incremental_matches_full_normals_and_blend(self) -> None:
        patches = [
            Patch(KIND_HEIGHT, (10, 8, 12, 10), 1.0, 0, 1, np.full((10, 12), 30.0, np.float32)),
            Patch(KIND_MATERIAL, (30, 20, 10, 8), 1.0, 1, 2, np.full((8, 10), 5, np.uint8)),
            Patch(KIND_HEIGHT, (2, 2, 8, 8), 1.0, 2, 3, np.full((8, 8), -5.0, np.float32)),
        ]
        inc = _engine()
        inc.full_rebuild(seq=0)
        for patch in patches:
            inc.apply_patch(patch)

        full = _engine()
        for patch in patches:
            full.apply_patch(patch)
        full.full_rebuild()

        np.testing.assert_array_equal(inc.elevations, full.elevations)
        np.testing.assert_array_equal(inc.normals, full.normals)
        np.testing.assert_array_equal(inc.blend, full.blend)
        np.testing.assert_array_equal(inc.chunk_levels, full.chunk_levels)
        angles = normal_angle_degrees(inc.normals, full.normals)
        # 同一纯函数 + 相同输入，法线逐位相等 -> 夹角恒为 0
        self.assertEqual(float(angles.max()), 0.0)

    def test_render_incremental_byte_identical_to_full(self) -> None:
        patches = [
            Patch(KIND_HEIGHT, (12, 10, 16, 12), 1.0, 0, 1, np.full((12, 16), 25.0, np.float32)),
            Patch(KIND_MATERIAL, (4, 4, 12, 12), 1.0, 1, 2, np.full((12, 12), 3, np.uint8)),
        ]
        inc = _engine()
        inc.full_rebuild(seq=0)
        r_inc = OffscreenRenderer(inc)
        r_inc.render_full()
        for patch in patches:
            report = inc.apply_patch(patch)
            r_inc.render_incremental(report)

        full = _engine()
        for patch in patches:
            full.apply_patch(patch)
        full.full_rebuild()
        r_full = OffscreenRenderer(full)
        r_full.render_full()

        np.testing.assert_array_equal(r_inc.color_buffer, r_full.color_buffer)
        np.testing.assert_array_equal(r_inc.heat_buffer, r_full.heat_buffer)
        self.assertEqual(
            r_inc.last_summary.color_hash, r_full.last_summary.color_hash
        )

    def test_render_deterministic_across_runs(self) -> None:
        engine = _engine()
        engine.full_rebuild()
        renderer = OffscreenRenderer(engine)
        first = renderer.render_full()
        second = renderer.render_full()
        self.assertEqual(first.color_hash, second.color_hash)
        self.assertEqual(first.heat_hash, second.heat_hash)

    def test_recompute_reused_partition_and_reasons(self) -> None:
        engine = _engine()
        engine.full_rebuild(seq=0)
        patch = Patch(KIND_HEIGHT, (1, 1, 6, 6), 1.0, 0, 1, np.zeros((6, 6), np.float32))
        report = engine.apply_patch(patch)
        total = len(engine.chunks)
        self.assertEqual(len(report.recomputed) + len(report.reused), total)
        self.assertTrue(report.reused)  # 远离补丁的块应被复用
        # 复用块与重算块不重叠
        self.assertEqual(set(report.recomputed) & set(report.reused), set())
        for key in report.recomputed:
            self.assertTrue(report.reasons[key])
        # 失效集合是渲染闭包的子集且被解释
        render_set = report.invalidation.invalid[NodeKind.RENDER]
        self.assertTrue(render_set.issubset(set(report.recomputed)))

    def test_patch_bounds_and_stale(self) -> None:
        engine = _engine()
        engine.full_rebuild(seq=0)
        oob = Patch(KIND_HEIGHT, (60, 44, 10, 10), 1.0, 0, 1, np.zeros((10, 10), np.float32))
        with self.assertRaises(TerrainError) as ctx:
            engine.apply_patch(oob)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PATCH_BOUNDS)

        stale = Patch(KIND_HEIGHT, (1, 1, 4, 4), 1.0, 5, 1, np.zeros((4, 4), np.float32))
        with self.assertRaises(TerrainError) as ctx:
            engine.apply_patch(stale)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PATCH_STALE)

    def test_idempotent_merge(self) -> None:
        engine = _engine()
        engine.full_rebuild(seq=0)
        data = np.full((6, 6), 9.0, np.float32)
        engine.apply_patch(Patch(KIND_HEIGHT, (5, 5, 6, 6), 1.0, 0, 1, data))
        once = engine.elevations.copy()
        # 同内容、推进版本号再应用一次，目标区域结果必须相同
        engine.apply_patch(Patch(KIND_HEIGHT, (5, 5, 6, 6), 1.0, 1, 2, data))
        np.testing.assert_array_equal(once, engine.elevations)

    def test_non_finite_patch_rejected_without_node_change(self) -> None:
        engine = _engine()
        engine.full_rebuild(seq=0)
        before = engine.elevations.copy()
        nan_patch = Patch(
            KIND_HEIGHT, (1, 1, 4, 4), 1.0, 0, 1, np.full((4, 4), np.nan, np.float32)
        )
        with self.assertRaises(TerrainError) as ctx:
            engine.apply_patch(nan_patch)
        self.assertEqual(ctx.exception.code, ErrorCode.E_PATCH_INVALID)
        np.testing.assert_array_equal(engine.elevations, before)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
