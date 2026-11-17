from __future__ import annotations

import unittest

import numpy as np

from drum_bus.dsp import integrated_loudness, limit_true_peak, true_peak_dbtp


class DspTests(unittest.TestCase):
    def test_bs1770_stereo_reference(self) -> None:
        sample_rate = 48000
        frames = sample_rate * 4
        time = np.arange(frames, dtype=np.float64) / sample_rate
        amplitude = 10.0 ** (-23.0 / 20.0)
        mono = amplitude * np.sin(2.0 * np.pi * 1000.0 * time)
        stereo = np.stack((mono, mono), axis=1)
        measured = integrated_loudness(stereo, sample_rate)
        self.assertAlmostEqual(measured, -23.0, delta=0.12)

    def test_true_peak_of_sine(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate, dtype=np.float64) / sample_rate
        sine = 0.5 * np.sin(2.0 * np.pi * 1000.0 * time)
        measured, _ = true_peak_dbtp(sine)
        self.assertAlmostEqual(measured, 20.0 * np.log10(0.5), delta=0.05)

    def test_limiter_preserves_length_and_ceiling(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate, dtype=np.float64) / sample_rate
        sine = np.sin(2.0 * np.pi * 1000.0 * time)
        stereo = np.stack((sine, sine), axis=1)
        limited, stats = limit_true_peak(stereo, -1.0, sample_rate)
        peak, _ = true_peak_dbtp(limited)
        self.assertEqual(limited.shape, stereo.shape)
        self.assertLessEqual(peak, -0.99)
        self.assertGreater(stats.triggered_frames, 0)
        self.assertEqual(stats.triggered_channel_samples, stats.triggered_frames * 2)


if __name__ == "__main__":
    unittest.main()

