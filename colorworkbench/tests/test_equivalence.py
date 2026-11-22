"""业务要求 5：增量与全量重算逐像素一致、统计/直方图一致、E_DIFF 定位。"""

import os
import unittest

import numpy as np

from cwb.engine import Workbench
from cwb.errors import CwbError, E_DIFF
from cwb.model import INTENT_ABSOLUTE, INTENT_PERCEPTUAL, INTENT_RELATIVE

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")
IMG = os.path.join(SAMPLES, "images", "tile_40x40_16_rgba.cimg")
SRC = os.path.join(SAMPLES, "profiles", "src_v3.json")
DST = os.path.join(SAMPLES, "profiles", "dst_v3.json")

# 对同一编辑序列，增量路径声明的缓存复用次数下限（每批至少保留 2 个早期阶段）
DECLARED_REUSE_FLOOR = 8


def _edit_sequence(editor):
    editor.edit_matrix([0.40, 0.36, 0.18, 0.21, 0.70, 0.07, 0.02, 0.12, 0.93])


def _fresh_workbench():
    return Workbench(SRC, DST, INTENT_RELATIVE, output_depth=16,
                     batch_rows=8, max_workers=2)


class EquivalenceTests(unittest.TestCase):
    def test_incremental_equals_full_pixel_for_pixel(self):
        # 基线：从空缓存对编辑后的模型全量重算
        full = _fresh_workbench()
        full.load_image(IMG, 16)
        full.run_full(parallel=False)
        full.editor.edit_matrix(
            [0.40, 0.36, 0.18, 0.21, 0.70, 0.07, 0.02, 0.12, 0.93])
        full.editor.commit()
        full.pipeline.invalidate(
            [b.batch_id for b in full.pipeline.batches], "S1_trc_invert")
        reference = full.run_full(parallel=False)
        ref_stats, ref_hist = full.stats_and_histogram(reference)

        # 增量路径：先跑原始，再编辑只重算受影响批
        incr = _fresh_workbench()
        incr.load_image(IMG, 16)
        incr.run_full(parallel=False)
        output = incr.edit_and_recompute(
            _edit_sequence,
            affected_batches=[b.batch_id for b in incr.pipeline.batches])

        np.testing.assert_array_equal(output, reference)
        report = incr.verify_equivalence(reference, ref_stats, ref_hist)
        self.assertTrue(report["equal"])

    def test_reuse_count_meets_declared_floor(self):
        wb = _fresh_workbench()
        wb.load_image(IMG, 16)
        wb.run_full(parallel=False)

        def intent_edit(ed):
            ed.edit_intent(INTENT_PERCEPTUAL)
        wb.edit_and_recompute(intent_edit)
        total_hits = sum(b.cache_hits for b in wb.pipeline.batches)
        self.assertGreaterEqual(total_hits, DECLARED_REUSE_FLOOR)

    def test_diff_locates_first_mismatch_batch_and_stage(self):
        wb = _fresh_workbench()
        wb.load_image(IMG, 16)
        out = wb.run_full(parallel=False)
        stats, hist = wb.stats_and_histogram(out)
        # 人为篡改参考图像模拟不一致
        tampered = out.copy()
        tampered[3, 2, 0] = np.uint16(tampered[3, 2, 0] + 1)
        with self.assertRaises(CwbError) as ctx:
            wb.verify_equivalence(tampered, stats, hist)
        self.assertEqual(ctx.exception.code, E_DIFF)
        self.assertEqual(tuple(ctx.exception.detail["first_mismatch"][:2]),
                         (3, 2))
        self.assertIn("batch", ctx.exception.detail)
        self.assertIn("stage", ctx.exception.detail)

    def test_multi_edit_sequence_remains_equivalent(self):
        full = _fresh_workbench()
        full.load_image(IMG, 16)
        full.run_full(parallel=False)
        incr = _fresh_workbench()
        incr.load_image(IMG, 16)
        incr.run_full(parallel=False)

        edits = [
            lambda ed: ed.edit_intent(INTENT_ABSOLUTE),
            lambda ed: ed.edit_trc_gamma(2.4),
            lambda ed: ed.edit_white_point([0.95, 1.0, 1.09]),
            lambda ed: ed.edit_matrix(
                [0.41, 0.35, 0.18, 0.21, 0.72, 0.07, 0.02, 0.12, 0.95]),
        ]
        for i, edit in enumerate(edits):
            edit(full.editor)
            full.editor.commit()
            full.pipeline.invalidate(
                [b.batch_id for b in full.pipeline.batches],
                "S1_trc_invert")
            ref = full.run_full(parallel=False)
            ref_stats, ref_hist = full.stats_and_histogram(ref)

            out = incr.edit_and_recompute(edit)
            np.testing.assert_array_equal(out, ref)
            self.assertTrue(
                incr.verify_equivalence(ref, ref_stats, ref_hist)["equal"])


if __name__ == "__main__":
    unittest.main()
