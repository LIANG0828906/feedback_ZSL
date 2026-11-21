from __future__ import annotations

import unittest

import numpy as np

from pcwb.errors import OperatorInvalid
from pcwb.format import serialize_point_cloud
from pcwb.operators import run_operator
from pcwb.selection import Selection
from tests.helpers import all_selection, sample_cloud, snapshot


class OperatorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.cloud = sample_cloud(10)
        self.snapshot = snapshot(self.cloud)
        self.selection = all_selection(self.cloud)

    def test_all_operators_are_deterministic_pure_functions(self) -> None:
        cases = [
            ("crop", {"mode": "keep"}),
            ("smooth", {"radius": 0.4, "iterations": 2}),
            ("downsample", {"mode": "voxel", "voxel_size": 0.25}),
            ("downsample", {"mode": "stride", "stride": 2}),
            ("color_overwrite", {"rgb": [10, 20, 30]}),
        ]
        original = serialize_point_cloud(self.cloud)
        for operator, params in cases:
            with self.subTest(operator=operator, params=params):
                first = run_operator(self.snapshot, self.selection, operator, params)
                second = run_operator(self.snapshot, self.selection, operator, params)
                self.assertEqual(
                    serialize_point_cloud(first.cloud),
                    serialize_point_cloud(second.cloud),
                )
                self.assertEqual(first.record.output_hash, second.record.output_hash)
                self.assertEqual(first.record.input_version, 1)
                self.assertEqual(first.record.input_hash, self.snapshot.content_hash)
                self.assertEqual(serialize_point_cloud(self.cloud), original)

    def test_crop_keep_and_remove(self) -> None:
        selection = Selection.from_indices(self.snapshot, [1, 3, 5])
        keep = run_operator(self.snapshot, selection, "crop", {"mode": "keep"})
        self.assertEqual(keep.cloud.count, 3)
        remove = run_operator(self.snapshot, selection, "crop", {"mode": "remove"})
        self.assertEqual(remove.cloud.count, 7)
        np.testing.assert_array_equal(
            remove.cloud.xyz,
            self.cloud.xyz[[0, 2, 4, 6, 7, 8, 9]],
        )

    def test_smooth_changes_only_selected_points(self) -> None:
        selection = Selection.from_indices(self.snapshot, [3, 4])
        result = run_operator(
            self.snapshot,
            selection,
            "smooth",
            {"radius": 0.5, "iterations": 1},
        )
        outside = np.setdiff1d(np.arange(10), selection.index_array())
        np.testing.assert_array_equal(
            result.cloud.xyz[outside], self.cloud.xyz[outside]
        )
        self.assertFalse(
            np.array_equal(
                result.cloud.xyz[selection.index_array()],
                self.cloud.xyz[selection.index_array()],
            )
        )

    def test_downsample_keeps_outside_points_and_one_per_voxel(self) -> None:
        selection = Selection.from_indices(self.snapshot, [0, 1, 2, 3])
        result = run_operator(
            self.snapshot,
            selection,
            "downsample",
            {"mode": "voxel", "voxel_size": 0.5},
        )
        self.assertGreaterEqual(result.cloud.count, 6)
        self.assertLess(result.cloud.count, 10)

    def test_color_overwrite_preserves_xyz_and_header_bounds(self) -> None:
        selection = Selection.from_indices(self.snapshot, [2, 4])
        result = run_operator(
            self.snapshot, selection, "color_overwrite", {"rgb": [255, 0, 128]}
        )
        np.testing.assert_array_equal(result.cloud.xyz, self.cloud.xyz)
        np.testing.assert_array_equal(
            result.cloud.rgb[selection.index_array()],
            np.asarray([[255, 0, 128], [255, 0, 128]], dtype=np.uint8),
        )
        self.assertEqual(result.cloud.bounds, self.cloud.bounds)

    def test_empty_selection_and_bad_parameters_are_operator_errors(self) -> None:
        empty = Selection.empty(self.snapshot)
        with self.assertRaises(OperatorInvalid) as caught:
            run_operator(self.snapshot, empty, "crop", {"mode": "keep"})
        self.assertEqual(caught.exception.code, "E_OP_INVALID")

        invalid_cases = [
            ("crop", {"mode": "bad"}),
            ("smooth", {"radius": 0.0, "iterations": 1}),
            ("smooth", {"radius": 0.2, "iterations": 0}),
            ("downsample", {"mode": "voxel", "voxel_size": -1}),
            ("downsample", {"mode": "unknown"}),
            ("color_overwrite", {"rgb": [0, 1, 300]}),
            ("crop", {"mode": "keep", "extra": 1}),
        ]
        for operator, params in invalid_cases:
            with self.subTest(operator=operator, params=params):
                with self.assertRaises(OperatorInvalid) as caught:
                    run_operator(self.snapshot, self.selection, operator, params)
                self.assertEqual(caught.exception.code, "E_OP_INVALID")


if __name__ == "__main__":
    unittest.main()
