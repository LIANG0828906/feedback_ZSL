from __future__ import annotations

import struct
import tempfile
import unittest
from pathlib import Path

import numpy as np

from loudness_diag.dsp import true_peak_4x, weighted_downmix
from loudness_diag.errors import E_HEADER_INVALID, DiagnosticError
from loudness_diag.generator import generate_test_set
from loudness_diag.wav import read_wav, read_wav_bytes


class WavTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name)
        cls.manifest = generate_test_set(cls.root, seed=77, seconds=0.4)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_reads_16_and_24_bit_with_channel_evidence(self) -> None:
        wav16 = read_wav(self.root / "program_51_16bit.wav")
        wav24 = read_wav(self.root / "program_51_24bit.wav")
        self.assertEqual(wav16.info.bit_depth, 16)
        self.assertEqual(wav24.info.bit_depth, 24)
        self.assertEqual(wav16.info.channels, 6)
        self.assertEqual(wav16.info.channel_mask, 0x3F)
        self.assertEqual(wav16.frame_count, 19200)
        self.assertEqual(wav24.frame_count, wav16.frame_count)
        self.assertEqual(len(wav16.channel_statistics), 6)
        for channel in wav16.channel_statistics:
            self.assertEqual(channel["sample_count"], wav16.frame_count)
            self.assertGreaterEqual(channel["peak"], 0.0)
            self.assertEqual(len(channel["content_hash"]), 64)
        self.assertEqual(wav16.samples.shape, (19200, 6))
        self.assertAlmostEqual(
            float(np.max(np.abs(wav16.samples))),
            self.manifest["assets"][0]["sample_peak"],
            places=12,
        )
        self.assertNotEqual(wav16.content_hash, wav24.content_hash)
        self.assertEqual(weighted_downmix(wav16.samples).shape, (19200,))

        source = (self.root / "program_51_16bit.wav").read_bytes()
        fmt_data = source.find(b"fmt ") + 8
        side_layout = self._mutate(source, fmt_data + 20, struct.pack("<I", 0x60F))
        wav_side = read_wav_bytes(side_layout)
        self.assertEqual(
            wav_side.info.channel_names,
            ("FL", "FR", "FC", "LFE", "SL", "SR"),
        )

        repeat_root = self.root / "repeat"
        generate_test_set(repeat_root, seed=77, seconds=0.4)
        self.assertEqual(
            (self.root / "manifest.json").read_bytes(),
            (repeat_root / "manifest.json").read_bytes(),
        )
        self.assertEqual(
            (self.root / "program_51_24bit.wav").read_bytes(),
            (repeat_root / "program_51_24bit.wav").read_bytes(),
        )

    def test_lfe_is_excluded_from_weighted_downmix(self) -> None:
        samples = np.zeros((8, 6), dtype=np.float64)
        samples[:, 3] = 0.9
        self.assertTrue(np.allclose(weighted_downmix(samples), 0.0))
        samples[:, 4] = 0.5
        self.assertGreater(float(np.max(np.abs(weighted_downmix(samples)))), 0.5)

    def test_four_times_true_peak_is_bounded_and_captures_intersample_peak(self) -> None:
        constant = np.ones(200, dtype=np.float64)
        self.assertAlmostEqual(true_peak_4x(constant), 1.0, places=6)
        time = np.arange(4800, dtype=np.float64) / 48000.0
        sine = 0.8 * np.sin(2.0 * np.pi * 997.0 * time + 0.37)
        sample_peak = float(np.max(np.abs(sine)))
        oversampled_peak = true_peak_4x(sine)
        self.assertGreaterEqual(oversampled_peak + 1e-12, sample_peak)
        self.assertLess(oversampled_peak, 0.8001)

    def _mutate(self, target: bytes, offset: int, payload: bytes) -> bytes:
        value = bytearray(target)
        value[offset : offset + len(payload)] = payload
        return bytes(value)

    def test_header_contradictions_return_business_code(self) -> None:
        path = self.root / "program_51_16bit.wav"
        original = path.read_bytes()
        fmt_chunk = original.find(b"fmt ")
        self.assertGreaterEqual(fmt_chunk, 0)
        fmt_data = fmt_chunk + 8

        bad_rate = self._mutate(original, fmt_data + 4, struct.pack("<I", 44100))
        with self.assertRaises(DiagnosticError) as context:
            read_wav_bytes(bad_rate)
        self.assertEqual(context.exception.code, E_HEADER_INVALID)

        bad_mask = self._mutate(original, fmt_data + 20, struct.pack("<I", 0x3C))
        with self.assertRaises(DiagnosticError) as context:
            read_wav_bytes(bad_mask)
        self.assertEqual(context.exception.code, E_HEADER_INVALID)

        bad_depth = self._mutate(original, fmt_data + 14, struct.pack("<H", 20))
        with self.assertRaises(DiagnosticError) as context:
            read_wav_bytes(bad_depth)
        self.assertEqual(context.exception.code, E_HEADER_INVALID)

        with self.assertRaises(DiagnosticError) as context:
            read_wav_bytes(original + b"\0")
        self.assertEqual(context.exception.code, E_HEADER_INVALID)


if __name__ == "__main__":
    unittest.main()

