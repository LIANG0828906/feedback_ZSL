from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from spectraforge.color import map_gamut
from spectraforge.errors import WorkbenchError
from spectraforge.fusion import _fusion_weights, fuse_exposures
from spectraforge.metadata import load_scene
from spectraforge.parameters import default_parameters, validate_parameters
from spectraforge.pipeline import evaluate_color_truth, prepare_scene
from spectraforge.sample_generator import generate_all
from spectraforge.util import canonical_json_bytes

ROOT = Path(__file__).resolve().parents[1]


class TestGenerationAndFusion(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory(prefix="sf-samples-")
        cls.samples = Path(cls._temp.name) / "samples"
        cls.catalog = generate_all(
            cls.samples,
            seed=7777,
            scene_count=1,
            width=96,
            height=64,
            band_count=8,
            exposure_count=6,
            with_faults=True,
        )
        cls.scene_path = cls.samples / cls.catalog["scenes"][0]["path"]
        cls.scene = load_scene(cls.scene_path)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_deterministic_generation(self) -> None:
        with tempfile.TemporaryDirectory(prefix="sf-samples-2-") as temp:
            second = generate_all(
                Path(temp) / "samples",
                seed=7777,
                scene_count=1,
                width=96,
                height=64,
                band_count=8,
                exposure_count=6,
                with_faults=False,
            )
            self.assertEqual(
                self.catalog["scenes"][0]["metadata_hash"],
                second["scenes"][0]["metadata_hash"],
            )
            first_band = (self.scene_path / "bands" / "band_00.msi").read_bytes()
            second_band = (
                Path(temp) / "samples" / second["scenes"][0]["path"] / "bands" / "band_00.msi"
            ).read_bytes()
            self.assertEqual(first_band, second_band)
            self.assertEqual(
                canonical_json_bytes(self.catalog["scenes"][0]),
                canonical_json_bytes(second["scenes"][0]),
            )

    def test_fault_samples_have_stable_codes(self) -> None:
        expected = {
            name: item["expected_code"] for name, item in self.catalog["faults"].items()
        }
        for name, code in expected.items():
            with self.subTest(name=name):
                with self.assertRaises(WorkbenchError) as raised:
                    load_scene(self.samples / self.catalog["faults"][name]["path"])
                self.assertEqual(raised.exception.code, code)

    def test_analytical_color_truth_and_peak(self) -> None:
        prepared = prepare_scene(self.scene, "bilinear")
        report = evaluate_color_truth(self.scene, prepared)
        self.assertTrue(report["all_patches_within_tolerance"])
        self.assertLessEqual(report["max_abs_error"], report["tolerance"])
        self.assertLess(report["peak_relative_error"], 0.01)
        self.assertEqual(
            report["registration_offsets"],
            report["truth_offsets"],
        )

    def test_weight_function_suppresses_both_extremes(self) -> None:
        adjusted = np.array([0.0, 0.005, 0.25, 0.5, 0.75, 0.999, 1.0])
        weights = _fusion_weights(
            adjusted,
            weight_power=2.0,
            saturation_threshold=0.995,
            underexposure_threshold=0.008,
        )
        self.assertEqual(float(weights[0]), 0.0)
        self.assertEqual(float(weights[-1]), 0.0)
        self.assertEqual(float(weights[1]), 0.0)
        self.assertGreater(float(weights[3]), float(weights[2]))
        self.assertGreater(float(weights[3]), float(weights[4]))

    def test_nested_exposure_subset_peak_is_monotonic(self) -> None:
        previous = -1.0
        exposure_count = self.scene.exposure_count
        for count in range(1, exposure_count + 1):
            params = default_parameters(exposure_count)
            params["exposure_indices"] = list(range(exposure_count - count, exposure_count))
            result = fuse_exposures(
                self.scene.exposure_raw,
                self.scene.metadata.exposures,
                params,
                self.scene.metadata.camera_response,
            )
            peak = float(result.radiance.max())
            self.assertGreaterEqual(peak + 1e-6, previous)
            previous = peak
        self.assertLess(previous, float(self.scene.truth["peak_radiance"]) + 0.2)

    def test_parameter_errors_name_the_field(self) -> None:
        with self.assertRaises(WorkbenchError) as raised:
            validate_parameters(
                {**self.scene.metadata.default_parameters, "weight_power": 99},
                self.scene.exposure_count,
            )
        self.assertEqual(raised.exception.code, "E_PARA")
        self.assertEqual(raised.exception.details["field"], "weight_power")
        with self.assertRaises(WorkbenchError) as empty:
            validate_parameters(
                {**self.scene.metadata.default_parameters, "exposure_indices": []},
                self.scene.exposure_count,
            )
        self.assertEqual(empty.exception.code, "E_PARA")
        self.assertEqual(empty.exception.details["field"], "exposure_indices")

    def test_strict_gamut_error_contains_coordinate_and_channel(self) -> None:
        rgb = np.array([[[-0.1, 0.5, 1.1]]], dtype=np.float64)
        with self.assertRaises(WorkbenchError) as raised:
            map_gamut(rgb, "strict")
        self.assertEqual(raised.exception.code, "E_GAMUT")
        self.assertEqual(raised.exception.details["x"], 0)
        self.assertEqual(raised.exception.details["y"], 0)
        self.assertEqual(raised.exception.details["channel"], "R")


if __name__ == "__main__":
    unittest.main()
