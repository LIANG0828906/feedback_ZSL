import unittest

from afpipe.config import Config
from afpipe.errors import ParameterError
from afpipe.fingerprint import (
    frame_match_rate,
    hamming_match_rate,
    quantize_delta,
)
from afpipe.pipeline import process_file

from . import MANIFEST


class FingerprintTest(unittest.TestCase):
    def test_every_frame_has_time_offset(self):
        media = MANIFEST.file("gA_long_wav")
        processed = process_file(media, MANIFEST, Config())
        times = [f.time_s for f in processed.track.frames]
        self.assertEqual(times, sorted(times))
        self.assertGreater(len(times), 10)

    def test_nested_quantisation_prefix_property(self):
        # Nested codes share the leading sign bit at every level.
        for delta in (-2.0, -0.1, 0.4, 3.0):
            sign = quantize_delta(delta, 1.0, 1)
            self.assertEqual(sign, 1 if delta >= 0 else 0)
            code2 = quantize_delta(delta, 1.0, 2)
            self.assertEqual(code2 >> 1, sign, delta)
            code4 = quantize_delta(delta, 1.0, 4)
            self.assertEqual(code4 >> 2, sign, delta)
            # Same sign hemisphere at every level.
            self.assertEqual((code2 >> 1) == 1, delta >= 0)
            self.assertEqual((code4 >> 2) == 1, delta >= 0)

    def test_match_rate_meets_floor_and_monotonic(self):
        pcm = MANIFEST.file("gA_long_wav")
        mp3 = MANIFEST.file("gA_long_mp3")
        rates = {}
        for bits in (1, 2, 4):
            cfg = Config(quant_bits=bits)
            a = process_file(pcm, MANIFEST, cfg)
            b = process_file(mp3, MANIFEST, cfg)
            rates[bits] = hamming_match_rate(a.track, b.track, cfg)
        self.assertGreaterEqual(rates[2], Config().match_rate_floor)
        self.assertGreaterEqual(rates[1], rates[2])
        self.assertGreaterEqual(rates[2], rates[4])

    def test_exact_match_is_non_increasing(self):
        pcm = MANIFEST.file("gB_long_wav")
        mp3 = MANIFEST.file("gB_long_mp3")
        exact = []
        for bits in (1, 2, 4):
            cfg = Config(quant_bits=bits)
            a = process_file(pcm, MANIFEST, cfg)
            b = process_file(mp3, MANIFEST, cfg)
            exact.append(frame_match_rate(a.track, b.track))
        self.assertGreaterEqual(exact[0], exact[1])
        self.assertGreaterEqual(exact[1], exact[2])

    def test_invalid_parameters_raise_E_PARA(self):
        with self.assertRaises(ParameterError):
            Config(quant_bits=3).validate()
        with self.assertRaises(ParameterError):
            Config(bands=999, frame_ms=4.0).validate()
        with self.assertRaises(ParameterError):
            Config(max_workers=0).validate()


if __name__ == "__main__":
    unittest.main()
