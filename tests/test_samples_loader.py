from __future__ import annotations

import json
from pathlib import Path
import shutil
import tempfile
import unittest

import numpy as np

from edgeforge.errors import DecodeError, MetaError
from edgeforge.hashing import sha256_array
from edgeforge.loader import load_sample
from edgeforge.samples import generate_samples


class SampleLoaderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name) / "samples"
        generate_samples(
            cls.root,
            ["low_contrast_gray_320x240", "noisy_rgb_800x600"],
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def test_generation_is_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as other:
            other_root = Path(other) / "samples"
            generate_samples(other_root, ["low_contrast_gray_320x240"])
            first = load_sample(self.root, "low_contrast_gray_320x240")
            second = load_sample(other_root, "low_contrast_gray_320x240")
            self.assertEqual(first.image_sha256, second.image_sha256)
            self.assertEqual(first.truth_sha256, second.truth_sha256)
            self.assertTrue(np.array_equal(first.image, second.image))
            self.assertTrue(np.array_equal(first.truth_mask, second.truth_mask))
            self.assertEqual(first.image.dtype, np.uint8)

    def test_truth_declarations(self) -> None:
        sample = load_sample(self.root, "low_contrast_gray_320x240")
        self.assertGreater(len(sample.lines), 0)
        self.assertGreater(len(sample.circles), 0)
        self.assertAlmostEqual(
            float(sample.truth_mask.mean()),
            float(sample.meta["truth"]["edge_coverage"]),
            places=12,
        )
        for line in sample.lines:
            self.assertEqual(len(line["start"]), 2)
            self.assertEqual(len(line["end"]), 2)

    def test_load_order_does_not_matter(self) -> None:
        ids = ["noisy_rgb_800x600", "low_contrast_gray_320x240"]
        forward = {sample_id: load_sample(self.root, sample_id) for sample_id in ids}
        backward = {
            sample_id: load_sample(self.root, sample_id)
            for sample_id in reversed(ids)
        }
        for sample_id in ids:
            self.assertEqual(
                forward[sample_id].image_sha256,
                backward[sample_id].image_sha256,
            )
            self.assertEqual(
                forward[sample_id].truth_sha256,
                backward[sample_id].truth_sha256,
            )

    def test_corrupted_image_returns_e_decode(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            copied = Path(temp) / "samples"
            shutil.copytree(self.root, copied)
            (copied / "low_contrast_gray_320x240" / "image.png").write_bytes(b"bad")
            with self.assertRaises(DecodeError) as context:
                load_sample(copied, "low_contrast_gray_320x240")
            self.assertEqual(context.exception.code, "E_DECODE")

    def test_size_mismatch_returns_e_meta_with_field(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            copied = Path(temp) / "samples"
            shutil.copytree(self.root, copied)
            meta_path = copied / "low_contrast_gray_320x240" / "meta.json"
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            meta["width"] = int(meta["width"]) + 1
            meta_path.write_text(
                json.dumps(meta, indent=2, sort_keys=True),
                encoding="utf-8",
            )
            with self.assertRaises(MetaError) as context:
                load_sample(copied, "low_contrast_gray_320x240")
            self.assertEqual(context.exception.code, "E_META")
            self.assertEqual(context.exception.field, "width")

    def test_channel_mismatch_returns_e_meta_with_field(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            copied = Path(temp) / "samples"
            shutil.copytree(self.root, copied)
            meta_path = copied / "low_contrast_gray_320x240" / "meta.json"
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            meta["channels"] = 3
            meta_path.write_text(
                json.dumps(meta, indent=2, sort_keys=True),
                encoding="utf-8",
            )
            with self.assertRaises(MetaError) as context:
                load_sample(copied, "low_contrast_gray_320x240")
            self.assertEqual(context.exception.code, "E_META")
            self.assertEqual(context.exception.field, "channels")

    def test_rgb_metadata_and_hash(self) -> None:
        sample = load_sample(self.root, "noisy_rgb_800x600")
        self.assertEqual(sample.image.shape, (600, 800, 3))
        self.assertEqual(sample.image_sha256, sha256_array(sample.image))
        self.assertEqual(sample.truth_sha256, sha256_array(sample.truth_mask.astype(np.uint8)))


if __name__ == "__main__":
    unittest.main()

