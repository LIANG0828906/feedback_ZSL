"""业务要求 2：关节映射、冲突与最近祖先回退。"""

import unittest

from retarget_pipeline import compat, mapping, samples
from retarget_pipeline.errors import E_MAP_CONFLICT
from retarget_pipeline.model import AnimDocument, JointDef
import numpy as np


class MappingTest(unittest.TestCase):
    def test_alias_and_normalized_priority(self):
        docs = samples.build_source_documents(101, 12, 4)
        target = samples.build_target_document(101, 12)
        mp = mapping.build_mapping(compat.upgrade_to_v3(docs[3]), target)
        # 全部命中，且别名匹配优先出现
        self.assertEqual(mp.hit_count, 12)
        self.assertTrue(any(e.match_type == mapping.MATCH_ALIAS for e in mp.entries))
        self.assertEqual(mp.conflicts, [])

    def test_mapping_is_deterministic_and_serializable(self):
        docs = samples.build_source_documents(5, 8, 3)
        target = samples.build_target_document(5, 8)
        a = mapping.build_mapping(compat.upgrade_to_v3(docs[3]), target)
        b = mapping.build_mapping(compat.upgrade_to_v3(docs[3]), target)
        self.assertEqual(a.canonical_json(), b.canonical_json())
        self.assertEqual(a.content_hash(), b.content_hash())

    def test_conflict_reported(self):
        # 两个源关节别名都指向同一目标关节
        source = AnimDocument(
            version=3,
            joints=[
                JointDef("A", None, aliases=["shared"]),
                JointDef("B", 0, aliases=["shared"]),
            ],
            local_rotations=np.zeros((1, 2, 4)),
            local_translations=np.zeros((1, 2, 3)),
        )
        source.local_rotations[..., 0] = 1
        target = AnimDocument(
            version=3,
            joints=[JointDef("T", None, aliases=["shared"])],
            local_rotations=np.ones((1, 1, 4)),
            local_translations=np.zeros((1, 1, 3)),
        )
        target.local_rotations[..., 0] = 1
        mp = mapping.build_mapping(source, target)
        self.assertEqual(len(mp.conflicts), 1)
        self.assertEqual(mp.conflicts[0]["code"], E_MAP_CONFLICT)
        self.assertEqual(mp.conflicts[0]["target_name"], "T")

    def test_ancestor_fallback_for_unmatched_target(self):
        # 目标多出一个叶子关节，无法直接命中，应回退到最近祖先
        docs = samples.build_source_documents(9, 5, 2)
        target = samples.build_target_document(9, 5)
        target.joints.append(JointDef("ExtraLeaf", len(target.joints) - 1,
                                      bind_matrix=np.eye(4), aliases=[]))
        target.local_rotations = np.concatenate(
            [target.local_rotations, np.ones((1, 1, 4))], axis=1)
        target.local_rotations[:, -1, 1:] = 0
        target.local_translations = np.concatenate(
            [target.local_translations, np.zeros((1, 1, 3))], axis=1)
        mp = mapping.build_mapping(compat.upgrade_to_v3(docs[3]), target)
        fallback = [f for f in mp.fallback_targets if f["target_name"] == "ExtraLeaf"]
        self.assertEqual(len(fallback), 1)
        self.assertTrue(fallback[0]["resolved"])
        anchor = mapping.target_driver_index(mp)
        self.assertIn(len(target.joints) - 1, anchor)


if __name__ == "__main__":
    unittest.main()
