"""Structured validation failures with stable business codes/exit codes."""

from __future__ import annotations

import copy
import json
import os
import tempfile
import unittest

from stage_noise.errors import EXIT_CODES, StageNoiseError  # noqa: E402
from stage_noise.generator import generate, verify_wav_headers  # noqa: E402
from stage_noise.manifest import load_manifest  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEMO = os.path.join(ROOT, "configs", "demo_manifest.json")


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.base = tempfile.mkdtemp(prefix="sn_test_")
        with open(DEMO, encoding="utf-8") as handle:
            self.raw = json.load(handle)

    def tearDown(self):
        import shutil
        shutil.rmtree(self.base, ignore_errors=True)

    def _write(self, raw, name="manifest.json"):
        path = os.path.join(self.base, name)
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(raw, handle)
        return path

    def assert_code(self, raw, code, *, generated=False, mutate_wav=None):
        path = self._write(raw)
        if generated:
            manifest = load_manifest(path)
            generate(manifest)
            if mutate_wav:
                mutate_wav(manifest)
            with self.assertRaises(StageNoiseError) as caught:
                verify_wav_headers(manifest)
        else:
            with self.assertRaises(StageNoiseError) as caught:
                load_manifest(path)
            self.assertEqual(caught.exception.code, code)
        if generated:
            self.assertEqual(caught.exception.mic, manifest.mics[0].name)
        self.assertEqual(EXIT_CODES[code],
                         caught.exception.to_payload()["exit_code"])

    def test_duplicate_mic_name(self):
        raw = copy.deepcopy(self.raw)
        raw["microphones"][1]["name"] = raw["microphones"][0]["name"]
        self.assert_code(raw, "DUPLICATE_MIC_NAME")

    def test_action_segment_not_ordered(self):
        raw = copy.deepcopy(self.raw)
        raw["action_segments"][1]["start_time"] = 0.5
        self.assert_code(raw, "INVALID_ACTION_SEGMENT")

    def test_action_segment_reversed(self):
        raw = copy.deepcopy(self.raw)
        raw["action_segments"][0]["end_time"] = 1.0
        self.assert_code(raw, "INVALID_ACTION_SEGMENT")

    def test_invalid_frequency_band(self):
        raw = copy.deepcopy(self.raw)
        raw["noise_reference_band_hz"] = [200.0, 40.0]
        self.assert_code(raw, "INVALID_FREQUENCY_BAND")

    def test_band_above_nyquist(self):
        raw = copy.deepcopy(self.raw)
        raw["noise_reference_band_hz"] = [100.0, 30000.0]
        self.assert_code(raw, "INVALID_FREQUENCY_BAND")

    def test_sample_rate_mismatch(self):
        raw = copy.deepcopy(self.raw)
        raw["microphones"][0]["sample_rate"] = 44100
        self.assert_code(raw, "SAMPLE_RATE_MISMATCH")

    def test_channel_count_mismatch(self):
        raw = copy.deepcopy(self.raw)
        raw["microphones"][0]["channels"] = 2
        self.assert_code(raw, "CHANNEL_COUNT_MISMATCH")

    def test_bit_depth_mismatch(self):
        raw = copy.deepcopy(self.raw)
        raw["microphones"][0]["bit_depth"] = 24
        self.assert_code(raw, "BIT_DEPTH_MISMATCH")

    def test_missing_wav_file(self):
        raw = copy.deepcopy(self.raw)
        path = self._write(raw)
        manifest = load_manifest(path)
        with self.assertRaises(StageNoiseError) as caught:
            verify_wav_headers(manifest)
        self.assertEqual(caught.exception.code, "FILE_MISSING")
        self.assertEqual(caught.exception.mic, manifest.mics[0].name)

    def test_actual_wav_rate_mismatch(self):
        import soundfile as sf

        def rewrite(manifest):
            mic_path = os.path.join(manifest.source_dir,
                                    manifest.mics[0].path)
            data, _ = sf.read(mic_path, dtype="float64")
            sf.write(mic_path, data, 44100, subtype="PCM_16")

        self.assert_code(copy.deepcopy(self.raw), "SAMPLE_RATE_MISMATCH",
                         generated=True, mutate_wav=rewrite)


if __name__ == "__main__":
    unittest.main(verbosity=2)
