import unittest

import numpy as np

from space_audio.acoustics import (
    BinauralPanner,
    binaural_params,
    build_impulse_response,
)
from space_audio.demo import build_demo_scene
from space_audio.model import build_frame_table


class AcousticsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = build_demo_scene()
        self.table = build_frame_table(self.scene)

    def test_itd_ild_symmetric_and_bounded(self) -> None:
        itd_right, ild_right, _, _ = binaural_params(np.pi / 4, 0.0)
        itd_left, ild_left, _, _ = binaural_params(-np.pi / 4, 0.0)
        self.assertAlmostEqual(itd_right, -itd_left, places=12)
        self.assertAlmostEqual(ild_right, -ild_left, places=12)
        self.assertLessEqual(abs(itd_right), 0.7e-3 + 1e-12)

    def test_frame_itd_continuous_after_unwrap(self) -> None:
        for arrays in self.table["sources"].values():
            jumps = np.abs(np.diff(arrays["itd_s"]))
            self.assertLess(np.max(jumps), 120e-6)

    def test_rt60_respects_rate_limit(self) -> None:
        frame_rate = self.table["frame_rate"]
        limit = self.scene["project"]["max_rt60_rate_s"]
        rate = np.max(np.abs(np.diff(self.table["rt60"]))) * frame_rate
        self.assertLessEqual(rate, limit + 1e-9)

    def test_impulse_response_unit_energy_and_deterministic(self) -> None:
        seed = self.scene["project"]["seed"]
        first = build_impulse_response(1.2, 24000, seed, "arch")
        second = build_impulse_response(1.2, 24000, seed, "arch")
        np.testing.assert_array_equal(first, second)
        self.assertAlmostEqual(float(np.dot(first, first)), 1.0, places=9)

    def test_panner_sample_level_continuity(self) -> None:
        sample_rate = 24000
        panner = BinauralPanner(sample_rate)
        n = 4800
        itd = np.linspace(-0.6e-3, 0.6e-3, n)
        gain = np.ones(n)
        signal = np.sin(2.0 * np.pi * 220.0 * np.arange(n) / sample_rate)
        rendered = panner.render(signal, itd, gain, gain)
        self.assertEqual(rendered.shape, (n, 2))
        self.assertLess(np.max(np.abs(np.diff(rendered[:, 0]))), 0.06)
        self.assertLess(np.max(np.abs(np.diff(rendered[:, 1]))), 0.06)


if __name__ == "__main__":
    unittest.main()

