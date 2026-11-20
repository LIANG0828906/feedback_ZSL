import json
import os
import tempfile
import unittest

import numpy as np

from terrain_tool.errors import ErrorCode, TerrainError
from terrain_tool.hashing import GENESIS_HASH
from terrain_tool.snapshot import (
    cleanup_tmp,
    commit_batch,
    latest_seq,
    recover,
)


class SnapshotTest(unittest.TestCase):
    def setUp(self) -> None:
        self.snap = tempfile.mkdtemp()
        self.elev = np.arange(64, dtype=np.float32).reshape(8, 8)
        self.mat = np.zeros((8, 8), np.uint8)

    def _commit(self, seq: int, prev: str, value: float) -> str:
        elev = np.full((8, 8), value, np.float32)
        record = commit_batch(
            self.snap,
            elev,
            self.mat,
            1.0,
            (0, 0, 8, 8),
            seq,
            prev,
            {"render": [[0, 0]]},
            {"seq": seq},
        )
        return record.chain

    def test_commit_and_recover_chain(self) -> None:
        chain = GENESIS_HASH
        for seq in range(1, 4):
            chain = self._commit(seq, chain, float(seq))
        info = recover(self.snap)
        self.assertEqual(info["last_seq"], 3)
        self.assertEqual(latest_seq(self.snap), 3)
        self.assertEqual(info["chain"], chain)

    def test_crash_leaves_tmp_and_recovers_last_commit(self) -> None:
        chain = self._commit(1, GENESIS_HASH, 1.0)
        with open(os.path.join(self.snap, "seq_000002.hgt.tmp"), "wb") as fh:
            fh.write(b"partial")
        with open(os.path.join(self.snap, "seq_000002.mat.tmp"), "wb") as fh:
            fh.write(b"partial")
        info = recover(self.snap)
        self.assertEqual(info["last_seq"], 1)
        self.assertEqual(info["chain"], chain)
        self.assertFalse(
            any(name.endswith(".tmp") for name in os.listdir(self.snap))
        )

    def test_commit_failure_keeps_previous_version(self) -> None:
        self._commit(1, GENESIS_HASH, 1.0)
        bad_dir = os.path.join(self.snap, "manifest.json", "nested")
        with self.assertRaises(TerrainError) as ctx:
            self._commit(2, "x", 2.0) if False else commit_batch(
                bad_dir,
                self.elev,
                self.mat,
                1.0,
                (0, 0, 8, 8),
                2,
                "x",
                {},
                {},
            )
        self.assertEqual(ctx.exception.code, ErrorCode.E_COMMIT_FAILED)
        self.assertEqual(latest_seq(self.snap), 1)

    def test_broken_chain_detected(self) -> None:
        chain = self._commit(1, GENESIS_HASH, 1.0)
        self._commit(2, chain, 2.0)
        manifest = os.path.join(self.snap, "seq_000002.manifest.json")
        with open(manifest, encoding="utf-8") as fh:
            data = json.load(fh)
        data["prev_chain"] = "deadbeef"
        with open(manifest, "w", encoding="utf-8") as fh:
            json.dump(data, fh)
        with self.assertRaises(TerrainError) as ctx:
            recover(self.snap)
        self.assertEqual(ctx.exception.code, ErrorCode.E_RECOVERY_INVALID)

    def test_missing_field_detected(self) -> None:
        self._commit(1, GENESIS_HASH, 1.0)
        manifest = os.path.join(self.snap, "seq_000001.manifest.json")
        with open(manifest, encoding="utf-8") as fh:
            data = json.load(fh)
        del data["chain"]
        with open(manifest, "w", encoding="utf-8") as fh:
            json.dump(data, fh)
        with self.assertRaises(TerrainError) as ctx:
            recover(self.snap)
        self.assertEqual(ctx.exception.code, ErrorCode.E_RECOVERY_INVALID)

    def test_cleanup_tmp(self) -> None:
        path = os.path.join(self.snap, "x.tmp")
        with open(path, "wb") as fh:
            fh.write(b"z")
        removed = cleanup_tmp(self.snap)
        self.assertEqual(removed, [path])
        self.assertFalse(os.path.exists(path))


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
