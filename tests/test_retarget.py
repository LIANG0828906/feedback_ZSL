"""业务要求 4/5：重定向、缩放单次施加、跨版本一致性。"""

import unittest

import numpy as np

from retarget_pipeline import compat, mapping, retarget, samples, validate

TOL = 1e-6


class RetargetTest(unittest.TestCase):
    def test_all_versions_resolve_all_frames(self):
        docs = samples.build_source_documents(101, 12, 10)
        target = samples.build_target_document(101, 12)
        for version in (1, 2, 3):
            source = compat.upgrade_to_v3(docs[version])
            mp = mapping.build_mapping(source, target)
            result = retarget.retarget_clip(source, target, mp)
            self.assertEqual(len(result.skipped_frames), 0, version)
            self.assertEqual(result.frame_count, 10)
            report = validate.validate_retarget(source, target, mp, result, TOL)
            self.assertTrue(report.passed, (version, report.exceeded[:2]))
            self.assertLess(report.max_angle_rad, TOL)

    def test_cross_version_statistics_within_tolerance(self):
        docs = samples.build_source_documents(202, 12, 16)
        target = samples.build_target_document(202, 12)
        reports = {}
        for version in (1, 3):
            source = compat.upgrade_to_v3(docs[version])
            mp = mapping.build_mapping(source, target)
            result = retarget.retarget_clip(source, target, mp)
            reports[f"v{version}"] = validate.validate_retarget(
                source, target, mp, result, TOL)
        summary = validate.compare_version_runs(reports, TOL)
        self.assertTrue(summary["passed"])

    def test_scale_applied_once_to_root_translation(self):
        docs = samples.build_source_documents(77, 5, 3)
        target = samples.build_target_document(77, 5)
        source = compat.upgrade_to_v3(docs[3])
        mp = mapping.build_mapping(source, target)
        result = retarget.retarget_clip(source, target, mp)
        # 手工按“只施加一次根缩放”复算第 2 帧根位移
        from retarget_pipeline.math3d import quat_rotate

        alignment = retarget.compute_alignment(source, target, mp)
        _, src_pos = retarget.frame_global_poses(source, 1)
        expected = quat_rotate(alignment[0], src_pos[0]) * target.joints[0].scale
        np.testing.assert_allclose(result.frames[1].root_translation, expected, atol=1e-10)

    def test_pose_unresolved_marks_and_skips_frame(self):
        docs = samples.build_source_documents(77, 5, 3)
        target = samples.build_target_document(77, 5)
        source = compat.upgrade_to_v3(docs[3])
        # 注入一个退化旋转矩阵路径：把某帧四元数替换为近零值会在 FK 归一化兜底，
        # 因此直接 monkeypatch nearest_rotation_matrix 制造不可求解场景。
        mp = mapping.build_mapping(source, target)
        original = retarget.nearest_rotation_matrix
        calls = {"n": 0}

        def boom(mat):
            calls["n"] += 1
            if calls["n"] == 3:
                raise np.linalg.LinAlgError("SVD did not converge")
            return original(mat)

        retarget.nearest_rotation_matrix = boom
        try:
            result = retarget.retarget_clip(source, target, mp)
        finally:
            retarget.nearest_rotation_matrix = original
        self.assertTrue(any(not f.resolved for f in result.frames))
        bad = next(f for f in result.frames if not f.resolved)
        self.assertEqual(bad.error_code, "E_POSE_UNRESOLVED")


if __name__ == "__main__":
    unittest.main()
