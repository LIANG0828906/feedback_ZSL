"""端到端集成：三路径一致性、检查点、异常码、资源预算。"""
from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

import numpy as np

from loudbench.core.config import PipelineConfig, SchedulerConfig
from loudbench.core.errors import DecodeError, DiffError, ParameterError
from loudbench.core.states import SegmentState
from loudbench.pipeline.app import (
    build_orchestrator, load_program, run_path, verify_three_paths)
from loudbench.pipeline.compare import assert_outputs_equal

SAMPLES_DIR = os.path.join(os.path.dirname(__file__), "..", "samples")
MANIFEST = os.path.join(SAMPLES_DIR, "manifest.json")


class IntegrationTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if not os.path.exists(MANIFEST):
            from samples.generate_samples import generate
            generate(os.path.join(SAMPLES_DIR, "data"))

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="loudbench_test_")
        self.cp = os.path.join(self.tmp, "cp")
        self.exports = os.path.join(self.tmp, "exports")
        self.config = PipelineConfig()

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_three_paths_bit_exact_and_report_matches_truth(self) -> None:
        result = verify_three_paths(
            MANIFEST, self.config, self.cp, self.exports)
        self.assertTrue(os.path.exists(result["export_wav"]))
        report = result["report"]
        self.assertTrue(report["within_tolerance"])
        self.assertAlmostEqual(report["overall_integrated_lufs"],
                               report["target_lufs"], delta=0.5)
        segments, manifest = load_program(MANIFEST)
        # 逐段真值：处理前测量值与清单真值在上界内一致
        for seg, entry in zip(segments, manifest["segments"]):
            truth = entry["truth"]
            self.assertAlmostEqual(
                seg.truth["integrated_lufs"],
                truth["integrated_lufs"], delta=1e-6)

    def test_parallel_equals_serial_sample_by_sample(self) -> None:
        orch_p = run_path(MANIFEST, self.config,
                          os.path.join(self.cp, "p"), parallel=True)
        orch_s = run_path(MANIFEST, self.config,
                          os.path.join(self.cp, "s"), parallel=False)
        assert_outputs_equal(orch_p._ordered_outputs(),
                             orch_s._ordered_outputs())

    def test_restore_path_bit_exact(self) -> None:
        orch = run_path(MANIFEST, self.config,
                        os.path.join(self.cp, "r"), parallel=True)
        orch2 = build_orchestrator(
            MANIFEST, self.config, os.path.join(self.cp, "r"))
        self.assertTrue(orch2.restore())
        assert_outputs_equal(orch._ordered_outputs(),
                             orch2._ordered_outputs())
        self.assertEqual(orch2.progress(), (6, 6))

    def test_checkpoint_invalidated_on_param_change(self) -> None:
        run_path(MANIFEST, self.config, os.path.join(self.cp, "v"),
                 parallel=True)
        changed = PipelineConfig(
            scheduler=SchedulerConfig(max_workers=1))
        orch = build_orchestrator(MANIFEST, changed,
                                  os.path.join(self.cp, "v"))
        # 参数指纹变化 -> 快照整体作废，无恢复数据
        self.assertFalse(orch.restore())
        self.assertEqual(orch.progress(), (0, 6))

    def test_checkpoint_invalidated_on_content_change(self) -> None:
        cp_dir = os.path.join(self.cp, "c")
        run_path(MANIFEST, self.config, cp_dir, parallel=True)
        # 篡改输入内容哈希清单
        manifest_path = os.path.join(self.tmp, "manifest_copy.json")
        with open(MANIFEST, "rb") as fh:
            manifest = json.loads(fh.read().decode())
        manifest["segments"][0]["content_hash"] = "deadbeef"
        with open(manifest_path, "w", encoding="utf-8") as fh:
            json.dump(manifest, fh)
        with self.assertRaises(DecodeError) as ctx:
            load_program(manifest_path)
        self.assertEqual(ctx.exception.code, "E_DECODE")

    def test_diff_error_locates_segment_and_sample(self) -> None:
        orch = run_path(MANIFEST, self.config,
                        os.path.join(self.cp, "d"), parallel=True)
        outputs = orch._ordered_outputs()
        # 人为制造段 2 的第 100 个样本差异
        import copy
        bad = copy.deepcopy(outputs)
        bad[2].output_samples.flat[100] += 0.5
        with self.assertRaises(DiffError) as ctx:
            assert_outputs_equal(outputs, bad)
        self.assertEqual(ctx.exception.code, "E_DIFF")
        self.assertEqual(ctx.exception.segment_index, 2)
        self.assertEqual(ctx.exception.sample_index, 100)

    def test_corrupt_wav_returns_edecode(self) -> None:
        bad_wav = os.path.join(self.tmp, "bad.wav")
        with open(bad_wav, "wb") as fh:
            fh.write(b"RIFFxxxxWAVEgarbage-not-a-real-wav")
        from loudbench.audio.io import read_wav
        with self.assertRaises(DecodeError) as ctx:
            read_wav(bad_wav)
        self.assertEqual(ctx.exception.code, "E_DECODE")

    def test_segment_failure_isolated_and_retryable(self) -> None:
        orch = build_orchestrator(
            MANIFEST, self.config, os.path.join(self.cp, "f"))
        original = orch.segments[1].samples
        # 注入 NaN 触发单段失败
        broken = original.copy()
        broken.flat[0] = np.nan
        orch.segments[1].samples = broken
        with self.assertRaises(ParameterError):
            orch.run(parallel=True)
        self.assertIs(orch.segments[1].state, SegmentState.FAILED)
        # 其他已提交段不受污染
        self.assertIs(orch.segments[0].state, SegmentState.COMMITTED)
        # 修复后重试成功
        orch.segments[1].samples = original
        orch.retry_failed(parallel=True)
        self.assertEqual(orch.progress(), (6, 6))

    def test_buffer_budget_enforced_epara(self) -> None:
        tight = PipelineConfig(scheduler=SchedulerConfig(
            max_workers=2, per_worker_buffer_samples=8192))
        orch = build_orchestrator(MANIFEST, tight,
                                  os.path.join(self.cp, "b"))
        with self.assertRaises(ParameterError) as ctx:
            orch.run(parallel=True)
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertEqual(ctx.exception.field,
                         "per_worker_buffer_samples")

    def test_progress_counts_committed_only(self) -> None:
        orch = build_orchestrator(MANIFEST, self.config,
                                  os.path.join(self.cp, "prog"))
        self.assertEqual(orch.progress(), (0, 6))
        orch.run(parallel=True)
        self.assertEqual(orch.progress(), (6, 6))


if __name__ == "__main__":
    unittest.main()
