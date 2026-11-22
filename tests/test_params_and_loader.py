import json
import os
import tempfile

import numpy as np
import unittest
from PIL import Image

from forensics.errors import ForensicsError, E_PARA, E_META, E_DECODE
from forensics.params import AnalysisParams, VersionControl
from forensics.loader import load_sample, BlockIndex


SAMPLE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "samples"
)


class TestAnalysisParams(unittest.TestCase):
    def test_valid_defaults(self):
        params = AnalysisParams().validate()
        self.assertEqual(params.block_size, 32)

    def test_block_size_zero(self):
        with self.assertRaises(ForensicsError) as ctx:
            AnalysisParams(block_size=0).validate()
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "block_size")

    def test_even_window_rejected(self):
        with self.assertRaises(ForensicsError) as ctx:
            AnalysisParams(noise_window=4).validate()
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "noise_window")

    def test_fingerprint_granularity_out_of_range(self):
        with self.assertRaises(ForensicsError) as ctx:
            AnalysisParams(block_size=32, fingerprint_granularity=64).validate()
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "fingerprint_granularity")

    def test_bad_hash_algorithm(self):
        with self.assertRaises(ForensicsError) as ctx:
            AnalysisParams(hash_algorithm="nope").validate()
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "hash_algorithm")

    def test_negative_threshold(self):
        with self.assertRaises(ForensicsError) as ctx:
            AnalysisParams(noise_threshold=-1).validate()
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "noise_threshold")


class TestVersionControl(unittest.TestCase):
    def test_edit_creates_new_version_and_reuses_unchanged(self):
        vc = VersionControl()
        v1 = vc.latest
        v2 = vc.edit(noise_threshold=50.0)
        self.assertNotEqual(v1.version, v2.version)
        v3 = vc.edit(noise_threshold=50.0)
        self.assertEqual(v2.version, v3.version)

    def test_locked_snapshot_is_distinct_from_latest(self):
        vc = VersionControl()
        locked = vc.latest
        vc.edit(noise_threshold=10.0)
        self.assertNotEqual(locked.version, vc.latest.version)


class TestBlockIndex(unittest.TestCase):
    def test_oversized_block_raises_para(self):
        with self.assertRaises(ForensicsError) as ctx:
            BlockIndex(16, 16, 32)
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "block_size")

    def test_block_id_out_of_range(self):
        idx = BlockIndex(64, 64, 32)
        with self.assertRaises(ForensicsError) as ctx:
            idx.bounds(99)
        self.assertEqual(ctx.exception.code, E_PARA)
        self.assertEqual(ctx.exception.field, "block_id")

    def test_partial_edge_blocks_record_bounds(self):
        idx = BlockIndex(40, 40, 32)
        self.assertEqual(len(idx), 4)
        self.assertEqual(idx.bounds(3), (32, 32, 8, 8))


class TestLoader(unittest.TestCase):
    def test_load_rgb_and_gray(self):
        rgb = load_sample(os.path.join(SAMPLE_DIR, "sample_rgb_256.png"))
        self.assertEqual(rgb.channels, 3)
        self.assertEqual(rgb.pixels.shape, (256, 256, 3))
        gray = load_sample(os.path.join(SAMPLE_DIR, "sample_gray_256.png"))
        self.assertEqual(gray.pixels.shape, (256, 256, 1))

    def test_missing_manifest_is_e_decode(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "x.png")
            Image.fromarray(np.zeros((8, 8), dtype=np.uint8), mode="L").save(path)
            with self.assertRaises(ForensicsError) as ctx:
                load_sample(path)
            self.assertEqual(ctx.exception.code, E_DECODE)

    def test_corrupt_image_is_e_decode(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "x.png")
            manifest = os.path.join(tmp, "x.manifest.json")
            with open(path, "wb") as fh:
                fh.write(b"not a real png")
            with open(manifest, "w") as fh:
                json.dump({"width": 8, "height": 8, "channels": 1}, fh)
            with self.assertRaises(ForensicsError) as ctx:
                load_sample(path)
            self.assertEqual(ctx.exception.code, E_DECODE)

    def test_dimension_mismatch_is_e_meta(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "x.png")
            manifest = os.path.join(tmp, "x.manifest.json")
            Image.fromarray(np.zeros((8, 8), dtype=np.uint8), mode="L").save(path)
            with open(manifest, "w") as fh:
                json.dump(
                    {
                        "width": 16,
                        "height": 8,
                        "channels": 1,
                        "tamper_mask": [],
                        "primitives": [],
                    },
                    fh,
                )
            with self.assertRaises(ForensicsError) as ctx:
                load_sample(path)
            self.assertEqual(ctx.exception.code, E_META)
            self.assertEqual(ctx.exception.field, "width")

    def test_channel_mismatch_is_e_meta(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "x.png")
            manifest = os.path.join(tmp, "x.manifest.json")
            Image.fromarray(np.zeros((8, 8, 3), dtype=np.uint8), mode="RGB").save(
                path
            )
            with open(manifest, "w") as fh:
                json.dump(
                    {
                        "width": 8,
                        "height": 8,
                        "channels": 1,
                        "tamper_mask": [],
                        "primitives": [],
                    },
                    fh,
                )
            with self.assertRaises(ForensicsError) as ctx:
                load_sample(path)
            self.assertEqual(ctx.exception.code, E_META)
            self.assertEqual(ctx.exception.field, "channels")


if __name__ == "__main__":
    unittest.main()
