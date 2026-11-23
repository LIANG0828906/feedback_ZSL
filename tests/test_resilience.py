"""强制终止边界、取消语义与快照完整性测试。"""
from __future__ import annotations

import os
import shutil
import tempfile
import unittest

from loudbench.core.config import PipelineConfig
from loudbench.core.errors import CancelledError
from loudbench.core.states import SegmentState
from loudbench.pipeline.app import build_orchestrator

MANIFEST = os.path.join(os.path.dirname(__file__), "..",
                        "samples", "manifest.json")


class ResilienceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="loudbench_res_")

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_cancel_before_completion_no_full_report_claim(self) -> None:
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(), os.path.join(self.tmp, "cp"))
        orch.cancel()
        with self.assertRaises(CancelledError):
            orch.run(parallel=True)
        # 取消后检查点不完整 -> 不允许声称完整报告
        store = orch.checkpoints
        self.assertFalse(store.is_complete(len(orch.segments)))

    def test_partial_checkpoint_not_treated_as_complete(self) -> None:
        # 模拟“强制终止”：只提交前两段即落盘
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(), os.path.join(self.tmp, "cp2"))
        from loudbench.audio.io import content_hash
        hashes = [content_hash(s.samples) for s in orch.segments]
        store = orch.checkpoints
        for index in range(2):
            processed = __import__(
                "loudbench.pipeline.processor", fromlist=["process_segment_dsp"]
            ).process_segment_dsp(
                index, orch.segments[index].samples,
                orch.segments[index].sample_rate, orch.config)
            _out, record = orch.normalizer.submit(
                index, processed.pre_norm_samples, processed.fs)
            from loudbench.pipeline.checkpoint import SegmentCheckpoint
            store.save_segment(SegmentCheckpoint(
                segment_index=index, input_hash=processed.input_hash,
                output_samples=_out, loudness={"x": 1},
                gain_record=record.__dict__,
                gain_curve=processed.total_dsp_gain_curve))
            store.write_manifest(
                hashes, [0, 1],
                accumulated_energy=orch.normalizer.state.accumulated_energy,
                accumulated_weight=orch.normalizer.state.accumulated_weight)
        self.assertFalse(store.is_complete(len(orch.segments)))
        # 恢复后只有两段已提交，进度如实反映，不能生成完整报告
        orch2 = build_orchestrator(
            MANIFEST, PipelineConfig(), os.path.join(self.tmp, "cp2"))
        self.assertTrue(orch2.restore())
        self.assertEqual(orch2.progress(), (2, 6))

    def test_corrupt_segment_checkpoint_invalidates_whole_snapshot(self) -> None:
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(), os.path.join(self.tmp, "cp3"))
        orch.run(parallel=True)
        store = orch.checkpoints
        target = store._segment_path(2)
        with open(target, "wb") as fh:
            fh.write(b"corrupted-bytes")
        from loudbench.audio.io import content_hash
        hashes = [content_hash(s.samples) for s in orch.segments]
        ok, reason = store.validate_snapshot(hashes)
        self.assertFalse(ok)
        # 整体作废，不允许部分混用
        restored = store.restore_or_reset(hashes)
        self.assertEqual(restored, [])

    def test_failed_segment_does_not_touch_committed(self) -> None:
        import numpy as np
        orch = build_orchestrator(
            MANIFEST, PipelineConfig(), os.path.join(self.tmp, "cp4"))
        original = orch.segments[3].samples
        broken = original.copy()
        broken.flat[0] = np.nan
        orch.segments[3].samples = broken
        with self.assertRaises(Exception):
            orch.run(parallel=True)
        committed = [s.index for s in orch.segments
                     if s.state is SegmentState.COMMITTED]
        # 段 3 之前的段落保持已提交，段 3 失败
        self.assertIn(0, committed)
        self.assertIs(orch.segments[3].state, SegmentState.FAILED)


if __name__ == "__main__":
    unittest.main()
