"""业务要求 3：跨版本升级与降级损失报告。"""

import unittest

import numpy as np

from retarget_pipeline import compat, samples
from retarget_pipeline.errors import E_VERSION_LOSS, PipelineError


class CompatTest(unittest.TestCase):
    def test_upgrade_fills_derived_fields_with_provenance(self):
        docs = samples.build_source_documents(3, 6, 4)
        upgraded = compat.upgrade_to_v3(docs[1])
        self.assertEqual(upgraded.version, 3)
        for joint in upgraded.joints:
            rec = upgraded.provenance[joint.name]
            self.assertEqual(rec["bind_matrix"], compat.DERIVED_BIND)
            self.assertEqual(rec["aliases"], compat.DERIVED_ALIAS)
            self.assertIsNotNone(joint.bind_matrix)
            self.assertTrue(joint.aliases)

    def test_upgrade_native_v3_keeps_provenance_native(self):
        docs = samples.build_source_documents(3, 6, 4)
        upgraded = compat.upgrade_to_v3(docs[3])
        rec = upgraded.provenance[upgraded.joints[0].name]
        self.assertEqual(rec["bind_matrix"], compat.NATIVE_BIND)
        self.assertEqual(rec["aliases"], compat.NATIVE_ALIAS)

    def test_downgrade_raises_version_loss_by_default(self):
        docs = samples.build_source_documents(3, 6, 4)
        upgraded = compat.upgrade_to_v3(docs[3])
        with self.assertRaises(PipelineError) as ctx:
            compat.downgrade(upgraded, 1)
        self.assertEqual(ctx.exception.code, E_VERSION_LOSS)
        fields = {loc["field"] for loc in ctx.exception.locations}
        self.assertIn("scale", fields)
        self.assertIn("bind_matrix", fields)

    def test_downgrade_accept_loss_returns_report(self):
        docs = samples.build_source_documents(3, 6, 4)
        upgraded = compat.upgrade_to_v3(docs[3])
        out, report = compat.downgrade(upgraded, 2, accept_loss=True)
        self.assertEqual(out.version, 2)
        self.assertIn("bind_matrix", report.lost_fields)
        # v2 仍保留平移与父子层级
        self.assertNotIn("translations", report.lost_fields)
        self.assertNotIn("parent", report.lost_fields)

    def test_v1_to_v3_then_equivalent_motion(self):
        docs = samples.build_source_documents(11, 6, 5)
        v3 = compat.upgrade_to_v3(docs[1])
        v3_native = compat.upgrade_to_v3(docs[3])
        # 旋转通道在升级前后完全保持
        np.testing.assert_allclose(v3.local_rotations, docs[1].local_rotations)
        np.testing.assert_allclose(
            v3_native.local_rotations, docs[3].local_rotations)


if __name__ == "__main__":
    unittest.main()
