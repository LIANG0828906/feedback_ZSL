import json
import os
import unittest

import numpy as np

from opera_master.cue import load_cue, validate_wavs
from opera_master.errors import (
    BED_CHANNEL_MISSING,
    BIT_DEPTH_MISMATCH,
    CHANNEL_COUNT_MISMATCH,
    CueValidationError,
    DUPLICATE_PART_NAME,
    FILE_MISSING,
    REF_TIMES_NOT_ASCENDING,
    SAMPLE_RATE_MISMATCH,
)
from opera_master.wavio import write_wav

from .helpers import make_workspace, write_cue


class ValidationTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cue_path, self.cue = make_workspace()

    def _reload(self):
        return load_cue(self.cue_path)

    def test_golden_passes_validation(self):
        validate_wavs(self._reload(), base_dir=self.tmp)

    def test_missing_file_reports_file_missing(self):
        os.remove(os.path.join(self.tmp, self.cue.channel("大锣").path))
        with self.assertRaises(CueValidationError) as ctx:
            validate_wavs(self._reload(), base_dir=self.tmp)
        codes = {(e["code"], e.get("channel")) for e in ctx.exception.errors}
        self.assertIn((FILE_MISSING, "大锣"), codes)

    def test_sample_rate_mismatch(self):
        path = os.path.join(self.tmp, self.cue.channel("小锣").path)
        write_wav(path, np.zeros(1000), 44100, self.cue.bit_depth)
        with self.assertRaises(CueValidationError) as ctx:
            validate_wavs(self._reload(), base_dir=self.tmp)
        self.assertTrue(
            any(e["code"] == SAMPLE_RATE_MISMATCH for e in ctx.exception.errors)
        )

    def test_bit_depth_mismatch(self):
        path = os.path.join(self.tmp, self.cue.channel("月琴").path)
        write_wav(path, np.zeros(1000), self.cue.sample_rate, 24)
        with self.assertRaises(CueValidationError) as ctx:
            validate_wavs(self._reload(), base_dir=self.tmp)
        self.assertTrue(
            any(e["code"] == BIT_DEPTH_MISMATCH for e in ctx.exception.errors)
        )

    def test_channel_count_mismatch(self):
        path = os.path.join(self.tmp, self.cue.channel("板鼓").path)
        stereo = np.zeros((1000, 2))
        write_wav(path, stereo, self.cue.sample_rate, self.cue.bit_depth)
        with self.assertRaises(CueValidationError) as ctx:
            validate_wavs(self._reload(), base_dir=self.tmp)
        self.assertTrue(
            any(
                e["code"] == CHANNEL_COUNT_MISMATCH
                for e in ctx.exception.errors
            )
        )

    def test_duplicate_part_name(self):
        with open(self.cue_path, encoding="utf-8") as _fh:

            raw = json.load(_fh)
        raw["channels"][1]["part"] = raw["channels"][0]["part"]
        write_cue(self.cue_path, raw)
        with self.assertRaises(CueValidationError) as ctx:
            load_cue(self.cue_path)
        self.assertTrue(
            any(e["code"] == DUPLICATE_PART_NAME for e in ctx.exception.errors)
        )

    def test_reference_times_must_be_ascending(self):
        with open(self.cue_path, encoding="utf-8") as _fh:

            raw = json.load(_fh)
        raw["reference_times"] = [1.0, 0.5, 2.0]
        write_cue(self.cue_path, raw)
        with self.assertRaises(CueValidationError) as ctx:
            load_cue(self.cue_path)
        self.assertTrue(
            any(
                e["code"] == REF_TIMES_NOT_ASCENDING
                for e in ctx.exception.errors
            )
        )

    def test_bed_channel_missing(self):
        with open(self.cue_path, encoding="utf-8") as _fh:

            raw = json.load(_fh)
        raw["channels"] = [
            c for c in raw["channels"] if c["part"] != "底噪参考"
        ]
        write_cue(self.cue_path, raw)
        with self.assertRaises(CueValidationError) as ctx:
            load_cue(self.cue_path)
        self.assertTrue(
            any(e["code"] == BED_CHANNEL_MISSING for e in ctx.exception.errors)
        )


if __name__ == "__main__":
    unittest.main()
