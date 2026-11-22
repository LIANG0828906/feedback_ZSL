import os
import unittest

from forensics.errors import ForensicsError, E_DIVERGE
from forensics.loader import load_dataset, load_sample
from forensics.params import AnalysisParams
from forensics.engine import dual_path_analysis
from forensics.engine import (
    downscale_single_block,
    PREVIEW_CONTRAST_GATE,
)
from forensics.report import confusion_against_truth, LatencyTracker
from forensics.analysis import analyze_block


SAMPLE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
)


class TestDualPathConsistency(unittest.TestCase):
    def test_all_samples_region_and_metrics_consistent(self):
        samples = load_dataset(SAMPLE_DIR, block_size=32)
        self.assertGreaterEqual(len(samples), 5)
        params = AnalysisParams()
        for sample in samples:
            dual = dual_path_analysis(sample, params)
            self.assertTrue(dual["consistent"])
            self.assertEqual(
                set(dual["preview"]["decisions"]),
                set(dual["precise"]["decisions"]),
            )
            confusion = confusion_against_truth(
                sample, dual["precise"]["decisions"]
            )
            self.assertTrue(confusion["within_tolerance"], sample.name)

    def test_preview_latency_p95_under_100ms(self):
        samples = load_dataset(SAMPLE_DIR, block_size=32)
        params = AnalysisParams()
        tracker = LatencyTracker()
        for _ in range(10):
            for sample in samples:
                tracker.record(dual_path_analysis(sample, params)["preview_ms"])
        stats = tracker.quantiles()
        self.assertLessEqual(stats["p95_ms"], 100.0, stats)

    def test_force_divergence_reports_first_block_and_metric(self):
        sample = load_sample(os.path.join(SAMPLE_DIR, "sample_rgb_256.png"))
        with self.assertRaises(ForensicsError) as ctx:
            dual_path_analysis(
                sample, AnalysisParams(), force_divergence=True
            )
        self.assertEqual(ctx.exception.code, E_DIVERGE)
        self.assertEqual(ctx.exception.metric, "suspicious")
        self.assertIsInstance(ctx.exception.block_id, int)

    def test_preview_gate_has_wide_margin_on_every_sample(self):
        # Guards against gate regression: pristine and tampered coarse-block
        # contrast must stay well separated around the declared gate.
        samples = load_dataset(SAMPLE_DIR, block_size=32)
        params = AnalysisParams()
        pristine = []
        tampered = []
        for sample in samples:
            truth = set(sample.manifest["tampered_block_ids"])
            for bid in range(len(sample.block_index)):
                small = downscale_single_block(
                    sample.block_pixels(bid), params.preview_scale
                )
                contrast = analyze_block(small, params)["local_contrast"]
                (tampered if bid in truth else pristine).append(contrast)
        self.assertLess(max(pristine), PREVIEW_CONTRAST_GATE)
        self.assertGreater(min(tampered), PREVIEW_CONTRAST_GATE)
        # Require a concrete margin on both sides, not a razor-thin boundary.
        self.assertGreater(PREVIEW_CONTRAST_GATE - max(pristine), 3.0)
        self.assertGreater(min(tampered) - PREVIEW_CONTRAST_GATE, 3.0)

    def test_subsampling_is_deterministic(self):
        sample = load_sample(
            os.path.join(SAMPLE_DIR, "sample_rgb_256.png")
        )
        block = sample.block_pixels(0)
        first = downscale_single_block(block, 2)
        second = downscale_single_block(block, 2)
        import numpy as np

        np.testing.assert_array_equal(first, second)


if __name__ == "__main__":
    unittest.main()
