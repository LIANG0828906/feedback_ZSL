"""边界行为：取消、并发资源上限、提交冻结、哈希基线。"""
from __future__ import annotations

import os
import shutil
import tempfile
import unittest

from segworkbench.core.errors import ErrorCode, WorkbenchError
from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.assemble import stitch_final_mask
from segworkbench.pipeline.concurrency import BoundedRunner
from segworkbench.pipeline.engine import SegmentationEngine
from segworkbench.pipeline.loader import load_sample
from segworkbench.pipeline.parameters import SegmentationParameters
from segworkbench.pipeline.samplegen import generate_all
from segworkbench.utils.hashing import sha256_array
from segworkbench.validation.verifier import run_verification

SAMPLE_ID = "sample_03_mixed"


class BoundaryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="segwb-bound-")
        self.data_dir = os.path.join(self.tmp, "data")
        generate_all(self.data_dir)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _engine(self, name: str, params: SegmentationParameters | None = None) -> SegmentationEngine:
        sample = load_sample(self.data_dir, SAMPLE_ID)
        return SegmentationEngine(sample, params or SegmentationParameters(),
                                  CheckpointStore(os.path.join(self.tmp, name)))

    def test_runner_bounds(self) -> None:
        with self.assertRaises(WorkbenchError) as captured:
            BoundedRunner(max_workers=0)
        self.assertEqual(captured.exception.code, ErrorCode.E_PARA)
        with self.assertRaises(WorkbenchError) as captured:
            BoundedRunner(max_workers=4, max_buffered_per_block=0)
        self.assertEqual(captured.exception.code, ErrorCode.E_PARA)

    def test_parallel_matches_serial(self) -> None:
        serial_engine = self._engine("serial")
        serial_engine.run_all()
        serial_hash = sha256_array(stitch_final_mask(CheckpointStore(os.path.join(self.tmp, "serial"))))

        parallel_engine = self._engine("parallel")
        parallel_engine.run_all(runner=BoundedRunner(max_workers=4))
        parallel_hash = sha256_array(
            stitch_final_mask(CheckpointStore(os.path.join(self.tmp, "parallel")))
        )
        self.assertEqual(serial_hash, parallel_hash)
        self.assertTrue(run_verification(os.path.join(self.tmp, "parallel"),
                                         self.data_dir, SAMPLE_ID)["passed"])

    def test_cancel_at_phase_boundary(self) -> None:
        engine = self._engine("cancel")
        engine.initialize()
        engine.process_block("b00")
        engine.request_cancel()
        with self.assertRaises(WorkbenchError) as captured:
            engine.process_block("b01")
        self.assertEqual(captured.exception.code, ErrorCode.E_CANCELLED)
        # 已提交块保持冻结，恢复后可继续。
        rebuilt = self._engine("cancel")
        rebuilt.restore()
        rebuilt.process_block("b01")
        rebuilt.process_block("b02")
        rebuilt.process_block("b03")
        self.assertTrue(run_verification(os.path.join(self.tmp, "cancel"),
                                         self.data_dir, SAMPLE_ID)["passed"])

    def test_parameter_change_does_not_rewrite_committed(self) -> None:
        engine = self._engine("freeze")
        engine.initialize()
        engine.process_block("b00")
        store = CheckpointStore(os.path.join(self.tmp, "freeze"))
        before = store.load_phase("b00", "clean")["array_sha256"]

        new_params = SegmentationParameters(color_tol=80.0, struct_radius=2)
        sample = load_sample(self.data_dir, SAMPLE_ID)
        shifted = SegmentationEngine(sample, new_params, store)
        shifted.restore()
        shifted.process_block("b01")  # 仅未提交块使用新参数
        after = store.load_phase("b00", "clean")["array_sha256"]
        self.assertEqual(before, after)

    def test_image_hash_mismatch_invalidates_snapshot(self) -> None:
        engine = self._engine("img-hash")
        engine.run_all()
        header_path = os.path.join(self.tmp, "img-hash", "snapshot.json")
        import json
        with open(header_path, "r", encoding="utf-8") as handle:
            header = json.load(handle)
        header["image_sha256"] = "deadbeef"
        with open(header_path, "w", encoding="utf-8") as handle:
            json.dump(header, handle)
        rebuilt = self._engine("img-hash")
        with self.assertRaises(WorkbenchError) as captured:
            rebuilt.restore()
        self.assertEqual(captured.exception.code, ErrorCode.E_VERSION)
        self.assertIn("image_sha256", captured.exception.context["mismatched_fields"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
