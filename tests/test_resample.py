import glob
import os

import numpy as np

from syncbench.markers import detect_markers
from syncbench.resample import resample
from syncbench.wavio import read_channel_wavs

from .base import SampleSuiteTestCase


class TestResample(SampleSuiteTestCase):
    def _alignment_error(self, mode):
        paths = sorted(glob.glob(os.path.join(self.dataset_dir("skew4"), "*.wav")))
        signals, fs = read_channel_wavs(paths)
        truth = self.load_truth("skew4")
        ref_markers = {m["index"]: m["sample"] for m in truth["channels"][0]["markers"]}
        errors = []
        for ch in (1, 2, 3):
            spec = truth["channels"][ch]
            offset = spec["offset_samples"]
            slope = spec["skew_ppm"] * 1e-6
            comp = resample(signals[ch], offset, slope, mode)
            det = detect_markers(comp, fs, 1.0)["markers"]
            for m in det:
                if m["index"] in ref_markers:
                    errors.append(abs(m["sample"] - ref_markers[m["index"]]))
        return max(errors), float(np.mean(errors))

    def test_both_modes_within_tolerance(self):
        for mode in ("linear", "sinc"):
            max_err, mean_err = self._alignment_error(mode)
            self.assertLess(max_err, 1.0, f"{mode} max error {max_err}")
            self.assertLess(mean_err, 0.5, f"{mode} mean error {mean_err}")

    def test_mode_comparison_sinc_not_worse(self):
        max_lin, mean_lin = self._alignment_error("linear")
        max_sinc, mean_sinc = self._alignment_error("sinc")
        # both modes are accurate; the comparison must be reported and close
        self.assertLess(max_lin, 0.5)
        self.assertLess(max_sinc, 0.5)
        self.assertLess(abs(mean_sinc - mean_lin), 0.2)

    def test_deterministic_output(self):
        paths = sorted(glob.glob(os.path.join(self.dataset_dir("skew4"), "*.wav")))
        signals, _ = read_channel_wavs(paths)
        a = resample(signals[1], 3.0, 40e-6, "sinc")
        b = resample(signals[1], 3.0, 40e-6, "sinc")
        np.testing.assert_array_equal(a, b)
