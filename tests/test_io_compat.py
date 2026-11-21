from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from vpipe.compat import compare_geometry
from vpipe.errors import (
    E_PARSE_INVALID,
    E_SHARE_INVALID,
    E_VERSION_LOSS,
    PipelineError,
)
from vpipe.io import export_path, load_path, parse_path, serialize_v1
from vpipe.samplegen import ensure_samples


class IoCompatibilityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory()
        cls.samples = Path(cls._temporary.name) / "samples"
        ensure_samples(cls.samples)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def test_v1_infers_and_corrects_hole_direction(self) -> None:
        path = load_path(self.samples / "v1" / "base_a.json")
        self.assertEqual([item.direction for item in path.contours], ["ccw", "cw"])
        self.assertFalse(path.contours[0].orientation_corrected)
        self.assertTrue(path.contours[1].orientation_corrected)
        self.assertEqual(
            path.contours[1].direction_source,
            "v1_order_containment",
        )

    def test_v2_has_explicit_groups_directions_and_shares(self) -> None:
        path = load_path(self.samples / "v2" / "base_a.json")
        self.assertEqual(path.version, 2)
        self.assertEqual(path.tolerance_source, "v2_declared")
        self.assertEqual([item.group for item in path.contours], ["body", "cutout"])
        self.assertTrue(all(item.direction_source == "v2_explicit" for item in path.contours))
        shared = load_path(self.samples / "v2" / "shared.json")
        self.assertEqual(len(shared.vertex_shares), 1)
        self.assertEqual(len(shared.vertex_shares[0].refs), 2)

    def test_unclosed_contour_is_parse_invalid(self) -> None:
        payload = json.loads((self.samples / "v1" / "base_a.json").read_text())
        payload["contours"][0]["segments"][-1]["to"] = [1, 1]
        with self.assertRaises(PipelineError) as raised:
            parse_path(payload)
        self.assertEqual(raised.exception.code, E_PARSE_INVALID)
        self.assertEqual(raised.exception.details.get("contour_index"), 0)

    def test_dangling_group_is_parse_invalid(self) -> None:
        payload = json.loads((self.samples / "v2" / "base_a.json").read_text())
        payload["contours"][0]["group"] = "missing-group"
        with self.assertRaises(PipelineError) as raised:
            parse_path(payload)
        self.assertEqual(raised.exception.code, E_PARSE_INVALID)
        self.assertEqual(raised.exception.details.get("field"), "group")

    def test_share_index_mismatch_locates_contour(self) -> None:
        payload = json.loads((self.samples / "v2" / "shared.json").read_text())
        payload["vertex_shares"][0]["refs"][1]["vertex"] = 99
        with self.assertRaises(PipelineError) as raised:
            parse_path(payload)
        self.assertEqual(raised.exception.code, E_SHARE_INVALID)
        self.assertEqual(raised.exception.details.get("contour_index"), 1)
        self.assertIn("vertex_count", raised.exception.details)

    def test_v2_downgrade_lists_direction_group_share_and_tolerance(self) -> None:
        path = load_path(self.samples / "v2" / "shared.json")
        payload, report = export_path(path, 1)
        _, direct_report = serialize_v1(path)
        self.assertEqual(payload["version"], 1)
        self.assertEqual(report.code, E_VERSION_LOSS)
        self.assertEqual(direct_report.code, E_VERSION_LOSS)
        fields = {item["field"] for item in report.affected}
        self.assertIn("direction", fields)
        self.assertIn("group", fields)
        self.assertIn("vertex_shares", fields)
        self.assertIn("tolerance", fields)
        self.assertEqual(report.tolerance, path.tolerance)

    def test_cross_version_geometry_is_consistent(self) -> None:
        for name in ("base_a", "nested", "near_a", "degenerate", "shared"):
            with self.subTest(name=name):
                v1 = load_path(self.samples / "v1" / f"{name}.json")
                v2 = load_path(self.samples / "v2" / f"{name}.json")
                comparison = compare_geometry(
                    v1,
                    v2,
                    max(v1.tolerance, v2.tolerance),
                )
                self.assertTrue(comparison["consistent"])
                self.assertLessEqual(
                    comparison["max_boundary_deviation"],
                    comparison["tolerance"] + 1e-12,
                )


if __name__ == "__main__":
    unittest.main()

