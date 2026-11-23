"""K 加权滤波器规范系数与真峰值测试。"""
from __future__ import annotations

import unittest

import numpy as np

from loudbench.dsp.kweighting import apply_k_weighting, k_weighting_sos, true_peak_db


class KWeightingTest(unittest.TestCase):
    def test_48k_coefficients_match_bs1770_annex(self) -> None:
        sos = k_weighting_sos(48000)
        # BS.1770-4 附录给定的高通级规范系数（a0 归一化后）
        hp = sos[1]
        expected_b = np.array([1.0, -2.0, 1.0])
        expected_a = np.array([1.0, -1.990047454833984, 0.990072250366211])
        np.testing.assert_allclose(hp[:3], expected_b, atol=1e-9)
        np.testing.assert_allclose(
            np.concatenate([[hp[3]], hp[4:]]), expected_a, atol=1e-5)
        # 搁架级与附录系数吻合（转角频率数值表示差异 < 1e-4）
        shelf = sos[0]
        shelf_exp = np.array([1.53512485958697, -2.69169618940638,
                              1.19839281085285, 1.0,
                              -1.69065929318241, 0.73248077421585])
        np.testing.assert_allclose(shelf, shelf_exp, atol=2e-4)

    def test_supported_rates_produce_stable_filter(self) -> None:
        for fs in (32000, 44100, 48000, 96000):
            sos = k_weighting_sos(fs)
            self.assertEqual(sos.shape, (2, 6))
            self.assertTrue(np.isfinite(sos).all())

    def test_shape_preserved_mono_and_stereo(self) -> None:
        fs = 48000
        mono = np.random.default_rng(1).standard_normal(fs)
        stereo = np.stack([mono, mono * 0.5], axis=1)
        self.assertEqual(apply_k_weighting(mono, fs).shape, mono.shape)
        self.assertEqual(apply_k_weighting(stereo, fs).shape, stereo.shape)

    def test_true_peak_detects_overshoot(self) -> None:
        fs = 48000
        # 采样点间峰值：高频正弦在样本间过冲
        t = np.arange(fs) / fs
        x = 0.707 * np.sin(2 * np.pi * 12000 * t + np.pi / 4)
        tp = true_peak_db(x, 4)
        self.assertGreater(tp, 20 * np.log10(np.max(np.abs(x))))


if __name__ == "__main__":
    unittest.main()
