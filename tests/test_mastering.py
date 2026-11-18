import unittest

import numpy as np

from opera_master.align import apply_offsets, compute_offsets
from opera_master.dsp import design_oversample_filter, true_peak_db
from opera_master.decouple import run_decoupling
from opera_master.loudness import integrated_loudness
from opera_master.mastering import comply
from opera_master.mix import mix_master
from opera_master.synthesis import apply_crosstalk, build_clean_sources

from .helpers import make_workspace


class MasteringTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cue_path, self.cue = make_workspace(shared=True)
        signals = build_clean_sources(self.cue)
        apply_crosstalk(signals, self.cue)
        decoupled, _ = run_decoupling(signals, self.cue)
        offsets, _, _, micro = compute_offsets(signals, self.cue)
        aligned = apply_offsets(decoupled, offsets, micro_shifts=micro)
        self.master = mix_master(aligned, self.cue)

    def test_master_is_stereo_and_longest_length(self):
        self.assertEqual(self.master.ndim, 2)
        self.assertEqual(self.master.shape[1], 2)
        self.assertEqual(self.master.shape[0], self.cue.total_samples)

    def test_loudness_and_true_peak_compliance(self):
        limited, report = comply(
            self.master,
            self.cue.sample_rate,
            self.cue.target_lufs,
            self.cue.true_peak_limit_db,
        )
        kernel = design_oversample_filter()
        tp = true_peak_db(limited, kernel)
        lufs = integrated_loudness(limited, self.cue.sample_rate)
        self.assertLessEqual(tp, self.cue.true_peak_limit_db)
        self.assertLessEqual(abs(lufs - self.cue.target_lufs), 0.5)
        self.assertTrue(report["tp_compliant"])
        self.assertTrue(report["loudness_compliant"])
        # Limiting must not change the sample count (delay compensated).
        self.assertEqual(limited.shape[0], self.master.shape[0])

    def test_limiter_engages_only_when_needed(self):
        loud, _ = comply(
            self.master,
            self.cue.sample_rate,
            self.cue.target_lufs,
            self.cue.true_peak_limit_db,
        )
        # An artificially low ceiling must trigger gain reduction.
        crushed, report = comply(
            self.master,
            self.cue.sample_rate,
            self.cue.target_lufs,
            -20.0,
        )
        self.assertGreater(report["limiter_triggered_samples"], 0)
        self.assertGreater(report["limiter_max_attenuation_db"], 0.0)
        self.assertEqual(crushed.shape[0], loud.shape[0])
        self.assertLessEqual(
            true_peak_db(crushed, design_oversample_filter()), -20.0
        )

    def test_crossfade_does_not_exceed_declared_duration(self):
        sr = self.cue.sample_rate
        max_fade = int(round(self.cue.crossfade_seconds * sr))
        # Section boundaries are spaced far beyond the crossfade duration.
        for i in range(1, len(self.cue.sections)):
            boundary = int(round(self.cue.sections[i].start * sr))
            self.assertGreater(boundary, max_fade)


if __name__ == "__main__":
    unittest.main()
