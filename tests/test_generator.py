import filecmp
import os

from syncbench.generator import ChannelSpec, SampleSpec, generate_sample

from .base import SampleSuiteTestCase


class TestGenerator(SampleSuiteTestCase):
    def test_deterministic_regeneration(self):
        spec = SampleSpec(
            name="det",
            seed=99,
            n_channels=3,
            duration_s=8.0,
            channels=[
                ChannelSpec(),
                ChannelSpec(skew_ppm=15.0, missing_markers=[3]),
                ChannelSpec(jumps=[{"marker_index": 4, "step_samples": 10.0}]),
            ],
        )
        dir_a = os.path.join(self.tmp, "det_a")
        dir_b = os.path.join(self.tmp, "det_b")
        paths_a, truth_a = generate_sample(dir_a, spec)
        paths_b, truth_b = generate_sample(dir_b, spec)
        for pa, pb in zip(paths_a, paths_b):
            self.assertTrue(filecmp.cmp(pa, pb, shallow=False), pa)
        with open(truth_a, "rb") as fa, open(truth_b, "rb") as fb:
            self.assertEqual(fa.read(), fb.read())

    def test_suite_shapes_and_truth(self):
        truth = self.load_truth("skew4")
        self.assertEqual(truth["n_channels"], 4)
        self.assertEqual(truth["fs"], 48000)
        self.assertEqual(truth["truth_source"], "generator_truth")
        self.assertAlmostEqual(truth["channels"][1]["skew_ppm"], 40.0)
        markers = truth["channels"][1]["markers"]
        self.assertGreater(len(markers), 15)
        drift = markers[-1]["sample"] - markers[0]["sample"]
        expect = (markers[-1]["index"] - markers[0]["index"]) * 48000 * (1 + 40e-6)
        self.assertAlmostEqual(drift, expect, places=3)

    def test_missing_and_low_snr_recorded(self):
        truth = self.load_truth("degraded4")
        ch1 = truth["channels"][1]
        present = {m["index"] for m in ch1["markers"]}
        for idx in (5, 6, 7):
            self.assertNotIn(idx, present)
        self.assertEqual(ch1["missing_markers"], [5, 6, 7])
