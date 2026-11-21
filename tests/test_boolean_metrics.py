from __future__ import annotations

import tempfile
import unittest
from unittest import mock
from pathlib import Path

from tests.helpers import holed_square, rectangle
from vpipe.boolean import BooleanOptions, boolean_paths, check_self_intersections
from vpipe.errors import E_EXPORT_DEGRADED, E_SELF_INTERSECT, PipelineError
from vpipe.io import load_path, parse_path
from vpipe.metrics import ErrorThresholds, evaluate_error
from vpipe.raster import RasterSpec, rasterize_path
from vpipe.samplegen import ensure_samples
from vpipe.tasks import BooleanService


class BooleanMetricsTests(unittest.TestCase):
    def test_four_operations_on_overlapping_rectangles(self) -> None:
        first = rectangle(0, 0, 10, 10, "a")
        second = rectangle(5, 5, 15, 15, "b")
        expected = {
            "union": 175.0,
            "intersection": 25.0,
            "difference": 75.0,
            "xor": 150.0,
        }
        for operation, area in expected.items():
            with self.subTest(operation=operation):
                result = boolean_paths(
                    first,
                    second,
                    BooleanOptions(operation, 1e-5),
                )
                self.assertAlmostEqual(result.report["area"], area, places=6)
                self.assertEqual(result.report["self_intersections"], [])
                self.assertTrue(
                    all(
                        segment.trace
                        for contour in result.path.contours
                        for segment in contour.segments
                    )
                )

    def test_holes_keep_alternating_direction(self) -> None:
        first = holed_square()
        second = rectangle(15, 5, 40, 25, "b")
        result = boolean_paths(
            first,
            second,
            BooleanOptions("union", 1e-5),
        )
        self.assertGreaterEqual(len(result.path.contours), 2)
        directions = {contour.metadata["nesting_depth"]: contour.direction for contour in result.path.contours}
        self.assertEqual(directions[0], "ccw")
        self.assertEqual(directions[1], "cw")

    def test_tolerance_closes_near_coincident_gap(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            samples = Path(temporary) / "samples"
            ensure_samples(samples)
            first = load_path(samples / "v1" / "near_a.json")
            second = load_path(samples / "v1" / "near_b.json")
            tight = boolean_paths(
                first,
                second,
                BooleanOptions("union", 0.001),
            )
            loose = boolean_paths(
                first,
                second,
                BooleanOptions("union", 0.01),
            )
            self.assertEqual(tight.report["loop_count"], 2)
            self.assertEqual(loose.report["loop_count"], 1)
            self.assertAlmostEqual(loose.report["area"], 800.0, places=5)

    def test_self_intersection_detector(self) -> None:
        bowtie = parse_path(
            {
                "version": 1,
                "fill_rule": "nonzero",
                "contours": [
                    {
                        "id": "bowtie",
                        "start": [0, 0],
                        "segments": [
                            {"type": "line", "to": [10, 10]},
                            {"type": "line", "to": [10, 0]},
                            {"type": "line", "to": [0, 10]},
                            {"type": "line", "to": [0, 0]},
                        ],
                    }
                ],
            }
        )
        intersections = check_self_intersections(bowtie, 1e-6)
        self.assertEqual(len(intersections), 1)

    def test_metrics_and_degraded_export_guard(self) -> None:
        first = rectangle(0, 0, 10, 10, "a")
        second = rectangle(5, 5, 15, 15, "b")
        service = BooleanService()
        record = service.run_sync(
            first,
            second,
            "union",
            1e-5,
            ErrorThresholds(max_area_relative_error=0.0, max_boundary_error=0.0),
        )
        self.assertTrue(record.metrics.degraded)
        self.assertFalse(record.metrics.exportable)
        with self.assertRaises(PipelineError) as raised:
            service.export_result(record, 2)
        self.assertEqual(raised.exception.code, E_EXPORT_DEGRADED)

    def test_raster_is_byte_deterministic(self) -> None:
        path = holed_square()
        spec = RasterSpec.auto([path], max_side=96)
        first = rasterize_path(path, spec, 1e-6)
        second = rasterize_path(path, spec, 1e-6)
        self.assertEqual(first.mask, second.mask)
        self.assertEqual(first.rgba, second.rgba)
        self.assertEqual(first.raster_hash, second.raster_hash)

    def test_self_intersection_failure_keeps_last_committed_result(self) -> None:
        first = rectangle(0, 0, 10, 10, "a")
        second = rectangle(5, 5, 15, 15, "b")
        service = BooleanService()
        committed = service.run_sync(first, second, "union", 1e-5)
        with mock.patch(
            "vpipe.tasks.boolean_paths",
            side_effect=PipelineError(E_SELF_INTERSECT, "forced"),
        ):
            with self.assertRaises(PipelineError):
                service.run_sync(first, second, "difference", 1e-5)
        self.assertEqual(service.store.latest().result_hash, committed.result_hash)
        snapshot, frame = service.store.snapshot()
        self.assertEqual(snapshot.result_hash, committed.result_hash)
        self.assertEqual(frame.relevant_result_hash, committed.result_hash)


if __name__ == "__main__":
    unittest.main()

