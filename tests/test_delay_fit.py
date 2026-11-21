import glob
import os

from syncbench.delay import estimate_segment_delays
from syncbench.diagnose import CLOCK_SKEW, EVENT_JUMP, diagnose
from syncbench.errors import E_DRIFT_UNKNOWN
from syncbench.fit import fit_drift
from syncbench.markers import detect_markers
from syncbench.wavio import read_channel_wavs

from .base import SampleSuiteTestCase


class TestDelayFit(SampleSuiteTestCase):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls._ref_pos_cache = {}

    def _signals(self, dataset):
        paths = sorted(glob.glob(os.path.join(self.dataset_dir(dataset), "*.wav")))
        signals, fs = read_channel_wavs(paths)
        return signals, fs

    def _ref_positions(self, dataset):
        if dataset not in self._ref_pos_cache:
            signals, fs = self._signals(dataset)
            det = detect_markers(signals[0], fs, 1.0)
            self._ref_pos_cache[dataset] = [m["sample"] for m in det["markers"]]
        return self._ref_pos_cache[dataset]

    def _segments(self, dataset, ch, seg_len_s=2.0):
        signals, fs = self._signals(dataset)
        return estimate_segment_delays(
            signals[0], signals[ch], int(seg_len_s * fs), 2400,
            ref_markers=self._ref_positions(dataset),
        )

    def _fit_channel(self, dataset, ch, seg_len_s=2.0):
        segs = self._segments(dataset, ch, seg_len_s)
        valid = [s for s in segs if s["valid"]]
        fit = fit_drift([s["center"] for s in valid], [s["delay"] for s in valid])
        return segs, fit

    def _diagnose(self, dataset, ch, seg_len_s=2.0):
        segs = self._segments(dataset, ch, seg_len_s)
        valid = [s for s in segs if s["valid"]]
        return diagnose(
            [s["center"] for s in valid],
            [s["delay"] for s in valid],
            2.0,
            indices=[s["segment"] for s in valid],
        )

    def test_skew_fit_matches_truth(self):
        truth = self.load_truth("skew4")
        for ch in (1, 2, 3):
            _, fit = self._fit_channel("skew4", ch)
            self.assertAlmostEqual(
                fit["ppm"], truth["channels"][ch]["skew_ppm"], delta=2.0
            )
            self.assertAlmostEqual(
                fit["offset"], truth["channels"][ch]["offset_samples"], delta=1.0
            )
            self.assertLess(fit["residual_rms"], 1.0)

    def test_segment_length_change_reestimates(self):
        _, fit_a = self._fit_channel("skew4", 1, seg_len_s=2.0)
        _, fit_b = self._fit_channel("skew4", 1, seg_len_s=4.0)
        self.assertAlmostEqual(fit_a["ppm"], fit_b["ppm"], delta=2.0)
        self.assertAlmostEqual(fit_a["offset"], fit_b["offset"], delta=1.0)

    def test_diagnose_skew(self):
        diag = self._diagnose("skew4", 3)
        self.assertEqual(diag["conclusion"], CLOCK_SKEW)
        self.assertAlmostEqual(diag["ppm"], 60.0, delta=2.0)
        self.assertGreater(diag["ppm_ci"], 0.0)

    def test_diagnose_jump(self):
        diag = self._diagnose("jump4", 2)
        self.assertEqual(diag["conclusion"], EVENT_JUMP)
        self.assertEqual(diag["jump_segment"], 6)
        self.assertAlmostEqual(diag["jump_size"], 40.0, delta=2.0)
        self.assertEqual(diag["evidence_segments"], [5, 6])

    def test_diagnose_unknown(self):
        diag = self._diagnose("unknown4", 1)
        self.assertEqual(diag["conclusion"], E_DRIFT_UNKNOWN)
        self.assertTrue(diag["candidate_hypotheses"])
        self.assertIn("residual_rms", diag["residual_stats"])
