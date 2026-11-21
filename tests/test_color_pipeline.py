import unittest

import numpy as np

from hdrworkbench import color
from hdrworkbench.errors import E_MODE_INVALID, ModeInvalidError
from hdrworkbench.generator import SampleSpec, generate_frames
from hdrworkbench.hdrformat import (
    CS_DISPLAY_P3,
    CS_REC2020,
    CS_REC709,
    TF_HLG,
    TF_PQ,
    TF_SDR,
    Header,
)
from hdrworkbench.pipeline import (
    STEP_NAMES,
    ConversionConfig,
    convert_sequence,
    diff_summary,
)


class ColorMathTests(unittest.TestCase):
    def test_identity_matrix_when_same_space(self):
        matrix = color.color_transform_matrix(CS_REC2020, CS_REC2020)
        np.testing.assert_allclose(matrix, np.eye(3), atol=1e-12)

    def test_white_adapt_identity_for_d65(self):
        np.testing.assert_array_equal(
            color.white_adaptation_matrix(), np.eye(3)
        )

    def test_transfer_roundtrips(self):
        x = np.linspace(0.0, 1.0, 64)
        for tf in (TF_SDR, TF_PQ, TF_HLG):
            recovered = color.linearize(color.encode(x, tf), tf)
            np.testing.assert_allclose(recovered, x, atol=1e-9)

    def test_tone_curves_monotonic(self):
        x = np.linspace(0.0, 2.0, 64)
        for curve in color.TONE_CURVES:
            y = color.tone_map(x, curve, 1000.0, 1000.0)
            self.assertTrue(np.all(np.diff(y.reshape(-1)) >= -1e-12))


def _header(transfer=TF_PQ, colorspace=CS_REC2020, peak=1000, depth=10):
    return Header(8, 6, 2, depth, colorspace, transfer, peak)


def _sequence(spec):
    from hdrworkbench.hdrformat import FrameSequence

    header = Header(
        spec.width,
        spec.height,
        spec.frame_count,
        spec.bit_depth,
        spec.colorspace,
        spec.transfer,
        spec.peak_nits,
    )
    return FrameSequence(header=header, raw_frames=generate_frames(spec))


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.spec = SampleSpec(10, 8, 3, 10, CS_REC2020, TF_PQ, 1000, seed=7)
        self.seq = _sequence(self.spec)
        self.base = ConversionConfig(
            target_colorspace=CS_REC709,
            target_transfer=TF_SDR,
            target_bit_depth=10,
            target_peak_nits=100,
            tone_curve=color.TONE_REINHARD,
            mode="fixed",
            frac_bits=12,
            tolerance_codes=4.0,
            seed=3,
        )

    def test_float_and_fixed_both_run_and_differ_bounded(self):
        float_cfg = ConversionConfig(**{**self.base.factor_dict(), "mode": "float"})
        fixed = convert_sequence(self.seq, self.base)
        floats = convert_sequence(self.seq, float_cfg)
        self.assertEqual(len(fixed), 3)
        for result in fixed:
            self.assertTrue(np.isfinite(result.pixel_diff).all())
            self.assertEqual(set(result.stage_hashes), set(STEP_NAMES))
            self.assertTrue(all(v >= 0.0 for v in result.stage_max_abs.values()))
        diffs = [
            np.abs(
                a.codes.astype(np.int32) - b.codes.astype(np.int32)
            ).max()
            for a, b in zip(fixed, floats)
        ]
        # 12 位小数定点与双精度浮点在 10-bit 码值上应高度接近
        self.assertLessEqual(max(diffs), 6)

    def test_more_frac_bits_reduces_error(self):
        coarse = ConversionConfig(**{**self.base.factor_dict(), "frac_bits": 6})
        fine = ConversionConfig(**{**self.base.factor_dict(), "frac_bits": 24})
        coarse_max = max(r.max_diff_codes for r in convert_sequence(self.seq, coarse))
        fine_max = max(r.max_diff_codes for r in convert_sequence(self.seq, fine))
        self.assertLessEqual(fine_max, coarse_max)

    def test_rounding_directions_recorded(self):
        results = convert_sequence(self.seq, self.base)
        directions = results[0].rounding_directions
        self.assertEqual(
            set(directions),
            {"linearize", "primaries", "adapt", "tonemap", "encode"},
        )
        self.assertTrue(
            set(np.unique(directions["linearize"])).issubset({-1, 0, 1})
        )

    def test_invalid_mode_emits_no_frames(self):
        bad = ConversionConfig(**{**self.base.factor_dict(), "mode": "quantum"})
        with self.assertRaises(ModeInvalidError) as ctx:
            convert_sequence(self.seq, bad)
        self.assertEqual(ctx.exception.code, E_MODE_INVALID)

    def test_invalid_frac_bits(self):
        bad = ConversionConfig(**{**self.base.factor_dict(), "frac_bits": 0})
        with self.assertRaises(ModeInvalidError):
            convert_sequence(self.seq, bad)

    def test_target_change_summary(self):
        cfg_a = ConversionConfig(**{**self.base.factor_dict()})
        cfg_b = ConversionConfig(
            **{**self.base.factor_dict(), "target_colorspace": CS_DISPLAY_P3}
        )
        summary = diff_summary(
            convert_sequence(self.seq, cfg_a),
            convert_sequence(self.seq, cfg_b),
        )
        self.assertEqual(summary["frames_compared"], 3)
        self.assertIn("per_frame", summary)

    def test_stochastic_deterministic_with_seed(self):
        cfg = ConversionConfig(
            **{**self.base.factor_dict(), "rounding": "stochastic"}
        )
        first = convert_sequence(self.seq, cfg)
        second = convert_sequence(self.seq, cfg)
        for a, b in zip(first, second):
            np.testing.assert_array_equal(a.codes, b.codes)


if __name__ == "__main__":
    unittest.main()

