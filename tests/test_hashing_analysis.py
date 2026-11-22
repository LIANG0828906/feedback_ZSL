import os
import unittest

import numpy as np

from forensics.errors import ForensicsError, E_PARA
from forensics.loader import load_sample
from forensics.params import AnalysisParams
from forensics.engine import build_precise_results, process_block
from forensics.analysis import analyze_block, monotonicity_check
from forensics.hashing import content_hash, fingerprint, median_filter2d


SAMPLE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
)


class TestHashingDeterminism(unittest.TestCase):
    def setUp(self):
        self.sample = load_sample(os.path.join(SAMPLE_DIR, "sample_rgb_256.png"))
        self.params = AnalysisParams()

    def test_hash_identical_across_repeats(self):
        first = build_precise_results(self.sample, self.params)
        second = build_precise_results(self.sample, self.params)
        self.assertEqual(
            [r["content_hash"] for r in first],
            [r["content_hash"] for r in second],
        )
        self.assertEqual(
            [r["fingerprint"]["digest"] for r in first],
            [r["fingerprint"]["digest"] for r in second],
        )

    def test_order_and_thread_independence(self):
        ordered = build_precise_results(self.sample, self.params)
        shuffled_ids = np.random.RandomState(0).permutation(
            len(self.sample.block_index)
        )
        shuffled = {}
        for bid in shuffled_ids:
            shuffled[int(bid)] = process_block(
                self.sample.block_pixels(int(bid)), self.params
            )
        for record in ordered:
            bid = record["block_id"]
            self.assertEqual(
                record["content_hash"], shuffled[bid]["content_hash"]
            )
            self.assertEqual(
                record["fingerprint"]["digest"],
                shuffled[bid]["fingerprint"]["digest"],
            )

    def test_hash_algorithms_configurable(self):
        for algo in ("sha256", "sha1", "md5"):
            block = self.sample.block_pixels(0)
            digest = content_hash(block, algo)
            self.assertIsInstance(digest, str)
            self.assertTrue(digest)

    def test_fingerprint_granularity_zero_rejected(self):
        with self.assertRaises(ForensicsError) as ctx:
            fingerprint(self.sample.block_pixels(0), 0)
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "fingerprint_granularity")

    def test_median_window_zero_rejected(self):
        with self.assertRaises(ForensicsError) as ctx:
            median_filter2d(np.zeros((4, 4)), 0)
        self.assertEqual(ctx.exception.code, E_PARA)


class TestChannelSemantics(unittest.TestCase):
    def test_rgb_keeps_three_channels(self):
        sample = load_sample(os.path.join(SAMPLE_DIR, "sample_rgb_256.png"))
        record = analyze_block(sample.block_pixels(0), AnalysisParams())
        self.assertEqual(record["channels"], 3)
        self.assertEqual(len(record["per_channel"]["noise_var"]), 3)
        self.assertEqual(len(record["per_channel"]["edge_consistency"]), 3)

    def test_gray_keeps_one_channel(self):
        sample = load_sample(os.path.join(SAMPLE_DIR, "sample_gray_256.png"))
        record = analyze_block(sample.block_pixels(0), AnalysisParams())
        self.assertEqual(record["channels"], 1)
        self.assertEqual(len(record["per_channel"]["noise_var"]), 1)

    def test_no_silent_downmix(self):
        # Tampering only the red channel must change the red statistic without
        # being averaged away into a single luminance channel.
        block = np.full((16, 16, 3), 100, dtype=np.uint8)
        tampered = block.copy()
        rng = np.random.RandomState(1)
        tampered[:, :, 0] = np.clip(
            tampered[:, :, 0] + rng.normal(0, 60, (16, 16)), 0, 255
        ).astype(np.uint8)
        base = analyze_block(block, AnalysisParams())
        changed = analyze_block(tampered, AnalysisParams())
        self.assertNotAlmostEqual(
            base["per_channel"]["noise_var"][0],
            changed["per_channel"]["noise_var"][0],
        )
        # Green and blue channels remain untouched.
        self.assertAlmostEqual(
            base["per_channel"]["noise_var"][1],
            changed["per_channel"]["noise_var"][1],
        )


class TestMonotonicity(unittest.TestCase):
    def test_flagged_set_monotonic_as_noise_threshold_tightens(self):
        sample = load_sample(os.path.join(SAMPLE_DIR, "sample_rgb_256.png"))
        base = AnalysisParams()
        flagged_counts = []
        for threshold in (2000.0, 500.0, 100.0, 10.0):
            params = AnalysisParams(noise_threshold=threshold)
            results = build_precise_results(sample, params)
            flagged_counts.append(
                sum(r["analysis"]["suspicious"] for r in results)
            )
        # Lowering (tightening) the threshold can only add flagged blocks.
        self.assertTrue(monotonicity_check(flagged_counts, list(range(4))))
        self.assertEqual(flagged_counts, sorted(flagged_counts))


if __name__ == "__main__":
    unittest.main()
