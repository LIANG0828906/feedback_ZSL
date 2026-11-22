from __future__ import annotations

import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import numpy as np

from artmask.config import Budget, QualityTolerance, ScaleTolerance, SegmentationParams
from artmask.errors import BusinessError
from artmask.experiments import run_scale_experiment
from artmask.geometry import compare_masks, extract_regions, scale_consistency
from artmask.samplegen import DEFAULT_SAMPLES, generate_suite
from artmask.segmentation import segment_image
from artmask.validation import load_suite


class SegmentationGeometryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory()
        root = Path(cls.temp.name) / "samples"
        generate_suite(root, DEFAULT_SAMPLES)
        cls.samples = {sample.sample_id: sample for sample in load_suite(root)}

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    def _params(self, sample_id: str) -> SegmentationParams:
        sample = self.samples[sample_id]
        return SegmentationParams().for_sample(
            sample.recommended_params, sample.region_count
        )

    def test_fine_masks_match_truth_geometry(self) -> None:
        for sample_id in (
            "s01-color-blocks",
            "s03-low-contrast",
            "s04-embedded-noise",
            "s05-multi-overlap",
        ):
            with self.subTest(sample=sample_id):
                sample = self.samples[sample_id]
                result = segment_image(
                    sample.image, self._params(sample_id), mode="fine"
                )
                report = compare_masks(
                    result.final_labels,
                    sample.truth_mask,
                    tolerance=QualityTolerance(),
                )
                self.assertEqual(
                    report.region_count_predicted, report.region_count_truth
                )
                self.assertLess(report.mean_centroid_error_px, 1.0)
                self.assertLess(report.mean_area_relative_error, 0.03)
                self.assertGreater(report.mean_bbox_iou, 0.95)
                self.assertGreater(report.mean_mask_iou, 0.98)

    def test_thread_and_repeat_determinism(self) -> None:
        sample = self.samples["s04-embedded-noise"]
        params = self._params(sample.sample_id)

        def run_once(_index: int) -> tuple[bytes, bytes, list[tuple[int, int]]]:
            result = segment_image(sample.image, params, mode="fine")
            return (
                result.initial_labels.tobytes(),
                result.final_labels.tobytes(),
                result.seeds,
            )

        with ThreadPoolExecutor(max_workers=4, thread_name_prefix="different") as pool:
            outputs = list(pool.map(run_once, range(4)))
        self.assertTrue(all(item == outputs[0] for item in outputs))

    def test_downsampled_scale_tolerance_and_not_worsening_trend(self) -> None:
        budget = Budget(
            time_seconds=20.0,
            memory_mb=512.0,
            tile_size=256,
            max_workers=4,
        )
        payload = run_scale_experiment(
            list(self.samples.values()),
            SegmentationParams(),
            budget,
            scale_tolerance=ScaleTolerance(),
            quality_tolerance=QualityTolerance(),
        )
        self.assertTrue(payload["all_within_tolerance"])
        self.assertTrue(payload["not_monotonically_worsening"])
        self.assertFalse(payload["combined_deviation_monotonic_worsening"])

    def test_scale_violation_returns_e_scale_with_region_metrics(self) -> None:
        sample = self.samples["s01-color-blocks"]
        params = self._params(sample.sample_id)
        fine = segment_image(sample.image, params, mode="fine")
        coarse = segment_image(sample.image, params, mode="downsampled", factor=4)
        with self.assertRaises(BusinessError) as context:
            scale_consistency(
                fine.final_labels,
                coarse.final_labels,
                4,
                ScaleTolerance(
                    centroid_px=0.0001,
                    area_relative=0.0001,
                    bbox_iou_min=0.99999,
                ),
            )
        self.assertEqual(context.exception.code, "E_SCALE")
        violations = context.exception.details["violations"]
        self.assertTrue(violations)
        self.assertIn("region_id", violations[0])
        self.assertIn("centroid_error_px", violations[0])
        self.assertIn("area_relative_error", violations[0])
        self.assertIn("bbox_iou", violations[0])

    def test_region_count_mismatch_returns_e_region_with_areas(self) -> None:
        sample = self.samples["s01-color-blocks"]
        predicted = sample.truth_mask.copy()
        predicted[predicted == 1] = 0
        extra_region = np.argwhere((predicted == 0) & (sample.truth_mask == 0))
        for label, offset in ((90, 0), (91, 101)):
            y, x = extra_region[offset]
            predicted[y : y + 8, x : x + 8] = label
        with self.assertRaises(BusinessError) as context:
            compare_masks(predicted, sample.truth_mask)
        self.assertEqual(context.exception.code, "E_REGION")
        self.assertIn("extra_regions", context.exception.details)
        self.assertIn("missing_regions", context.exception.details)
        self.assertTrue(
            context.exception.details["extra_regions"]
            or context.exception.details["missing_regions"]
        )
        for item in (
            context.exception.details["extra_regions"]
            + context.exception.details["missing_regions"]
        ):
            self.assertIn("region_id", item)
            self.assertIn("area", item)

        missing_only = sample.truth_mask.copy()
        missing_only[missing_only == 1] = 0
        with self.assertRaises(BusinessError) as missing_context:
            compare_masks(missing_only, sample.truth_mask)
        self.assertEqual(missing_context.exception.code, "E_REGION")
        self.assertEqual(
            missing_context.exception.details["missing_regions"][0]["region_id"], 1
        )

    def test_optimal_matching_handles_permuted_region_labels(self) -> None:
        sample = self.samples["s02-gradient"]
        remapped = np.zeros_like(sample.truth_mask)
        mapping = {1: 20, 2: 10, 3: 30}
        for source, target in mapping.items():
            remapped[sample.truth_mask == source] = target
        report = compare_masks(remapped, sample.truth_mask)
        self.assertEqual(report.mean_centroid_error_px, 0.0)
        self.assertEqual(report.mean_mask_iou, 1.0)
        matched = {
            (item.truth_region_id, item.predicted_region_id)
            for item in report.regions
        }
        self.assertEqual(matched, {(1, 20), (2, 10), (3, 30)})

    def test_contours_are_closed_and_use_final_mask(self) -> None:
        sample = self.samples["s01-color-blocks"]
        result = segment_image(
            sample.image, self._params(sample.sample_id), mode="fine"
        )
        regions = extract_regions(result.final_labels, include_contours=True)
        self.assertEqual(len(regions), sample.region_count)
        for region in regions:
            self.assertGreaterEqual(len(region.contour), 4)
            self.assertEqual(region.contour[0], region.contour[-1])
            self.assertEqual(
                region.area,
                int(np.count_nonzero(result.final_labels == region.region_id)),
            )


if __name__ == "__main__":
    unittest.main()
