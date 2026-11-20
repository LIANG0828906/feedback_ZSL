import unittest

from terrain_tool.dependency import (
    NodeKind,
    chunks_overlapping_rect,
    compute_invalidation,
)


class InvalidationTest(unittest.TestCase):
    def test_rect_to_chunks(self) -> None:
        chunks = chunks_overlapping_rect((0, 0, 17, 17), chunk_size=16, height=40, width=40)
        self.assertEqual(chunks, {(0, 0), (1, 0), (0, 1), (1, 1)})

    def test_height_patch_invalidation_closure(self) -> None:
        result = compute_invalidation(
            height_rect=(0, 0, 4, 4),
            material_rect=None,
            chunk_size=16,
            height=64,
            width=64,
        )
        # 直接块 (0,0)；法线光环扩展到相邻块；渲染闭包覆盖全部受影响块
        self.assertIn((0, 0), result.invalid[NodeKind.HEIGHT])
        self.assertIn((0, 0), result.invalid[NodeKind.NORMALS])
        self.assertIn((1, 0), result.invalid[NodeKind.NORMALS])  # 光环
        self.assertIn((0, 0), result.invalid[NodeKind.LOD])
        # 材质链路不应被高程补丁触发
        self.assertEqual(result.invalid[NodeKind.MATERIAL], set())
        self.assertEqual(result.invalid[NodeKind.BLEND], set())
        # 每个失效节点都有原因解释
        for kind, chunks in result.invalid.items():
            for cx, cy in chunks:
                self.assertIn((kind, cx, cy), result.reasons)

    def test_material_patch_does_not_touch_height_chain(self) -> None:
        result = compute_invalidation(
            height_rect=None,
            material_rect=(20, 20, 5, 5),
            chunk_size=16,
            height=64,
            width=64,
        )
        self.assertEqual(result.invalid[NodeKind.HEIGHT], set())
        self.assertEqual(result.invalid[NodeKind.NORMALS], set())
        self.assertIn((1, 1), result.invalid[NodeKind.BLEND])
        self.assertIn((1, 1), result.invalid[NodeKind.RENDER])

    def test_conservative_full_coverage(self) -> None:
        # 变更范围无法收窄时（整图），所有块都进渲染闭包
        result = compute_invalidation(
            height_rect=(0, 0, 64, 64),
            material_rect=None,
            chunk_size=16,
            height=64,
            width=64,
        )
        self.assertEqual(len(result.render_chunks()), 16)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
