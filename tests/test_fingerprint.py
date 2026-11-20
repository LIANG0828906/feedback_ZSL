from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT  # noqa: F401

from fpstore.audio import generate_single_sample, read_wav
from fpstore.config import FingerprintConfig
from fpstore.fingerprint import compare_extractions, extract_fingerprint
from fpstore.util import canonical_bytes


class FingerprintTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = Path(tempfile.mkdtemp(prefix="fpstore-fp-"))
        cls.wav = generate_single_sample(
            cls.temp / "tone.wav",
            seed=21,
            sample_rate=16000,
            duration_s=4.0,
            stereo=True,
        )
        cls.audio = read_wav(cls.wav)

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.temp, ignore_errors=True)

    def _snapshot(self, config: FingerprintConfig) -> bytes:
        fp = extract_fingerprint(self.audio, config, track_id="tone")
        return canonical_bytes(
            [
                {
                    "segment_index": segment.segment_index,
                    "segment_hash": segment.segment_hash,
                    "pairs": [pair.as_dict() for pair in segment.peak_pairs],
                    "hashes": [hit.as_dict() for hit in segment.hashes],
                }
                for segment in fp.segments
            ]
        )

    def test_extraction_is_byte_deterministic(self) -> None:
        config = FingerprintConfig(version=3, time_quant_bits=0)
        self.assertEqual(self._snapshot(config), self._snapshot(config))

    def test_quantization_change_recomputes_offsets_and_reports_max(self) -> None:
        report = compare_extractions(
            self.audio,
            FingerprintConfig(version=3, time_quant_bits=0),
            FingerprintConfig(version=3, time_quant_bits=5),
        )
        self.assertTrue(report["affected"])
        self.assertGreater(report["hash_changed_pairs"], 0)
        self.assertGreater(report["max_abs_offset_ms"], 0)
        self.assertGreater(len(report["details"]), 0)

    def test_band_change_recomputes_hashes(self) -> None:
        report = compare_extractions(
            self.audio,
            FingerprintConfig(version=3, time_quant_bits=0, band_edges_hz=(0, 500, 2000, 4000, 8000)),
            FingerprintConfig(version=3, time_quant_bits=0, band_edges_hz=(0, 1000, 3000, 6000)),
        )
        self.assertTrue(report["affected"])
        self.assertGreater(report["hash_changed_pairs"], 0)

    def test_channel_merge_change_affects_stereo_track(self) -> None:
        report = compare_extractions(
            self.audio,
            FingerprintConfig(version=3, time_quant_bits=0, channel_merge="mean"),
            FingerprintConfig(version=3, time_quant_bits=0, channel_merge="left"),
        )
        self.assertTrue(report["affected"])
        self.assertGreater(report["unmatched_old_pairs"] + report["unmatched_new_pairs"], 0)


if __name__ == "__main__":
    unittest.main()
