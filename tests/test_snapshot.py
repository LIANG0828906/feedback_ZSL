"""业务要求 6：快照、哈希链、原子写与恢复。"""

import json
import os
import tempfile
import unittest

from retarget_pipeline import mapping, retarget, samples, snapshot, compat
from retarget_pipeline.errors import E_RECOVERY_INVALID, PipelineError


class SnapshotTest(unittest.TestCase):
    def setUp(self):
        self.docs = samples.build_source_documents(5, 6, 5)
        self.target = samples.build_target_document(5, 6)
        self.source = compat.upgrade_to_v3(self.docs[3])
        self.mapping = mapping.build_mapping(self.source, self.target)
        self.result = retarget.retarget_clip(self.source, self.target, self.mapping)

    def test_commit_chain_and_recover(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = snapshot.init_project(tmp, 3, 3, self.mapping.content_hash())
            for frame in self.result.frames:
                state, _ = snapshot.commit_frame(
                    tmp, state, frame, self.mapping.content_hash(), 3, 3)
            recovery = snapshot.recover(
                tmp, current_mapping_hash=self.mapping.content_hash(),
                current_source_version=3, current_target_version=3)
            self.assertFalse(recovery.needs_recompute)
            self.assertEqual(len(recovery.frames), self.result.frame_count)
            rebuilt = snapshot.result_from_recovered_frames(
                self.target.names, recovery.frames, self.mapping.content_hash(), 3, 3)
            self.assertEqual(rebuilt.frame_count, self.result.frame_count)

    def test_broken_chain_aborts(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = snapshot.init_project(tmp, 3, 3, self.mapping.content_hash())
            for frame in self.result.frames[:3]:
                state, _ = snapshot.commit_frame(
                    tmp, state, frame, self.mapping.content_hash(), 3, 3)
            # 篡改中间快照的 prev_hash
            files = snapshot.list_snapshots(tmp)
            mid = os.path.join(tmp, files[1])
            with open(mid, encoding="utf-8") as handle:
                data = json.load(handle)
            data["prev_hash"] = "deadbeef"
            with open(mid, "w", encoding="utf-8") as handle:
                json.dump(data, handle)
            with self.assertRaises(PipelineError) as ctx:
                snapshot.recover(tmp)
            self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)

    def test_mapping_change_triggers_recompute(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = snapshot.init_project(tmp, 3, 3, self.mapping.content_hash())
            state, _ = snapshot.commit_frame(
                tmp, state, self.result.frames[0],
                self.mapping.content_hash(), 3, 3)
            recovery = snapshot.recover(
                tmp, current_mapping_hash="changed-hash",
                current_source_version=3, current_target_version=3)
            self.assertTrue(recovery.needs_recompute)
            self.assertEqual(recovery.recompute_reason, "mapping_hash_changed")

    def test_version_change_triggers_recompute(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = snapshot.init_project(tmp, 3, 3, self.mapping.content_hash())
            state, _ = snapshot.commit_frame(
                tmp, state, self.result.frames[0],
                self.mapping.content_hash(), 3, 3)
            recovery = snapshot.recover(
                tmp, current_mapping_hash=self.mapping.content_hash(),
                current_source_version=1, current_target_version=3)
            self.assertTrue(recovery.needs_recompute)
            self.assertEqual(recovery.recompute_reason, "source_version_changed")


if __name__ == "__main__":
    unittest.main()
