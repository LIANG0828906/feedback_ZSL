import unittest

import numpy as np

from opera_master import PERCUSSION_PARTS
from opera_master.align import apply_offsets, compute_offsets, detect_onsets
from opera_master.errors import DETECTION_FAILED
from opera_master.synthesis import apply_crosstalk, build_clean_sources

from .helpers import make_workspace


class AlignmentTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cue_path, self.cue = make_workspace(shared=True)
        self.signals = build_clean_sources(self.cue)
        apply_crosstalk(self.signals, self.cue)

    def test_every_percussion_onset_within_one_sample(self):
        offsets, warnings, detail, micro = compute_offsets(self.signals, self.cue)
        self.assertEqual(warnings, [])
        for entry in detail:
            self.assertLessEqual(entry["max_abs_residual"], 1)

    def test_offsets_are_integer_samples(self):
        offsets, _, _, _ = compute_offsets(self.signals, self.cue)
        for value in offsets.values():
            self.assertIsInstance(value, int)

    def test_detection_failure_keeps_offset_and_warns(self):
        disabled = (PERCUSSION_PARTS[0],)
        offsets, warnings, detail, micro = compute_offsets(
            self.signals, self.cue, disabled_parts=disabled
        )
        self.assertTrue(
            any(w["code"] == DETECTION_FAILED for w in warnings)
        )
        self.assertEqual(offsets[PERCUSSION_PARTS[0]], 0)

    def test_apply_offsets_preserves_length_and_zeros_pad(self):
        offsets, _, _, micro = compute_offsets(self.signals, self.cue)
        aligned = apply_offsets(self.signals, offsets, micro_shifts=micro)
        n = max(x.shape[0] for x in self.signals.values())
        for part, y in aligned.items():
            self.assertEqual(y.shape[0], n)
            self.assertEqual(y.dtype, self.signals[part].dtype)

    def test_detect_onsets_returns_one_per_reference(self):
        ref = np.array(self.cue.ref_samples(), dtype=np.int64)
        for part in PERCUSSION_PARTS:
            onsets, ok = detect_onsets(
                part, self.signals[part], self.cue.sample_rate, ref
            )
            self.assertEqual(onsets.shape[0], ref.shape[0])
            self.assertTrue(ok)


if __name__ == "__main__":
    unittest.main()
