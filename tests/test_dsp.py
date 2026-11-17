from __future__ import annotations

import math
import unittest

import numpy as np

from biomemaster.constants import SOFT_LIMIT_THRESHOLD_DBFS
from biomemaster.dsp import (
    crossfade_count,
    db_to_linear,
    render_biome_chunk,
    render_layer_chunk,
    soft_limit,
    true_peak_limiter,
)
from biomemaster.loudness import integrated_loudness_from_audio
from biomemaster.model import Biome, Curve, Layer, LayerSpec
from biomemaster.truepeak import true_peak_dbtp_array


class DspTests(unittest.TestCase):
    @staticmethod
    def _spec(name: str, loop_end: int) -> LayerSpec:
        return LayerSpec(
            name=name,
            path=None,  # type: ignore[arg-type]
            relative_path=f"{name}.wav",
            loop_start=0,
            loop_end=loop_end,
            base_gain_db=0.0,
            curve=Curve(
                times_seconds=np.array([0.0, 10.0]),
                gains_db=np.array([0.0, 0.0]),
            ),
            source_channels=1,
            source_frames=loop_end,
            source_sha256="0" * 64,
        )

    def test_loop_ola_keeps_requested_length(self) -> None:
        source = np.linspace(0.1, 0.9, 1001, dtype=np.float64)
        spec = self._spec("test", 1001)
        rendered = render_layer_chunk(
            spec,
            source[:, None],
            start=0,
            count=5000,
            sample_rate=48000,
            crossfade_samples=200,
        )
        self.assertEqual(rendered.shape, (5000, 2))
        self.assertTrue(np.all(np.isfinite(rendered)))
        self.assertGreater(crossfade_count(1001, 200, 5000), 0)

        biome = Biome(
            name="b",
            weight=1.0,
            layers=(
                Layer(spec=spec, loop_audio=source[:, None]),
                Layer(
                    spec=self._spec("test2", 701),
                    loop_audio=source[:701, None],
                ),
            ),
        )
        self.assertEqual(
            render_biome_chunk(biome, 0, 8192, 48000, 200).shape, (8192, 2)
        )

    def test_soft_limiter_never_exceeds_threshold(self) -> None:
        audio = np.array([[-4.0, -0.9], [0.0, 0.49], [0.7, 2.5]], dtype=np.float64)
        limited, stats = soft_limit(audio)
        ceiling = float(db_to_linear(SOFT_LIMIT_THRESHOLD_DBFS))
        self.assertLessEqual(float(np.max(np.abs(limited))), ceiling + 1e-12)
        self.assertGreater(stats.trigger_samples, 0)
        self.assertGreater(stats.trigger_frames, 0)

    def test_true_peak_limiter_stays_below_ceiling(self) -> None:
        audio = np.array([[-8.0, 0.3], [1.8, 4.0]], dtype=np.float64)
        limited, stats = true_peak_limiter(audio, ceiling_db=-1.0)
        self.assertLessEqual(float(np.max(np.abs(limited))), db_to_linear(-1.0))
        self.assertGreater(stats.trigger_samples, 0)

    def test_bs1770_relative_level(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate * 2, dtype=np.float64) / sample_rate
        sine = np.sin(2.0 * math.pi * 997.0 * time) * 0.1
        loud = np.stack([sine, sine], axis=1)
        quiet = loud / (10.0 ** (3.0 / 20.0))
        loudness = integrated_loudness_from_audio(loud, sample_rate)
        quiet_loudness = integrated_loudness_from_audio(quiet, sample_rate)
        self.assertAlmostEqual(loudness - quiet_loudness, 3.0, places=4)
        self.assertTrue(math.isfinite(loudness))

    def test_oversampled_true_peak_is_finite(self) -> None:
        sample_rate = 48000
        time = np.arange(4096, dtype=np.float64) / sample_rate
        audio = np.stack([0.5 * np.sin(2 * math.pi * 997 * time)] * 2, axis=1)
        peak = true_peak_dbtp_array(audio)
        self.assertTrue(math.isfinite(peak))
        self.assertGreater(peak, -7.0)
        self.assertLess(peak, -5.0)


if __name__ == "__main__":
    unittest.main()
