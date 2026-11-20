"""业务要求 4 补充：不可求解帧标记后跳过落盘。"""

import tempfile
import unittest

import numpy as np

from retarget_pipeline import compat, engine, mapping, retarget, samples, snapshot


class UnresolvedSkipTest(unittest.TestCase):
    def test_unresolved_frame_kept_in_memory_but_not_snapshotted(self):
        docs = samples.build_source_documents(42, 6, 6)
        target = samples.build_target_document(42, 6)
        source = compat.upgrade_to_v3(docs[3])
        mp = mapping.build_mapping(source, target)

        original = retarget.nearest_rotation_matrix
        calls = {"n": 0}

        def boom(mat):
            calls["n"] += 1
            if calls["n"] == 5:
                raise np.linalg.LinAlgError("forced failure")
            return original(mat)

        retarget.nearest_rotation_matrix = boom
        try:
            worker = engine.RetargetEngine()
            with tempfile.TemporaryDirectory() as tmp:
                handle = worker.submit(
                    "skip-1", source, target, mp, tmp, 1e-6,
                    animation_key="anim-skip")
                self.assertTrue(worker.wait(handle, timeout=30))
                self.assertEqual(handle.state_holder["status"], "completed")
                result = handle.state_holder["result"]
                snap_files = snapshot.list_snapshots(tmp)
                self.assertEqual(len(snap_files), len(result.resolved_frames))
        finally:
            retarget.nearest_rotation_matrix = original

        self.assertEqual(len(result.skipped_frames), 1)
        self.assertEqual(result.skipped_frames[0].error_code, "E_POSE_UNRESOLVED")


if __name__ == "__main__":
    unittest.main()
