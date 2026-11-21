import glob
import os

from syncbench.markers import detect_markers
from syncbench.wavio import read_channel_wavs

from .base import SampleSuiteTestCase


class TestMarkers(SampleSuiteTestCase):
    def _channel(self, dataset, ch):
        paths = sorted(glob.glob(os.path.join(self.dataset_dir(dataset), "*.wav")))
        signals, fs = read_channel_wavs(paths)
        return signals[ch], fs

    def test_detection_matches_truth(self):
        truth = self.load_truth("skew4")
        for ch in (0, 1, 3):
            sig, fs = self._channel("skew4", ch)
            det = detect_markers(sig, fs, 1.0)
            got = {m["index"]: m["sample"] for m in det["markers"]}
            for rec in truth["channels"][ch]["markers"]:
                self.assertIn(rec["index"], got)
                self.assertAlmostEqual(got[rec["index"]], rec["sample"], delta=1.0)
                conf = [
                    m["confidence"]
                    for m in det["markers"]
                    if m["index"] == rec["index"]
                ][0]
                self.assertGreater(conf, 0.5)

    def test_unavailable_marked_not_interpolated(self):
        sig, fs = self._channel("degraded4", 1)
        det = detect_markers(sig, fs, 1.0)
        missing = {u["index"] for u in det["unavailable"]}
        for idx in (5, 6, 7):
            self.assertIn(idx, missing)
        for u in det["unavailable"]:
            self.assertEqual(u["truth_source"], "generator_truth")
        present = {m["index"] for m in det["markers"]}
        self.assertTrue(present.isdisjoint({5, 6, 7}))

    def test_low_snr_segment_unavailable(self):
        sig, fs = self._channel("degraded4", 2)
        det = detect_markers(sig, fs, 1.0)
        missing = {u["index"] for u in det["unavailable"]}
        for idx in (10, 12, 14):
            self.assertIn(idx, missing)
