from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from mshdr.io_utils import read_json
from mshdr.color import (
    SRGB_WHITE_XYZ,
    bradford_adaptation_matrix,
    map_to_gamut,
    quantize_linear_rgb,
)
from mshdr.errors import WorkbenchError
from mshdr.filtering import denoise_and_enhance_detail, filtering_quality, guided_filter
from mshdr.params import WorkbenchParams
from mshdr.pipeline import WorkbenchEngine

from support import generate_single_scene


class FilteringColorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene = cls.index.load_scene(cls.index.scene_records[0]["scene_id"])
        cls.params = WorkbenchParams(max_workers=2)
        cls.engine = WorkbenchEngine(cls.scene, cls.params)
        cls.base = cls.engine._ensure_base()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_guided_filter_is_repeatable_and_quality_bounds_pass(self) -> None:
        first = guided_filter(
            self.base.fused_radiance,
            self.scene.detail_layer,
            radius=self.params.guide_radius,
            eps=self.params.guide_eps,
        )
        second = guided_filter(
            self.base.fused_radiance,
            self.scene.detail_layer,
            radius=self.params.guide_radius,
            eps=self.params.guide_eps,
        )
        self.assertTrue(np.array_equal(first, second))
        quality = self.scene.truth["quality"]
        metrics = filtering_quality(
            first,
            self.scene.clean_detail,
            noise_roi=quality["noise_roi"],
            edge_roi=quality["edge_roi"],
        )
        self.assertLess(metrics["residual_std"], quality["max_residual_std"])
        self.assertGreaterEqual(metrics["edge_fidelity"], quality["min_edge_fidelity"])

    def test_denoise_enhance_repeatability(self) -> None:
        first = denoise_and_enhance_detail(
            self.base.fused_radiance,
            self.scene.detail_layer,
            radius=self.params.guide_radius,
            eps=self.params.guide_eps,
            gain=self.params.contrast_gain,
            threshold=self.params.contrast_threshold,
        )
        second = denoise_and_enhance_detail(
            self.base.fused_radiance,
            self.scene.detail_layer,
            radius=self.params.guide_radius,
            eps=self.params.guide_eps,
            gain=self.params.contrast_gain,
            threshold=self.params.contrast_threshold,
        )
        self.assertTrue(np.array_equal(first[0], second[0]))
        self.assertTrue(np.array_equal(first[1], second[1]))

    def test_filter_parameter_errors_name_fields(self) -> None:
        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(guide_radius=0).validate(self.scene.metadata, frame_count=3)
        self.assertEqual((context.exception.code, context.exception.field), ("E_PARA", "guide_radius"))
        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(guide_eps=0.0).validate(self.scene.metadata, frame_count=3)
        self.assertEqual((context.exception.code, context.exception.field), ("E_PARA", "guide_eps"))
        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(contrast_gain=-1.0).validate(self.scene.metadata, frame_count=3)
        self.assertEqual((context.exception.code, context.exception.field), ("E_PARA", "contrast_gain"))
        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(contrast_threshold=-0.1).validate(self.scene.metadata, frame_count=3)
        self.assertEqual((context.exception.code, context.exception.field), ("E_PARA", "contrast_threshold"))

    def test_gamut_compresses_and_exposes_boundary(self) -> None:
        rgb = np.array(
            [
                [[-0.20, 0.50, 1.20], [1.50, 0.35, 0.05]],
                [[0.50, 0.50, 0.50], [0.72, 0.30, -0.08]],
            ],
            dtype=np.float64,
        )
        result = map_to_gamut(rgb, intent="perceptual", tolerance=1.0e-6, max_iterations=8)
        self.assertEqual(result.out_of_gamut_count, 3)
        self.assertTrue(np.all(result.mapped >= -1.0e-6))
        self.assertTrue(np.all(result.mapped <= 1.0 + 1.0e-6))
        self.assertTrue(np.all(result.boundary_rgb[result.out_of_gamut_mask] >= 0.0))
        self.assertTrue(np.all(result.boundary_rgb[result.out_of_gamut_mask] <= 1.0))
        boundary = result.boundary_rgb[0, 0]
        self.assertTrue(np.isclose(np.max(boundary), 1.0) or np.isclose(np.min(boundary), 0.0))
        self.assertGreater(result.max_excess, 0.0)

    def test_gamut_nonfinite_and_relative_failure_are_gamut_errors(self) -> None:
        with self.assertRaises(WorkbenchError) as context:
            map_to_gamut(np.array([[[np.nan, 0.0, 0.0]]]), intent="perceptual")
        self.assertEqual(context.exception.code, "E_GAMUT")
        self.assertEqual(context.exception.details["out_of_gamut_count"], 1)

        with self.assertRaises(WorkbenchError) as context:
            map_to_gamut(np.array([[[2.0, 2.0, 2.0]]]), intent="relative_colorimetric")
        self.assertEqual(context.exception.code, "E_GAMUT")
        self.assertEqual(context.exception.details["out_of_gamut_count"], 1)
        self.assertGreaterEqual(context.exception.details["max_excess"], 1.0)

    def test_white_adaptation_and_output_bit_depth(self) -> None:
        identity = bradford_adaptation_matrix(SRGB_WHITE_XYZ, SRGB_WHITE_XYZ)
        self.assertTrue(np.allclose(identity, np.eye(3), atol=1.0e-12))
        d50 = np.array([0.96422, 1.0, 0.82521])
        adaptation = bradford_adaptation_matrix(d50, SRGB_WHITE_XYZ)
        self.assertTrue(np.allclose(adaptation @ d50, SRGB_WHITE_XYZ, atol=1.0e-10))
        for bit_depth, dtype in ((8, np.uint8), (10, np.uint16), (12, np.uint16)):
            quantized, levels = quantize_linear_rgb(
                np.array([[[0.0, 0.5, 1.0]]]),
                bit_depth,
            )
            self.assertEqual(quantized.dtype, dtype)
            self.assertLessEqual(int(quantized.max()), (1 << bit_depth) - 1)
            self.assertEqual(levels, (1 << bit_depth) - 1)

    def test_in_gamut_patch_errors_are_bounded(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = Path(directory) / "out"
            result = self.engine.run(output_dir=output_dir, mode="full")
            segment_errors = read_json(output_dir / "segment_errors.json", field="segment_errors")["segments"]
        in_gamut = [patch for patch in result.stats["patch_errors"] if patch["expected_in_gamut"]]
        self.assertGreaterEqual(len(in_gamut), 4)
        self.assertLess(max(patch["mae"] for patch in in_gamut), 0.025)
        patch_segments = [segment for segment in segment_errors if segment["expected_patch_pixel_count"] > 0]
        self.assertGreaterEqual(len(patch_segments), 4)
        self.assertTrue(all(segment["patch_mae"] is not None for segment in patch_segments))


if __name__ == "__main__":
    unittest.main()

