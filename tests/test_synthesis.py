import os
import unittest

import numpy as np

from opera_master import PART_BANGU, PART_BANGQIANG, PART_BED
from opera_master.synthesis import (
    apply_crosstalk,
    build_clean_sources,
    generate_channels,
)
from opera_master.wavio import read_wav

from .helpers import make_workspace


class SynthesisTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cue_path, self.cue = make_workspace(shared=True)

    def test_deterministic_byte_identical_generation(self):
        first = generate_channels(self.cue, base_dir=self.tmp)
        hashes_first = {
            ch.part: self._hash(ch.path) for ch in self.cue.channels
        }
        second = generate_channels(self.cue, base_dir=self.tmp)
        for part in first:
            np.testing.assert_array_equal(first[part], second[part])
        for ch in self.cue.channels:
            self.assertEqual(hashes_first[ch.part], self._hash(ch.path))

    def _hash(self, rel):
        import hashlib

        with open(os.path.join(self.tmp, rel), "rb") as fh:
            return hashlib.sha256(fh.read()).hexdigest()

    def test_bed_is_noise_only(self):
        signals = build_clean_sources(self.cue)
        bed = signals[PART_BED][: 8192]
        # FFT autocorrelation: white noise has no significant off-zero peak.
        spec = np.fft.rfft(bed - bed.mean())
        corr = np.fft.irfft(spec * np.conj(spec))[: 4096]
        normalized = corr / corr[0]
        self.assertLess(np.max(np.abs(normalized[10:])), 0.15)

    def test_crosstalk_injection_reproducible_from_cue(self):
        clean = build_clean_sources(self.cue)
        injected = {k: v.copy() for k, v in clean.items()}
        apply_crosstalk(injected, self.cue)
        # 大锣 -> 帮腔
        ch = self.cue.channel(PART_BANGQIANG)
        gain = 10.0 ** (ch.crosstalk_gain_db / 20.0)
        sr = self.cue.sample_rate
        ws = int(round(ch.crosstalk_window[0] * sr))
        we = int(round(ch.crosstalk_window[1] * sr))
        delta = injected[PART_BANGQIANG] - clean[PART_BANGQIANG]
        # Away from the 5 ms edge fades the injection equals gain*source.
        interior = slice(ws + int(0.01 * sr), we - int(0.01 * sr))
        expected = gain * clean[ch.crosstalk_source][interior]
        np.testing.assert_allclose(delta[interior], expected, rtol=1e-8)

    def test_no_crosstalk_outside_declared_window(self):
        clean = build_clean_sources(self.cue)
        injected = {k: v.copy() for k, v in clean.items()}
        apply_crosstalk(injected, self.cue)
        ch = self.cue.channel(PART_BANGQIANG)
        sr = self.cue.sample_rate
        we = int(round(ch.crosstalk_window[1] * sr))
        if we < self.cue.total_samples:
            delta = injected[PART_BANGQIANG] - clean[PART_BANGQIANG]
            np.testing.assert_array_equal(delta[we + 10 :], 0.0)

    def test_wav_roundtrip_matches_contract(self):
        generate_channels(self.cue, base_dir=self.tmp)
        for ch in self.cue.channels:
            samples, sr, bits, channels = read_wav(
                os.path.join(self.tmp, ch.path)
            )
            self.assertEqual(sr, self.cue.sample_rate)
            self.assertEqual(bits, self.cue.bit_depth)
            self.assertEqual(channels, 1)
            self.assertEqual(samples.shape[0], self.cue.total_samples)


if __name__ == "__main__":
    unittest.main()
