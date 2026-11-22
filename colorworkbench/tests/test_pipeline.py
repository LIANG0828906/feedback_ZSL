"""业务要求 2/3：转换管线、位深保持、TRC 差异、意图与白点约束。"""

import json
import os
import unittest

import numpy as np

from cwb.engine import Workbench
from cwb.errors import CwbError, E_DEPTH, E_WP
from cwb.model import (
    INTENT_ABSOLUTE,
    INTENT_PERCEPTUAL,
    INTENT_RELATIVE,
    TRC,
)
from cwb.trc import trc_inversion_difference

SAMPLES = os.path.join(os.path.dirname(__file__), "..", "samples")


def profile(name):
    return os.path.join(SAMPLES, "profiles", name + ".json")


def image(name):
    return os.path.join(SAMPLES, "images", name + ".cimg")


class PipelineTests(unittest.TestCase):
    def test_depth_and_channels_preserved_8bit(self):
        wb = Workbench(profile("src_v1"), profile("dst_v1"),
                       INTENT_RELATIVE, output_depth=8)
        src = wb.load_image(image("tile_16x16_8_rgb"), expected_depth=8)
        out = wb.run_full(parallel=False)
        self.assertEqual(out.dtype, np.uint8)
        self.assertEqual(out.shape, src.shape)

    def test_16bit_not_silently_downgraded(self):
        wb = Workbench(profile("src_v3"), profile("dst_v3"),
                       INTENT_RELATIVE, output_depth=16)
        wb.load_image(image("tile_32x24_16_rgb"), expected_depth=16)
        out = wb.run_full(parallel=False)
        self.assertEqual(out.dtype, np.uint16)

    def test_depth_mismatch_returns_E_DEPTH(self):
        wb = Workbench(profile("src_v3"), profile("dst_v3"),
                       INTENT_RELATIVE, output_depth=16)
        with self.assertRaises(CwbError) as ctx:
            wb.load_image(image("tile_16x16_8_rgb"), expected_depth=16)
        self.assertEqual(ctx.exception.code, E_DEPTH)

    def test_gray_and_rgba_channels_preserved(self):
        for name, ch in (("tile_24x32_8_gray", 1),
                         ("tile_40x40_16_rgba", 4),
                         ("tile_48x32_8_rgba", 2)):
            depth = 16 if "16" in name else 8
            wb = Workbench(profile("src_v1"), profile("dst_v1"),
                           INTENT_RELATIVE, output_depth=depth)
            src = wb.load_image(image(name), expected_depth=depth)
            out = wb.run_full(parallel=False)
            self.assertEqual(out.shape[2], ch, name)
            self.assertEqual(out.shape[:2], src.shape[:2], name)

    def test_table_vs_parametric_trc_recorded_difference(self):
        with open(os.path.join(SAMPLES, "manifest.json")) as fh:
            ref = json.load(fh)["trc_inversion_reference"]
        param = TRC(kind="parametric", curve="gamma", gamma=2.2)
        table = TRC(kind="table",
                    table=tuple(np.linspace(0, 1, 33) ** 2.2))
        record = trc_inversion_difference(param, table)
        self.assertGreater(record["max_abs_diff"], 0.0)
        self.assertAlmostEqual(record["max_abs_diff"],
                               ref["max_abs_diff"], places=12)

    def test_intents_share_model_but_differ_observably(self):
        outputs = {}
        for intent in (INTENT_RELATIVE, INTENT_ABSOLUTE,
                       INTENT_PERCEPTUAL):
            wb = Workbench(profile("src_v3"), profile("dst_v3"),
                           intent, output_depth=16)
            wb.load_image(image("tile_32x24_16_rgb"), 16)
            outputs[intent] = wb.run_full(parallel=False).astype(np.float64)
        # 浮点 PCS 阶段：感知意图对每个像素单调收缩、可解释
        probe = np.linspace(0.0, 1.3, 500)
        xyz = np.stack([probe, probe * 0.8, probe * 0.6], axis=1)
        wp = wb.pipeline._dst_wp
        from cwb.color_math import adapt_and_render
        rel_f = adapt_and_render(xyz, wp, wp, INTENT_RELATIVE)
        perc_f = adapt_and_render(xyz, wp, wp, INTENT_PERCEPTUAL)
        self.assertTrue(np.all(perc_f <= rel_f + 1e-12))
        self.assertGreater(np.abs(perc_f - rel_f).mean(), 0.0)
        d_abs = np.abs(outputs[INTENT_ABSOLUTE]
                       - outputs[INTENT_RELATIVE]).mean()
        d_perc = np.abs(outputs[INTENT_PERCEPTUAL]
                        - outputs[INTENT_RELATIVE]).mean()
        self.assertGreater(d_abs, 0.0)
        self.assertGreater(d_perc, 0.0)

    def test_missing_white_point_uses_declared_default_with_trace(self):
        wb = Workbench(profile("src_v2"), profile("dst_v2"),
                       INTENT_RELATIVE, output_depth=16)
        wb.load_image(image("tile_32x24_16_rgb"), 16)
        wb.run_full(parallel=False)
        self.assertTrue(wb.pipeline.traces)
        self.assertEqual(wb.pipeline.traces[0].stage, "white_point")

    def test_missing_white_point_and_no_declared_returns_E_WP(self):
        wb = Workbench(profile("src_v2"), profile("dst_v2"),
                       INTENT_RELATIVE, output_depth=16)
        wb.source_profile.white_point = None
        wb.source_profile.declared_default_white_point = None
        wb.load_image(image("tile_32x24_16_rgb"), 16)
        with self.assertRaises(CwbError) as ctx:
            wb.run_full(parallel=False)
        self.assertEqual(ctx.exception.code, E_WP)

    def test_version2_lut3d_pipeline_runs_and_preserves_depth(self):
        wb = Workbench(profile("src_v2"), profile("dst_v2"),
                       INTENT_RELATIVE, output_depth=16)
        wb.load_image(image("tile_32x24_16_rgb"), 16)
        out = wb.run_full(parallel=False)
        self.assertEqual(out.dtype, np.uint16)
        self.assertEqual(out.shape, (32, 24, 3))
        # 走到了 LUT 分支
        self.assertIsNotNone(wb.config.source.lut3d)

    def test_intent_switch_reuses_unaffected_pipeline_stages(self):
        wb = Workbench(profile("src_v3"), profile("dst_v3"),
                       INTENT_RELATIVE, output_depth=16, batch_rows=8)
        wb.load_image(image("tile_32x24_16_rgb"), 16)
        wb.run_full(parallel=False)
        s1 = wb.pipeline.batches[0].stages["S1_trc_invert"]
        s2 = wb.pipeline.batches[0].stages["S2_to_pcs"]

        def edit(editor):
            editor.edit_intent(INTENT_ABSOLUTE)

        wb.edit_and_recompute(edit)
        self.assertIs(wb.pipeline.batches[0].stages["S1_trc_invert"], s1)
        self.assertIs(wb.pipeline.batches[0].stages["S2_to_pcs"], s2)
        self.assertGreater(
            sum(b.cache_hits for b in wb.pipeline.batches), 0)


if __name__ == "__main__":
    unittest.main()
