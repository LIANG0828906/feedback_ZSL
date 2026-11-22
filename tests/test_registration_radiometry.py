from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from mshdr.dataset import DatasetIndex
from mshdr.errors import WorkbenchError
from mshdr.params import WorkbenchParams
from mshdr.pipeline import WorkbenchEngine
from mshdr.radiometry import linearize_and_fuse
from mshdr.registration import align_bands

from support import generate_single_scene


class RegistrationRadiometryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene = cls.index.load_scene(cls.index.scene_records[0]["scene_id"])

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_alignment_shape_and_order(self) -> None:
        for order in (1, 3, 5):
            aligned, report = align_bands(
                self.scene.bands,
                self.scene.registration_offsets,
                reference_band=self.scene.reference_band,
                interpolation_order=order,
                max_shift_px=float(self.scene.metadata["max_registration_shift_px"]),
            )
            self.assertEqual(aligned.shape, self.scene.bands.shape)
            self.assertEqual(tuple(report["aligned_shape"][1:]), self.scene.pixel_size)
            self.assertEqual(report["interpolation_order"], order)

    def test_registration_range_error_has_pair_and_offset(self) -> None:
        offsets = self.scene.registration_offsets.copy()
        offsets[2, 0] = 99.0
        with self.assertRaises(WorkbenchError) as context:
            align_bands(
                self.scene.bands,
                offsets,
                reference_band=0,
                interpolation_order=3,
                max_shift_px=6.0,
            )
        self.assertEqual(context.exception.code, "E_REG")
        self.assertEqual(context.exception.details["band_pair"], [0, 2])
        self.assertEqual(context.exception.details["measured_offset"], [99.0, float(offsets[2, 1])])

    def test_interpolation_and_weight_parameter_errors(self) -> None:
        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(interpolation_order=9).validate(self.scene.metadata, frame_count=3)
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "interpolation_order")

        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(exposure_weights=(1.0, -0.1, 0.2)).validate(self.scene.metadata, frame_count=3)
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "exposure_weights")

        with self.assertRaises(WorkbenchError) as context:
            WorkbenchParams(exposure_weights=(1.0, 0.0)).validate(self.scene.metadata, frame_count=3)
        self.assertEqual(context.exception.code, "E_PARA")
        self.assertEqual(context.exception.field, "exposure_weights")

    def test_inverse_response_fusion_is_deterministic(self) -> None:
        engine = WorkbenchEngine(self.scene, WorkbenchParams())
        first = engine._ensure_base()
        second = engine._ensure_base()
        self.assertIs(first, second)
        self.assertEqual(first.fused_radiance.shape, self.scene.pixel_size)
        self.assertTrue(np.all(np.isfinite(first.fused_radiance)))
        self.assertGreaterEqual(float(first.fused_radiance.min()), 0.0)
        self.assertGreater(first.fusion["valid_sample_fraction"], 0.5)
        fused_again, _stats = linearize_and_fuse(
            self.scene.exposure_frames,
            self.scene.metadata["exposures"],
            WorkbenchParams().exposure_weights,
            bit_depth=12,
        )
        self.assertTrue(np.array_equal(first.fused_radiance, fused_again))

    def test_reversed_exposure_times_are_order_error(self) -> None:
        exposures = {
            **self.scene.metadata["exposures"],
            "frames": list(reversed(self.scene.metadata["exposures"]["frames"])),
        }
        with self.assertRaises(WorkbenchError) as context:
            linearize_and_fuse(
                self.scene.exposure_frames,
                exposures,
                WorkbenchParams().exposure_weights,
                bit_depth=12,
            )
        self.assertEqual(context.exception.code, "E_ORDER")


if __name__ == "__main__":
    unittest.main()

