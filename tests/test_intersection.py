from __future__ import annotations

import unittest

from vpipe.intersect import compare_intersection_results, intersect_segments
from vpipe.model import Point, Segment


def point(x: float, y: float) -> Point:
    return Point(x, y)


class IntersectionTests(unittest.TestCase):
    def test_line_crossing_reports_parameters_and_tangents(self) -> None:
        first = Segment.line(point(0, 0), point(10, 10))
        second = Segment.line(point(0, 10), point(10, 0))
        result = intersect_segments(first, second, 1e-5)
        self.assertEqual(result.status, "intersect")
        self.assertEqual(len(result.hits), 1)
        self.assertAlmostEqual(result.hits[0].t1, 0.5)
        self.assertAlmostEqual(result.hits[0].t2, 0.5)
        self.assertGreater(result.hits[0].confidence, 0.0)

    def test_quadratic_tangency(self) -> None:
        line = Segment.line(point(0, 0), point(10, 0))
        quadratic = Segment.quadratic(point(0, 5), point(5, -5), point(10, 5))
        result = intersect_segments(line, quadratic, 1e-4)
        self.assertEqual(result.status, "tangent")
        self.assertEqual(result.hits[0].kind, "tangent")

    def test_cubic_curve_intersection(self) -> None:
        horizontal = Segment.line(point(0, 3), point(10, 3))
        cubic = Segment.cubic(
            point(0, 0),
            point(3, 8),
            point(7, -4),
            point(10, 5),
        )
        result = intersect_segments(horizontal, cubic, 1e-4)
        self.assertEqual(result.status, "intersect")
        self.assertGreaterEqual(len(result.hits), 1)

    def test_tolerance_band_marks_uncertain_instead_of_intersect(self) -> None:
        first = Segment.line(point(0, 0), point(10, 0))
        second = Segment.line(point(0, 0.015), point(10, 0.015))
        result = intersect_segments(first, second, 0.01)
        self.assertEqual(result.status, "uncertain")
        self.assertTrue(all(hit.kind == "uncertain" for hit in result.hits))

    def test_tolerance_change_produces_difference_summary(self) -> None:
        first = Segment.line(point(0, 0), point(10, 0))
        second = Segment.line(point(0, 0.004), point(10, 0.004))
        old = intersect_segments(first, second, 0.001)
        new = intersect_segments(first, second, 0.01)
        self.assertEqual(old.status, "disjoint")
        self.assertEqual(new.status, "coincident")
        difference = compare_intersection_results(old, new)
        self.assertTrue(difference["status_changed"])
        self.assertEqual(difference["added_count"], 1)
        self.assertEqual(difference["max_position_deviation"], 0.0)


if __name__ == "__main__":
    unittest.main()

