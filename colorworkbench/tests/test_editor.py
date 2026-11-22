"""业务要求 4：工作副本编辑、原子提交、回滚与失效范围查询。"""

import os
import unittest

import numpy as np

from cwb.engine import Workbench
from cwb.errors import CwbError, E_PARA
from cwb.model import INTENT_ABSOLUTE, INTENT_RELATIVE

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")


def profile(name):
    return os.path.join(SAMPLES, "profiles", name + ".json")


class EditorTests(unittest.TestCase):
    def setUp(self):
        self.wb = Workbench(profile("src_v3"), profile("dst_v3"),
                            INTENT_RELATIVE, output_depth=16, batch_rows=8)
        self.wb.load_image(os.path.join(SAMPLES, "images",
                                        "tile_32x24_16_rgb.cimg"), 16)
        self.wb.run_full(parallel=False)
        self.original_matrix = tuple(self.wb.config.source.matrix)

    def test_edit_only_touches_working_copy_before_commit(self):
        self.wb.editor.edit_matrix([0.1] * 9)
        self.assertEqual(tuple(self.wb.config.source.matrix),
                         self.original_matrix)

    def test_commit_invalidates_dependent_batches_and_stage(self):
        def edit(ed):
            ed.edit_matrix([0.2, 0.3, 0.1, 0.1, 0.6, 0.1, 0.0, 0.1, 0.7])
        self.wb.edit_and_recompute(edit, affected_batches=[0, 1])
        query = self.wb.editor.query_invalidation()
        self.assertEqual(query["batch_ids"], [0, 1])
        self.assertEqual(query["stage"], "S2_to_pcs")
        self.assertTrue(any("matrix" in r for r in query["reasons"]))

    def test_unaffected_batches_keep_cache(self):
        all_batches = [b.batch_id for b in self.wb.pipeline.batches]
        untouched = all_batches[2:]

        def edit(ed):
            ed.edit_matrix([0.2, 0.3, 0.1, 0.1, 0.6, 0.1, 0.0, 0.1, 0.7])
        self.wb.edit_and_recompute(edit, affected_batches=[0, 1])
        for bid in untouched:
            self.assertEqual(self.wb.pipeline.batches[bid].status, "done")

    def test_intent_switch_reuses_early_stages(self):
        before_s1 = dict(self.wb.pipeline.batches[0].stages)

        def edit(ed):
            ed.edit_intent(INTENT_ABSOLUTE)
        self.wb.edit_and_recompute(edit)
        after = self.wb.pipeline.batches[0].stages
        # S1/S2 未受意图影响，对象被复用
        self.assertIs(before_s1["S1_trc_invert"], after["S1_trc_invert"])
        self.assertIs(before_s1["S2_to_pcs"], after["S2_to_pcs"])

    def test_failed_commit_rolls_back_entire_batch(self):
        good = list(self.original_matrix)
        with self.assertRaises(CwbError) as ctx:
            def edit(ed):
                ed.edit_matrix(good)
                ed.edit_trc_gamma(99.0)  # 越界
            self.wb.edit_and_recompute(edit)
        self.assertEqual(ctx.exception.code, E_PARA)
        # 模型保持提交前状态
        self.assertEqual(tuple(self.wb.config.source.matrix),
                         self.original_matrix)
        self.assertEqual(self.wb.config.source.trc.gamma, 2.2)
        self.assertEqual(self.wb.editor.param_version, 1)

    def test_table_edit_validation(self):
        with self.assertRaises(CwbError) as ctx:
            self.wb.editor.edit_trc_table([0.0, 0.9, 0.2, 1.0])
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.tag, "trc_table")

    def test_output_depth_edit_invalidates_final_stage(self):
        self.assertEqual(self.wb.config.output_depth, 16)

        def edit(ed):
            ed.edit_output_depth(8)

        out = self.wb.edit_and_recompute(edit)
        self.assertEqual(out.dtype, np.uint8)
        query = self.wb.editor.query_invalidation()
        self.assertEqual(query["stage"], "S5_output_encode")
        # 早期阶段仍被复用
        self.assertIn("S1_trc_invert",
                      self.wb.pipeline.batches[0].stages)


if __name__ == "__main__":
    unittest.main()
