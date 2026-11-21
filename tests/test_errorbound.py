import unittest
import warnings
from unittest import mock

import numpy as np

from hdrworkbench import color
from hdrworkbench.errors import E_ERROR_UNBOUNDED, ErrorUnboundedError
from hdrworkbench.errorbound import compute_frame_error_bound
from hdrworkbench.generator import SampleSpec, generate_frames
from hdrworkbench.hdrformat import (
    CS_DISPLAY_P3,
    CS_REC2020,
    CS_REC709,
    FrameSequence,
    Header,
    TF_HLG,
    TF_PQ,
    TF_SDR,
)
from hdrworkbench.pipeline import ConversionConfig, convert_frame


def _sequence():
    spec = SampleSpec(10, 8, 2, 10, CS_REC2020, TF_PQ, 1000, seed=11)
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


class ErrorBoundTests(unittest.TestCase):
    def setUp(self):
        self.seq = _sequence()
        self.matrix = color.color_transform_matrix(CS_REC2020, CS_REC2020)
        self.adapt = color.white_adaptation_matrix()
        self.config = ConversionConfig(
            target_colorspace=CS_REC2020,
            target_transfer=TF_PQ,
            target_bit_depth=10,
            target_peak_nits=1000,
            tone_curve=color.TONE_REINHARD,
            mode="fixed",
            frac_bits=10,
            tolerance_codes=2.0,
        )

    def test_bounds_are_finite_non_negative_and_per_pixel(self):
        signal = self.seq.normalized_frames()[0]
        report = compute_frame_error_bound(
            0,
            signal,
            self.seq.header,
            self.config,
            self.matrix,
            self.adapt,
            threshold_codes=2.0,
        )
        self.assertEqual(report.bound_codes.shape, signal.shape)
        self.assertTrue(np.all(report.bound_codes >= 0.0))
        self.assertTrue(np.all(np.isfinite(report.bound_codes)))
        for step, bounds in report.stage_bounds.items():
            self.assertEqual(bounds.shape, signal.shape, step)

    def test_actual_deviation_never_exceeds_bound(self):
        signal = self.seq.normalized_frames()[0]
        float_cfg = ConversionConfig(
            **{**self.config.factor_dict(), "mode": "float"}
        )
        fixed_result = convert_frame(0, signal, "h", self.seq.header, self.config)
        float_result = convert_frame(0, signal, "h", self.seq.header, float_cfg)
        report = compute_frame_error_bound(
            0,
            signal,
            self.seq.header,
            self.config,
            self.matrix,
            self.adapt,
            threshold_codes=2.0,
        )
        actual = np.abs(
            fixed_result.codes.astype(np.float64)
            - float_result.codes.astype(np.float64)
        )
        # 保守上界必须包住真实偏差
        self.assertTrue(np.all(actual <= report.bound_codes + 1e-9))

    def test_first_offending_stage_attribution(self):
        signal = self.seq.normalized_frames()[0]
        report = compute_frame_error_bound(
            0,
            signal,
            self.seq.header,
            self.config,
            self.matrix,
            self.adapt,
            threshold_codes=0.3,
        )
        self.assertTrue(report.untrusted.any())
        stages = report.first_offending_stage[report.untrusted]
        self.assertTrue(all(stage is not None for stage in stages))
        # 最早越界环节必须按链路顺序出现
        order = {
            "linearize": 0,
            "primaries": 1,
            "adapt": 2,
            "tonemap": 3,
            "encode": 4,
            "quantize": 5,
        }
        self.assertTrue(all(stage in order for stage in stages))

    def test_fixed_bound_looser_than_float(self):
        signal = self.seq.normalized_frames()[0]
        float_cfg = ConversionConfig(
            **{**self.config.factor_dict(), "mode": "float"}
        )
        fixed_report = compute_frame_error_bound(
            0, signal, self.seq.header, self.config,
            self.matrix, self.adapt, 2.0
        )
        float_report = compute_frame_error_bound(
            0, signal, self.seq.header, float_cfg,
            self.matrix, self.adapt, 2.0
        )
        self.assertGreaterEqual(
            fixed_report.bound_codes.max(),
            float_report.bound_codes.max(),
        )

    def test_non_finite_propagation_raises_unbounded_with_candidates(self):
        signal = self.seq.normalized_frames()[0]
        broken = lambda *args, **kwargs: np.full_like(signal, np.inf)
        with mock.patch.object(color, "linearize", broken), np.errstate(
            invalid="ignore"
        ), warnings.catch_warnings():
            warnings.simplefilter("ignore", RuntimeWarning)
            with self.assertRaises(ErrorUnboundedError) as ctx:
                compute_frame_error_bound(
                    0,
                    signal,
                    self.seq.header,
                    self.config,
                    self.matrix,
                    self.adapt,
                    2.0,
                )
        self.assertEqual(ctx.exception.code, E_ERROR_UNBOUNDED)
        self.assertTrue(ctx.exception.details["candidates"])
        self.assertEqual(ctx.exception.details["bad_frame"], 0)

    def test_bounds_conservative_across_full_factor_grid(self):
        for tf, peak in ((TF_PQ, 1000), (TF_HLG, 1000), (TF_SDR, 100)):
            for src in (CS_REC709, CS_REC2020, CS_DISPLAY_P3):
                for dst in (CS_REC709, CS_REC2020, CS_DISPLAY_P3):
                    for frac_bits in (6, 12, 20):
                        spec = SampleSpec(
                            6, 5, 1, 10, src, tf, peak, seed=2
                        )
                        header = Header(
                            spec.width,
                            spec.height,
                            1,
                            spec.bit_depth,
                            spec.colorspace,
                            spec.transfer,
                            spec.peak_nits,
                        )
                        seq = FrameSequence(
                            header=header, raw_frames=generate_frames(spec)
                        )
                        signal = seq.normalized_frames()[0]
                        cfg = ConversionConfig(
                            target_colorspace=dst,
                            target_transfer=tf,
                            target_bit_depth=10,
                            target_peak_nits=min(peak, 2000),
                            tone_curve=color.TONE_HABLE,
                            mode="fixed",
                            frac_bits=frac_bits,
                            tolerance_codes=4.0,
                        )
                        fcfg = ConversionConfig(
                            **{**cfg.factor_dict(), "mode": "float"}
                        )
                        matrix = color.color_transform_matrix(src, dst)
                        adapt = color.white_adaptation_matrix()
                        fixed = convert_frame(
                            0, signal, "h", header, cfg
                        )
                        floats = convert_frame(
                            0, signal, "h", header, fcfg
                        )
                        report = compute_frame_error_bound(
                            0, signal, header, cfg, matrix, adapt, 4.0
                        )
                        actual = np.abs(
                            fixed.codes.astype(np.float64)
                            - floats.codes.astype(np.float64)
                        )
                        self.assertTrue(
                            np.all(actual <= report.bound_codes + 1e-9),
                            f"上界失效: tf={tf} src={src} dst={dst} fb={frac_bits}",
                        )


if __name__ == "__main__":
    unittest.main()

