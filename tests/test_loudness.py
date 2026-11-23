"""门限积分响度与 E_PARA 参数校验测试。"""
from __future__ import annotations

import unittest

import numpy as np

from loudbench.core.config import FrameConfig
from loudbench.core.errors import ParameterError
from loudbench.dsp.loudness import measure_loudness


class LoudnessTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fs = 48000
        rng = np.random.default_rng(7)
        t = np.arange(int(3.0 * self.fs)) / self.fs
        tone = 0.2 * np.sin(2 * np.pi * 440 * t)
        noise = 0.05 * rng.standard_normal(t.size)
        self.stereo = np.stack([tone + noise, tone * 0.8], axis=1)
        self.mono = tone + noise

    def test_mono_stereo_shapes_and_finite(self) -> None:
        for x in (self.mono, self.stereo):
            result = measure_loudness(x, self.fs)
            self.assertTrue(np.isfinite(result.integrated_lufs))
            self.assertGreater(result.frame_count, 0)
            self.assertEqual(result.frame_loudness.shape[0],
                             result.frame_count)

    def test_silence_gated_out(self) -> None:
        fs = self.fs
        t = np.arange(int(4.0 * fs)) / fs
        x = 0.2 * np.sin(2 * np.pi * 330 * t)
        x[int(fs * 1.0):int(fs * 3.0)] = 0.0  # 2 秒静音
        result = measure_loudness(x, fs)
        self.assertLess(result.gated_frame_count, result.frame_count)

    def test_loudness_scales_with_level(self) -> None:
        quiet = measure_loudness(self.mono * 0.5, self.fs).integrated_lufs
        loud = measure_loudness(self.mono, self.fs).integrated_lufs
        # 幅度减半 -> 响度约 -6.02 LKFS
        self.assertAlmostEqual(loud - quiet, 6.0206, delta=0.3)

    def test_unsupported_sample_rate_returns_epara(self) -> None:
        with self.assertRaises(ParameterError) as ctx:
            measure_loudness(self.mono, 22050)
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertEqual(ctx.exception.field, "sample_rate")

    def test_bad_frame_params_return_epara(self) -> None:
        bad = FrameConfig(frame_seconds=0.0)
        with self.assertRaises(ParameterError) as ctx:
            measure_loudness(self.mono, self.fs, bad)
        self.assertEqual(ctx.exception.code, "E_PARA")
        self.assertEqual(ctx.exception.field, "frame_seconds")
        bad_hop = FrameConfig(frame_seconds=0.4, hop_seconds=0.9)
        with self.assertRaises(ParameterError) as ctx:
            measure_loudness(self.mono, self.fs, bad_hop)
        self.assertEqual(ctx.exception.field, "hop_seconds")

    def test_all_silence_gives_negative_inf(self) -> None:
        x = np.zeros(self.fs)
        result = measure_loudness(x, self.fs)
        self.assertEqual(result.integrated_lufs, -np.inf)


if __name__ == "__main__":
    unittest.main()
