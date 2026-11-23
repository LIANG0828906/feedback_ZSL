"""全局响度归一与受控二次增益修正测试。"""
from __future__ import annotations

import unittest

import numpy as np

from loudbench.core.config import (
    FrameConfig, NormalizeConfig)
from loudbench.core.errors import StateError
from loudbench.dsp.kweighting import true_peak_db
from loudbench.pipeline.normalizer import GlobalNormalizer


class NormalizerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fs = 48000
        self.cfg = NormalizeConfig(target_lufs=-23.0)
        self.frame = FrameConfig()
        self.norm = GlobalNormalizer(
            self.cfg, self.frame, ceiling_db=-3.0)

    def _tone(self, rms_db: float, seconds: float = 3.0) -> np.ndarray:
        n = int(seconds * self.fs)
        t = np.arange(n) / self.fs
        x = np.sin(2 * np.pi * 440 * t)
        return x * (10.0 ** (rms_db / 20.0))

    def test_sequential_submit_hits_target(self) -> None:
        for i, rms in enumerate((-20.0, -30.0, -24.0)):
            out, record = self.norm.submit(i, self._tone(rms), self.fs)
            self.assertEqual(out.shape[0], int(3.0 * self.fs))
            self.assertAlmostEqual(record.output_lufs, -23.0, delta=0.2)
        ok, overall = self.norm.verify_target()
        self.assertTrue(ok)
        self.assertAlmostEqual(overall, -23.0, delta=0.5)

    def test_out_of_order_submit_raises_estate(self) -> None:
        with self.assertRaises(StateError) as ctx:
            self.norm.submit(1, self._tone(-20.0), self.fs)
        self.assertEqual(ctx.exception.code, "E_STATE")

    def test_secondary_correction_when_peak_exceeds_ceiling(self) -> None:
        # 低积分响度 + 高瞬态峰值：归一增益会把真峰推过 -3 dBTP，
        # 触发受控二次衰减（非裁剪）
        n = int(3.0 * self.fs)
        t = np.arange(n) / self.fs
        x = 0.02 * np.sin(2 * np.pi * 330 * t)
        center = int(1.5 * self.fs)
        width = self.fs // 1000
        env = np.sin(np.pi * np.arange(2 * width) / (2 * width)) ** 2
        x[center - width:center + width] += 0.9 * env
        out, record = self.norm.submit(0, x, self.fs)
        self.assertLess(record.secondary_correction_db, 0.0)
        self.assertLessEqual(
            true_peak_db(out, 4), -3.0 + 1e-6)
        # 二次修正是增益衰减而非裁剪：无硬削波平台
        self.assertGreater(np.max(np.abs(out)), 0.0)

    def test_gain_traceability(self) -> None:
        for i, rms in enumerate((-22.0, -26.0)):
            _out, record = self.norm.submit(i, self._tone(rms), self.fs)
            self.assertAlmostEqual(
                record.total_gain_db,
                record.primary_gain_db + record.secondary_correction_db,
                places=9)
        self.assertEqual(len(self.norm.state.records), 2)


if __name__ == "__main__":
    unittest.main()
