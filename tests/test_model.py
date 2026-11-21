from __future__ import annotations

import copy
import unittest

from surface_viewer.errors import ErrorCode, SurfaceViewerError
from surface_viewer.model import parse_assembly
from surface_viewer.samplegen import CASE_SPECS, generate_case


class SurfaceModelTests(unittest.TestCase):
    def setUp(self) -> None:
        self.valid = generate_case(CASE_SPECS[1])

    def test_valid_model_records_stable_patch_and_assembly_versions(self) -> None:
        first = parse_assembly(self.valid)
        second = parse_assembly(copy.deepcopy(self.valid))
        self.assertEqual(first.version_hash, second.version_hash)
        self.assertEqual(
            first.patches[0].parameter_versions,
            second.patches[0].parameter_versions,
        )

    def test_missing_field_returns_field_location(self) -> None:
        data = copy.deepcopy(self.valid)
        del data["patches"][1]["flip_normal"]
        with self.assertRaises(SurfaceViewerError) as raised:
            parse_assembly(data)
        self.assertEqual(raised.exception.code, ErrorCode.SURF_INVALID)
        self.assertEqual(raised.exception.field, "patches[1].flip_normal")

    def test_ragged_control_grid_is_rejected(self) -> None:
        data = copy.deepcopy(self.valid)
        data["patches"][0]["control_points"][2].pop()
        with self.assertRaises(SurfaceViewerError) as raised:
            parse_assembly(data)
        self.assertEqual(raised.exception.code, ErrorCode.SURF_INVALID)
        self.assertIn("control_points[2]", raised.exception.field)
        self.assertIn("inconsistent", raised.exception.message)

    def test_precision_out_of_range_is_rejected(self) -> None:
        data = copy.deepcopy(self.valid)
        data["patches"][2]["precision_level"] = 99
        with self.assertRaises(SurfaceViewerError) as raised:
            parse_assembly(data)
        self.assertEqual(raised.exception.code, ErrorCode.SURF_INVALID)
        self.assertEqual(
            raised.exception.field, "patches[2].precision_level"
        )

    def test_duplicate_block_id_is_rejected(self) -> None:
        data = copy.deepcopy(self.valid)
        data["patches"][3]["block_id"] = data["patches"][0]["block_id"]
        with self.assertRaises(SurfaceViewerError) as raised:
            parse_assembly(data)
        self.assertEqual(raised.exception.code, ErrorCode.SURF_INVALID)
        self.assertEqual(raised.exception.field, "patches[3].block_id")

    def test_parameter_change_changes_only_that_parameter_version(self) -> None:
        assembly = parse_assembly(self.valid)
        old = assembly.patch("p0001")
        control = [
            [list(point) for point in row] for row in old.control_points
        ]
        control[1][1][2] += 0.1
        changed = assembly.apply_changes(
            "p0001", {"control_points": control}
        ).patch("p0001")
        old_versions = old.parameter_versions
        new_versions = changed.parameter_versions
        self.assertNotEqual(
            old_versions["control_points"], new_versions["control_points"]
        )
        for name in ("surface_type", "precision_level", "flip_normal"):
            self.assertEqual(old_versions[name], new_versions[name])


if __name__ == "__main__":
    unittest.main()
