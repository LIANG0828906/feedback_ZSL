from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from drum_bus.synth import (
    DemoConfig,
    generate_demo_project,
    reflection_parameters,
)
from drum_bus.wavio import read_wav


class SynthesisTests(unittest.TestCase):
    def test_generation_is_byte_deterministic(self) -> None:
        config = DemoConfig(
            duration_seconds=1.4,
            reference_times=(0.2, 0.55, 0.9, 1.2),
            block_size=512,
        )
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second:
            generate_demo_project(first, config)
            generate_demo_project(second, config)
            for name in ("board_drum.wav", "bangzi.wav", "tanggu.wav", "ambient.wav"):
                self.assertEqual(
                    (Path(first) / name).read_bytes(),
                    (Path(second) / name).read_bytes(),
                )

    def test_manifest_reflection_values_are_recomputable(self) -> None:
        config = DemoConfig(
            duration_seconds=1.2,
            reference_times=(0.2, 0.5, 0.9),
            block_size=512,
        )
        expected = reflection_parameters(config)
        self.assertEqual(expected["delays_samples"], [456, 864])
        self.assertAlmostEqual(expected["gains_linear"][0], 10.0 ** (-11.5 / 20.0))

    def test_generated_wavs_are_24_bit_and_shifted(self) -> None:
        config = DemoConfig(
            duration_seconds=1.2,
            reference_times=(0.2, 0.5, 0.9),
            block_size=512,
        )
        with tempfile.TemporaryDirectory() as directory:
            generate_demo_project(directory, config)
            for name in ("board_drum.wav", "bangzi.wav", "tanggu.wav", "ambient.wav"):
                wav = read_wav(Path(directory) / name)
                self.assertEqual(wav.info.sample_rate, config.sample_rate)
                self.assertEqual(wav.info.bit_depth, 24)
                self.assertEqual(wav.info.channels, 1)
                self.assertEqual(
                    wav.info.frames,
                    int(round(config.duration_seconds * config.sample_rate)),
                )


if __name__ == "__main__":
    unittest.main()

