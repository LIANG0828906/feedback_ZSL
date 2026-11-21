import json
import os
import tempfile
import unittest

import numpy as np

from hdrworkbench.errors import E_RECOVERY_INVALID, RecoveryInvalidError
from hdrworkbench.hashing import array_hash
from hdrworkbench.snapshot import (
    GENESIS,
    SnapshotWriter,
    recover_snapshot,
)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.manifest = {"version_id": 1, "factors": {"frac_bits": 12}}

    def _codes(self, index):
        rng = np.random.default_rng(index)
        return rng.integers(0, 1023, size=(4, 4, 3), dtype=np.uint16)

    def test_crash_then_recover_only_missing_frames(self):
        crash_dir = os.path.join(self.dir, "crash")
        writer = SnapshotWriter(crash_dir, self.manifest, crash_after=1)
        with self.assertRaises(Exception):
            for index in range(4):
                codes = self._codes(index)
                writer.write_frame(
                    index, codes, {"max_bound_codes": 1.0},
                    f"src{index}", {}
                )
        self.assertFalse(
            os.path.exists(os.path.join(crash_dir, "manifest.json"))
        )
        recovered = recover_snapshot(crash_dir)
        self.assertEqual(recovered["committed_frames"], [0, 1])

        # 恢复后继续：已提交帧不重复写入
        writer2 = SnapshotWriter(crash_dir, self.manifest)
        before = set(os.listdir(os.path.join(crash_dir, "chunks")))
        reused_chunk_path = os.path.join(
            crash_dir, "chunks", "chunk_000000"
        )
        mtime_before = os.stat(reused_chunk_path).st_mtime_ns
        reused = writer2.write_frame(0, self._codes(99), {}, "ignored", {})
        self.assertEqual(reused["frame_index"], 0)
        self.assertEqual(
            os.stat(reused_chunk_path).st_mtime_ns, mtime_before
        )
        for index in range(2, 4):
            writer2.write_frame(
                index, self._codes(index), {}, f"src{index}", {}
            )
        writer2.publish_manifest()
        after = set(os.listdir(os.path.join(crash_dir, "chunks")))
        self.assertEqual(after.difference(before), {"chunk_000002", "chunk_000003"})
        full = recover_snapshot(crash_dir)
        self.assertEqual(full["committed_frames"], [0, 1, 2, 3])
        self.assertIsNotNone(full["manifest"])

    def test_broken_chain_locates_first_bad_frame(self):
        good_dir = os.path.join(self.dir, "good")
        writer = SnapshotWriter(good_dir, self.manifest)
        for index in range(3):
            writer.write_frame(index, self._codes(index), {}, "s", {})
        chunk_path = os.path.join(
            good_dir, "chunks", "chunk_000002"
        )
        with open(chunk_path, "r", encoding="utf-8") as handle:
            chunk = json.load(handle)
        chunk["prev_hash"] = "f" * 64
        with open(chunk_path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(chunk, sort_keys=True))
        with self.assertRaises(RecoveryInvalidError) as ctx:
            recover_snapshot(good_dir)
        self.assertEqual(ctx.exception.code, E_RECOVERY_INVALID)
        self.assertEqual(ctx.exception.details["bad_frame"], 2)

    def test_missing_field_reported(self):
        good_dir = os.path.join(self.dir, "missing")
        writer = SnapshotWriter(good_dir, self.manifest)
        writer.write_frame(0, self._codes(0), {}, "s", {})
        chunk_path = os.path.join(good_dir, "chunks", "chunk_000000")
        with open(chunk_path, "r", encoding="utf-8") as handle:
            chunk = json.load(handle)
        del chunk["content_hash"]
        with open(chunk_path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(chunk, sort_keys=True))
        with self.assertRaises(RecoveryInvalidError) as ctx:
            recover_snapshot(good_dir)
        self.assertEqual(ctx.exception.details["bad_frame"], 0)

    def test_factor_mismatch_rejects_reuse(self):
        good_dir = os.path.join(self.dir, "factor")
        writer = SnapshotWriter(good_dir, self.manifest)
        writer.write_frame(0, self._codes(0), {}, "s", {})
        different = {"version_id": 1, "factors": {"frac_bits": 20}}
        with self.assertRaises(RecoveryInvalidError) as ctx:
            SnapshotWriter(good_dir, different)
        self.assertEqual(
            ctx.exception.details["reason"], "factor_mismatch"
        )

    def test_genesis_constant(self):
        self.assertEqual(len(GENESIS), 64)
        self.assertEqual(array_hash(np.zeros((2, 2), dtype=np.uint8))[:1] != "", True)


if __name__ == "__main__":
    unittest.main()

