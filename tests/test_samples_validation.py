from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from artmask.config import Budget, SegmentationParams
from artmask.budget import choose_mode
from artmask.errors import BusinessError
from artmask.samplegen import DEFAULT_SAMPLES, generate_sample, generate_suite
from artmask.validation import load_sample, load_suite


class SampleValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temp.name) / "samples"
        cls.specs = (
            DEFAULT_SAMPLES[0],
            DEFAULT_SAMPLES[2],
        )
        generate_suite(cls.root, cls.specs)
        cls.source = cls.root / cls.specs[0].sample_id

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _copy_sample(self, name: str) -> Path:
        target = Path(self.temp.name) / name
        shutil.copytree(self.source, target)
        return target

    def test_suite_loads_and_manifest_geometry_matches_mask(self) -> None:
        samples = load_suite(self.root)
        self.assertEqual(len(samples), 2)
        for sample in samples:
            self.assertEqual(sample.image.shape, (sample.height, sample.width, 3))
            for region in sample.manifest["regions"]:
                region_id = int(region["id"])
                ys, xs = np.nonzero(sample.truth_mask == region_id)
                self.assertEqual(int(region["area"]), int(len(xs)))
                expected_centroid = np.array(
                    [xs.mean(), ys.mean()], dtype=np.float64
                )
                self.assertTrue(
                    np.allclose(region["centroid"], expected_centroid, atol=1e-9)
                )
                expected_bbox = [
                    int(xs.min()),
                    int(ys.min()),
                    int(xs.max() - xs.min() + 1),
                    int(ys.max() - ys.min() + 1),
                ]
                self.assertEqual(region["bbox"], expected_bbox)

    def test_generator_is_deterministic(self) -> None:
        first = Path(self.temp.name) / "deterministic-a"
        second = Path(self.temp.name) / "deterministic-b"
        first.mkdir()
        second.mkdir()
        spec = DEFAULT_SAMPLES[1]
        info_a = generate_sample(spec, first)
        info_b = generate_sample(spec, second)
        self.assertEqual(info_a["image_hash"], info_b["image_hash"])
        sample_a = load_sample(first / spec.sample_id)
        sample_b = load_sample(second / spec.sample_id)
        self.assertTrue(np.array_equal(sample_a.image, sample_b.image))
        self.assertTrue(np.array_equal(sample_a.truth_mask, sample_b.truth_mask))
        self.assertEqual(
            sample_a.manifest["regions"], sample_b.manifest["regions"]
        )

    def test_missing_manifest_returns_e_decode_with_file(self) -> None:
        target = self._copy_sample("missing-manifest")
        (target / "manifest.json").unlink()
        with self.assertRaises(BusinessError) as context:
            load_sample(target)
        self.assertEqual(context.exception.code, "E_DECODE")
        self.assertEqual(
            context.exception.details["file"], str((target / "manifest.json").resolve())
        )

    def test_corrupted_image_returns_e_decode_with_file(self) -> None:
        target = self._copy_sample("corrupt-image")
        (target / "image.npy").write_bytes(b"not-a-npy")
        with self.assertRaises(BusinessError) as context:
            load_sample(target)
        self.assertEqual(context.exception.code, "E_DECODE")
        self.assertEqual(
            context.exception.details["file"], str((target / "image.npy").resolve())
        )

    def test_missing_truth_mask_returns_e_decode_with_file(self) -> None:
        target = self._copy_sample("missing-mask")
        (target / "truth_mask.npy").unlink()
        with self.assertRaises(BusinessError) as context:
            load_sample(target)
        self.assertEqual(context.exception.code, "E_DECODE")
        self.assertIn("truth_mask.npy", context.exception.details["file"])

    def test_channel_mismatch_returns_e_meta_channels(self) -> None:
        target = self._copy_sample("channel-mismatch")
        sample = load_sample(target)
        rgba = np.dstack([sample.image, np.full(sample.image.shape[:2], 255, np.uint8)])
        np.save(target / "image.npy", rgba, allow_pickle=False)
        with self.assertRaises(BusinessError) as context:
            load_sample(target)
        self.assertEqual(context.exception.code, "E_META")
        self.assertEqual(context.exception.field, "channels")

    def test_dimension_mismatch_returns_e_meta_width(self) -> None:
        target = self._copy_sample("width-mismatch")
        manifest_path = target / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["width"] += 1
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        with self.assertRaises(BusinessError) as context:
            load_sample(target)
        self.assertEqual(context.exception.code, "E_META")
        self.assertEqual(context.exception.field, "width")

    def test_parameter_validation_names_each_field(self) -> None:
        cases = [
            (SegmentationParams(color_tolerance=0.0), "color_tolerance"),
            (SegmentationParams(gradient_threshold=2000.0), "gradient_threshold"),
            (SegmentationParams(confidence_threshold=1.5), "confidence_threshold"),
            (SegmentationParams(morph_size=4), "morph_size"),
            (SegmentationParams(min_area=-1), "min_area"),
            (
                SegmentationParams(auto_seeds=False, seeds=((1000, 1000),)),
                "seeds[0]",
            ),
        ]
        for params, field in cases:
            with self.subTest(field=field):
                with self.assertRaises(BusinessError) as context:
                    params.validate(image_shape=(100, 100))
                self.assertEqual(context.exception.code, "E_PARA")
                self.assertEqual(context.exception.field, field)

    def test_budget_validation_names_field(self) -> None:
        with self.assertRaises(BusinessError) as context:
            Budget(time_seconds=0).validate()
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "budget.time_seconds")

    def test_budget_tightening_is_stable_and_explained(self) -> None:
        budget = Budget(time_seconds=20.0, memory_mb=512.0)
        first = choose_mode(2048, 1536, 5, budget)
        second = choose_mode(2048, 1536, 5, budget)
        self.assertEqual(first.mode, "downsampled")
        self.assertEqual(first.downsample_factor, 2)
        self.assertEqual(first.to_dict(), second.to_dict())
        self.assertIsNotNone(first.degradation_reason)
        self.assertIn("超过 20.00s", first.degradation_reason)
        self.assertTrue(first.impact_scope)
        with self.assertRaises(BusinessError) as context:
            choose_mode(
                2048,
                1536,
                5,
                Budget(time_seconds=0.01, memory_mb=1.0),
            )
        self.assertEqual(context.exception.code, "E_BUDGET")


if __name__ == "__main__":
    unittest.main()
