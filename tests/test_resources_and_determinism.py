import os
import shutil
import tempfile
import unittest

import numpy as np

from forensics.loader import load_sample
from forensics.params import AnalysisParams, VersionControl
from forensics.scheduler import Scheduler
from forensics import sample_generator


SAMPLE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
)


class TestResourceBounds(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.sample = load_sample(
            os.path.join(SAMPLE_DIR, "sample_rgb_384x320.png"), block_size=32
        )
        self.vc = VersionControl(AnalysisParams())

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_tiny_buffer_still_completes_all_blocks(self):
        # A one-slot pixel budget forces workers to block; no block is dropped.
        scheduler = Scheduler(
            self.sample,
            self.vc.latest,
            self.tmp,
            max_workers=4,
            max_pixel_buffers=1,
        )
        summary = scheduler.run()
        self.assertEqual(summary["failed"], [])
        self.assertEqual(summary["committed"], len(scheduler.states))
        self.assertEqual(
            summary["committed_order"], sorted(summary["committed_order"])
        )

    def test_commit_sequence_stable_across_worker_counts(self):
        orders = []
        for workers in (1, 2, 8):
            tmp = tempfile.mkdtemp()
            try:
                scheduler = Scheduler(
                    self.sample, self.vc.latest, tmp, max_workers=workers
                )
                summary = scheduler.run()
                orders.append(summary["committed_order"])
            finally:
                shutil.rmtree(tmp, ignore_errors=True)
        self.assertEqual(orders[0], orders[1])
        self.assertEqual(orders[1], orders[2])


class TestSampleDeterminism(unittest.TestCase):
    def test_regeneration_is_byte_identical(self):
        first = tempfile.mkdtemp()
        second = tempfile.mkdtemp()
        try:
            sample_generator.generate_all(first)
            sample_generator.generate_all(second)
            for name in os.listdir(first):
                with open(os.path.join(first, name), "rb") as a, open(
                    os.path.join(second, name), "rb"
                ) as b:
                    self.assertEqual(a.read(), b.read(), name)
        finally:
            shutil.rmtree(first, ignore_errors=True)
            shutil.rmtree(second, ignore_errors=True)

    def test_dataset_has_required_mix(self):
        from forensics.loader import load_dataset

        samples = load_dataset(SAMPLE_DIR)
        self.assertGreaterEqual(len(samples), 5)
        channels = {s.channels for s in samples}
        self.assertEqual(channels, {1, 3})
        resolutions = {(s.height, s.width) for s in samples}
        self.assertGreater(len(resolutions), 1)
        for sample in samples:
            self.assertTrue(any(sample.ground_truth_mask()))
            self.assertGreater(len(sample.manifest["primitives"]), 0)


if __name__ == "__main__":
    unittest.main()
