from __future__ import annotations

import json
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

import numpy as np

from vector_workbench.errors import ErrorCode, WorkbenchError
from vector_workbench.generator import generate_samples
from vector_workbench.geometry import (
    flatten_path,
    point_to_polyline_distance,
    polygon_area,
)
from vector_workbench.model import RenderParams
from vector_workbench.raster import canvas_size, path_coverage
from vector_workbench.validation import load_scene

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "data"


class CoreTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = load_scene(
            DATA / "scenes" / "hud_orbit.json",
            DATA / "truth" / "truth_manifest.json",
        )
        cls.fill_path = next(path for path in cls.scene.paths if path.kind == "fill")
        cls.stroke_path = next(path for path in cls.scene.paths if path.kind == "stroke")

    def test_generation_is_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as first, tempfile.TemporaryDirectory() as second:
            generate_samples(first, 424242)
            generate_samples(second, 424242)
            first_files = sorted(
                path.relative_to(first)
                for path in Path(first).rglob("*")
                if path.is_file()
            )
            second_files = sorted(
                path.relative_to(second)
                for path in Path(second).rglob("*")
                if path.is_file()
            )
            self.assertEqual(first_files, second_files)
            for relative in first_files:
                self.assertEqual(
                    (Path(first) / relative).read_bytes(),
                    (Path(second) / relative).read_bytes(),
                    str(relative),
                )

    def test_decode_geometry_and_style_errors(self) -> None:
        original = json.loads(
            (DATA / "scenes" / "hud_orbit.json").read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            corrupt = root / "corrupt.json"
            corrupt.write_text('{"scene_id":', encoding="utf-8")
            with self.assertRaises(WorkbenchError) as raised:
                load_scene(corrupt)
            self.assertEqual(raised.exception.code, ErrorCode.E_DECODE)

            missing = json.loads(json.dumps(original))
            del missing["paths"][0]["style"]["opacity"]
            missing_path = root / "missing.json"
            missing_path.write_text(json.dumps(missing), encoding="utf-8")
            with self.assertRaises(WorkbenchError) as raised:
                load_scene(missing_path)
            self.assertEqual(raised.exception.code, ErrorCode.E_DECODE)

            unclosed = json.loads(json.dumps(original))
            unclosed["paths"][2]["segments"][-1]["end"][0] += 8
            unclosed_path = root / "unclosed.json"
            unclosed_path.write_text(json.dumps(unclosed), encoding="utf-8")
            with self.assertRaises(WorkbenchError) as raised:
                load_scene(unclosed_path)
            error = raised.exception
            self.assertEqual(error.code, ErrorCode.E_GEOM)
            self.assertEqual(error.path_id, unclosed["paths"][2]["id"])
            self.assertEqual(error.segment, len(unclosed["paths"][2]["segments"]) - 1)

            outside = json.loads(json.dumps(original))
            outside["paths"][0]["segments"][1]["c1"][0] = (
                outside["coord_bounds"][2] + 10
            )
            outside_path = root / "outside.json"
            outside_path.write_text(json.dumps(outside), encoding="utf-8")
            with self.assertRaises(WorkbenchError) as raised:
                load_scene(outside_path)
            error = raised.exception
            self.assertEqual(error.code, ErrorCode.E_GEOM)
            self.assertEqual(error.path_id, outside["paths"][0]["id"])
            self.assertEqual(error.segment, 1)

            bad_style = json.loads(json.dumps(original))
            bad_style["paths"][0]["style"]["opacity"] = 1.5
            bad_style_path = root / "bad_style.json"
            bad_style_path.write_text(json.dumps(bad_style), encoding="utf-8")
            with self.assertRaises(WorkbenchError) as raised:
                load_scene(bad_style_path)
            self.assertEqual(raised.exception.code, ErrorCode.E_DECODE)

    def test_flatten_parameter_and_truth_accuracy(self) -> None:
        tolerances = [1.0, 0.5, 0.25, 0.125, 0.0625, 0.03125]
        counts: list[int] = []
        for tolerance in tolerances:
            points = flatten_path(
                self.fill_path, self.scene.coord_scale, 1.0, tolerance
            )
            counts.append(len(points))
            self.assertFalse(np.array_equal(points[0], points[-1]))
            deltas = np.diff(points, axis=0)
            self.assertTrue(np.all(np.hypot(deltas[:, 0], deltas[:, 1]) > 1e-12))
        self.assertTrue(
            all(counts[index] >= counts[index - 1] for index in range(1, len(counts))),
            counts,
        )
        default = flatten_path(
            self.fill_path, self.scene.coord_scale, 1.0, 0.25
        )
        truth = self.scene.truth["paths"][self.fill_path.id]
        relative_error = abs(polygon_area(default) - truth["area"]) / truth["area"]
        self.assertLessEqual(relative_error, 0.01)
        for sample in truth["curve_samples"]:
            distance = point_to_polyline_distance(default, sample["point"])
            self.assertLessEqual(distance, 0.75)

        for field, scale, tolerance in (
            ("scale", 0.0, 0.25),
            ("scale", -1.0, 0.25),
            ("tolerance", 1.0, 0.0),
            ("tolerance", 1.0, -0.1),
        ):
            with self.assertRaises(WorkbenchError) as raised:
                if field == "scale":
                    flatten_path(self.fill_path, self.scene.coord_scale, scale, tolerance)
                else:
                    flatten_path(self.fill_path, self.scene.coord_scale, scale, tolerance)
            self.assertEqual(raised.exception.code, ErrorCode.E_PARA)
            self.assertEqual(raised.exception.field, field)

    def test_coverage_normalization_subsampling_and_join_modes(self) -> None:
        params = RenderParams()
        self.assertEqual(
            canvas_size(self.scene, params),
            tuple(self.scene.truth["expected_size_at_scale_1"]),
        )
        points = flatten_path(
            self.fill_path, self.scene.coord_scale, 1.0, 0.25
        )
        for subsamples in (1, 2, 3, 4, 5, 8):
            coverage = path_coverage(
                self.scene,
                self.fill_path,
                points,
                replace(params, subsamples=subsamples),
            )
            self.assertEqual(coverage.shape, canvas_size(self.scene, params)[::-1])
            self.assertGreaterEqual(float(coverage.min()), 0.0)
            self.assertLessEqual(float(coverage.max()), 1.0)
            self.assertEqual(coverage.dtype, np.float32)
            self.assertGreater(float(coverage.max()), 0.9)

        stroke_points = flatten_path(
            self.stroke_path, self.scene.coord_scale, 1.0, 0.25
        )
        for join in ("round", "bevel", "miter"):
            path = replace(
                self.stroke_path,
                style=replace(self.stroke_path.style, join=join),
            )
            coverage = path_coverage(self.scene, path, stroke_points, params)
            self.assertGreater(float(coverage.max()), 0.0, join)
            self.assertLessEqual(float(coverage.max()), 1.0, join)

        with self.assertRaises(WorkbenchError) as raised:
            path_coverage(
                self.scene,
                self.fill_path,
                points,
                replace(params, coverage_buffer_limit=1024),
            )
        self.assertEqual(raised.exception.code, ErrorCode.E_CLIP)

    def test_parameter_validation(self) -> None:
        invalid = (
            ("scale", replace(RenderParams(), scale=0.0)),
            ("tolerance", replace(RenderParams(), tolerance=0.0)),
            ("subsamples", replace(RenderParams(), subsamples=0)),
            ("max_parallelism", replace(RenderParams(), max_parallelism=0)),
            ("coverage_buffer_limit", replace(RenderParams(), coverage_buffer_limit=0)),
            ("max_stroke_pixels", replace(RenderParams(), max_stroke_pixels=0)),
        )
        for field, params in invalid:
            with self.assertRaises(WorkbenchError) as raised:
                params.validate()
            self.assertEqual(raised.exception.code, ErrorCode.E_PARA)
            self.assertEqual(raised.exception.field, field)


if __name__ == "__main__":
    unittest.main()

