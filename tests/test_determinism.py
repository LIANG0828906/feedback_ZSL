"""业务要求 8：确定性样本、逐字节一致与 GUI 离屏构建。"""

import os
import tempfile
import unittest

from retarget_pipeline import gui, mapping, retarget, samples, trajectory, validate
from retarget_pipeline import compat


class DeterminismTest(unittest.TestCase):
    def test_sample_generator_byte_identical(self):
        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            sets_a = samples.generate_default_library(a)
            sets_b = samples.generate_default_library(b)
            self.assertEqual([s.name for s in sets_a], [s.name for s in sets_b])
            for rel in ("small/small_source_v3.json", "medium/medium_target_v3.json",
                        "large/manifest.json"):
                with open(os.path.join(a, rel), "rb") as fa, \
                     open(os.path.join(b, rel), "rb") as fb:
                    self.assertEqual(fa.read(), fb.read(), rel)

    def test_trajectory_log_byte_identical(self):
        docs = samples.build_source_documents(202, 8, 6)
        target = samples.build_target_document(202, 8)
        source = compat.upgrade_to_v3(docs[3])
        mp = mapping.build_mapping(source, target)
        result = retarget.retarget_clip(source, target, mp)
        report = validate.validate_retarget(source, target, mp, result, 1e-6)
        records = trajectory.frame_records(
            "det", 3, mp, report, trajectory.document_hash(source))
        with tempfile.TemporaryDirectory() as tmp:
            p1 = os.path.join(tmp, "a.jsonl")
            p2 = os.path.join(tmp, "b.jsonl")
            trajectory.write_trajectory_log(p1, records)
            trajectory.write_trajectory_log(p2, records)
            with open(p1, "rb") as fa, open(p2, "rb") as fb:
                self.assertEqual(fa.read(), fb.read())

    def test_near_coincident_rotation_present(self):
        from retarget_pipeline.math3d import NEAR_COINCIDENT_ANGLE, quat_angle

        docs = samples.build_source_documents(101, 6, 4, near_coincident=True)
        q0 = docs[3].local_rotations[0, 0]
        q1 = docs[3].local_rotations[1, 0]
        self.assertLess(quat_angle(q0, q1), NEAR_COINCIDENT_ANGLE)

    def test_gui_builds_offscreen(self):
        docs = samples.build_source_documents(101, 6, 4)
        target = samples.build_target_document(101, 6)
        source = compat.upgrade_to_v3(docs[3])
        mp = mapping.build_mapping(source, target)
        result = retarget.retarget_clip(source, target, mp)
        report = validate.validate_retarget(source, target, mp, result, 1e-6)
        rc = gui.launch_gui(target, result, mp, report, offscreen=True)
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main()
