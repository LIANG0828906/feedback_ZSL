"""业务要求 6：版本化快照、原子串行写入与断点恢复。"""

import json
import os
import shutil
import tempfile
import unittest

import numpy as np

from cwb.engine import Workbench
from cwb.errors import CwbError, E_SNAPSHOT
from cwb.model import INTENT_RELATIVE
from cwb.snapshot import ALGORITHM_VERSION

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")
IMG = os.path.join(SAMPLES, "images", "tile_32x24_16_rgb.cimg")
SRC = os.path.join(SAMPLES, "profiles", "src_v1.json")
DST = os.path.join(SAMPLES, "profiles", "dst_v1.json")


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _wb(self):
        return Workbench(SRC, DST, INTENT_RELATIVE, output_depth=16,
                         batch_rows=8, snapshot_dir=self.tmp)

    def test_snapshot_roundtrip_restores_batches(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        out1 = wb.run_full(parallel=False)
        self.assertTrue(os.path.exists(os.path.join(self.tmp,
                                                     "manifest.json")))
        wb2 = self._wb()
        wb2.load_image(IMG, 16)
        out2 = wb2.run_full(parallel=False)
        np.testing.assert_array_equal(out1, out2)

    def test_param_version_mismatch_discards_entire_snapshot(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        wb.run_full(parallel=False)
        # 篡改清单参数版本
        manifest = os.path.join(self.tmp, "manifest.json")
        data = json.load(open(manifest))
        data["param_version"] = 999
        json.dump(data, open(manifest, "w"))
        with self.assertRaises(CwbError) as ctx:
            wb.store.load(data["source_hash"], data["config_version"], 1)
        self.assertEqual(ctx.exception.code, E_SNAPSHOT)
        # 整体作废：不允许部分混用旧批
        self.assertFalse(
            any(f.startswith("batch_") for f in os.listdir(self.tmp)))

    def test_batch_hash_mismatch_discards_entire_snapshot(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        wb.run_full(parallel=False)
        path = os.path.join(self.tmp, "batch_0000.npz")
        blob = np.load(path)
        tampered = blob["data"].copy()
        tampered[0, 0, 0] += 1
        np.savez(path, data=tampered,
                 row_start=blob["row_start"], row_end=blob["row_end"],
                 sha256=blob["sha256"])
        data = json.load(open(os.path.join(self.tmp, "manifest.json")))
        with self.assertRaises(CwbError) as ctx:
            wb.store.load(data["source_hash"], data["config_version"],
                          data["param_version"])
        self.assertEqual(ctx.exception.code, E_SNAPSHOT)

    def test_missing_manifest_treated_as_force_termination(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        wb.store.init_run("deadbeef", "1->1", 1, 4, 16, (32, 24, 3))
        # 仅写批、未提交 manifest，模拟进程被强制终止
        wb.store.write_batch(0, 0, 8,
                             np.zeros((8, 24, 3), dtype=np.uint16))
        with self.assertRaises(CwbError) as ctx:
            wb.store.load("deadbeef", "1->1", 1)
        self.assertEqual(ctx.exception.code, E_SNAPSHOT)

    def test_algorithm_version_recorded(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        wb.run_full(parallel=False)
        data = json.load(open(os.path.join(self.tmp, "manifest.json")))
        self.assertEqual(data["algorithm_version"], ALGORITHM_VERSION)

    def test_parallel_snapshot_commit_sequence_is_batch_ordered(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        wb.executor.max_workers = 4
        wb.run_full(parallel=True)
        expected = [b.batch_id for b in wb.pipeline.batches]
        # 锁内 append 与写盘同临界区，提交序列即串行落盘批序
        self.assertEqual(wb.executor.commit_sequence, expected)

    def test_serial_writer_invoked_once_per_batch_in_order(self):
        wb = self._wb()
        wb.load_image(IMG, 16)
        written: list[int] = []
        original = wb.store.write_batch

        def tracking_write(batch_id, row_start, row_end, data):
            written.append(batch_id)
            return original(batch_id, row_start, row_end, data)

        wb.store.write_batch = tracking_write
        wb.run_full(parallel=False)
        expected = [b.batch_id for b in wb.pipeline.batches]
        self.assertEqual(written, expected)


if __name__ == "__main__":
    unittest.main()
