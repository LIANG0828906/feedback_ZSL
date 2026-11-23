"""压缩器与限幅器测试：平滑、长度/通道保持、真峰上限、E_PARA。"""
from __future__ import annotations

import unittest

import numpy as np

from loudbench.core.config import CompressorConfig, LimiterConfig
from loudbench.core.errors import ParameterError
from loudbench.dsp.compressor import compress
from loudbench.dsp.kweighting import true_peak_db
from loudbench.dsp.limiter import limit


class DynamicsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fs = 48000
        n = int(2.0 * self.fs)
        t = np.arange(n) / self.fs
        # 大部分时间低电平，中段出现高峰
        x = 0.05 * np.sin(2 * np.pi * 300 * t)
        x[int(0.5 * self.fs):int(1.5 * self.fs)] += 0.8 * np.sin(
            2 * np.pi * 500 * t[:self.fs])
        self.mono = x
        self.stereo = np.stack([x, x * 0.7], axis=1)

    def test_compressor_preserves_length_and_channels(self) -> None:
        for x in (self.mono, self.stereo):
            result = compress(x, self.fs, CompressorConfig())
            self.assertEqual(result.samples.shape, x.shape)

    def test_attack_release_smooth_no_step(self) -> None:
        result = compress(self.mono, self.fs, CompressorConfig())
        diffs = np.abs(np.diff(result.gain_reduction_db))
        # 逐样本增益变化必须远小于阶跃量（平滑约束）
        self.assertLess(np.max(diffs), 0.5)

    def test_compressor_reduces_loud_peaks(self) -> None:
        cfg = CompressorConfig(threshold_db=-24.0, ratio=8.0)
        result = compress(self.mono, self.fs, cfg)
        self.assertLess(np.max(np.abs(result.samples)),
                        np.max(np.abs(self.mono)) + 1e-9)

    def test_compressor_bad_params_epara(self) -> None:
        with self.assertRaises(ParameterError) as ctx:
            compress(self.mono, self.fs, CompressorConfig(ratio=0.5))
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertEqual(ctx.exception.field, "ratio")

    def test_limiter_respects_ceiling_true_peak(self) -> None:
        cfg = LimiterConfig(ceiling_db=-3.0, lookahead_seconds=0.005)
        for x in (self.mono, self.stereo):
            result = limit(x, self.fs, cfg)
            self.assertEqual(result.samples.shape, x.shape)
            tp = true_peak_db(result.samples, 4)
            self.assertLessEqual(tp, cfg.ceiling_db + 1e-6)

    def test_limiter_lookahead_smooth_no_step(self) -> None:
        cfg = LimiterConfig(ceiling_db=-6.0, lookahead_seconds=0.005)
        result = limit(self.mono, self.fs, cfg)
        diffs = np.abs(np.diff(result.gain_db))
        self.assertLess(np.max(diffs), 1.0)

    def test_limiter_bad_ceiling_epara(self) -> None:
        with self.assertRaises(ParameterError) as ctx:
            limit(self.mono, self.fs, LimiterConfig(ceiling_db=6.0))
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertEqual(ctx.exception.field, "ceiling_db")

    def test_limiter_unsupported_rate_epara(self) -> None:
        with self.assertRaises(ParameterError) as ctx:
            limit(self.mono, 11025, LimiterConfig())
        self.assertEqual(ctx.exception.field, "sample_rate")


if __name__ == "__main__":
    unittest.main()
