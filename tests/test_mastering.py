"""BS.1770 loudness, true-peak measurement and limiter guarantees."""

from __future__ import annotations
import os
import sys
import unittest

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from stage_noise.processing.mastering import (  # noqa: E402
    integrated_loudness, lookahead_limiter, true_peak_dbtp, _sliding_min)


class MasteringTests(unittest.TestCase):
    def test_sliding_min_reference(self):
        values = np.array([5.0, 1.0, 4.0, 2.0, 3.0, 0.0, 2.0])
        got = _sliding_min(values, 2)
        expected = np.array([
            min(values[max(0, i - 2):i + 3]) for i in range(len(values))])
        np.testing.assert_array_equal(got, expected)

    def test_limiter_meets_true_peak_ceiling(self):
        sr = 48000
        t = np.arange(sr * 3) / sr
        mono = 0.9 * np.sin(2 * np.pi * 440 * t) \
            + 0.5 * np.sin(2 * np.pi * 1200 * t)
        stereo = np.stack([mono, np.roll(mono, 50)], axis=1)
        self.assertGreater(true_peak_dbtp(stereo, sr), -1.0)
        margin = 0.5
        for _ in range(6):
            ceiling = 10 ** ((-1.0 - margin) / 20.0)
            limited, gain, lookahead = lookahead_limiter(stereo, sr, ceiling)
            if true_peak_dbtp(limited, sr) <= -1.0:
                break
            margin += 0.5
        self.assertLessEqual(true_peak_dbtp(limited, sr), -1.0)
        self.assertEqual(len(limited), len(stereo))
        self.assertGreater(np.count_nonzero(gain < 0.999999), 0)
        self.assertEqual(lookahead, int(round(0.005 * sr)))

    def test_integrated_loudness_scale(self):
        # -23 LUFS-ish calibrated tone: verify the meter is in a sane range
        # and shifts by ~6 dB when amplitude is halved.
        sr = 48000
        t = np.arange(sr * 5) / sr
        tone = 0.2 * np.sin(2 * np.pi * 1000 * t)
        stereo = np.stack([tone, tone], axis=1)
        loud = integrated_loudness(stereo, sr)
        quiet = integrated_loudness(stereo * 0.5, sr)
        self.assertAlmostEqual(loud - quiet, 6.0, places=1)
        self.assertTrue(-40 < loud < -5)


if __name__ == "__main__":
    unittest.main(verbosity=2)
