from __future__ import annotations

import unittest

import numpy as np

from pcwb.errors import SelectionInvalid
from pcwb.model import Bounds, PointCloud, PointCloudSnapshot
from pcwb.selection import (
    Selection,
    select_screen_rectangle,
    select_sphere,
    select_world_rectangle,
    validate_selection,
)


def grid_cloud() -> PointCloud:
    xyz = np.asarray(
        [
            [-0.5, 0.5, 0.0],
            [0.5, 0.5, 0.0],
            [-0.5, -0.5, 0.0],
            [0.5, -0.5, 0.0],
            [0.0, 0.0, 0.0],
        ],
        dtype=np.float32,
    )
    rgb = np.zeros((5, 3), dtype=np.uint8)
    return PointCloud.from_arrays(xyz, rgb)


def snap(cloud: PointCloud | None = None) -> PointCloudSnapshot:
    value = cloud or grid_cloud()
    return PointCloudSnapshot(1, value.content_hash, value)


class SelectionTests(unittest.TestCase):
    def test_sphere_selection_reports_count_bounds_and_version(self) -> None:
        snapshot = snap()
        selection = select_sphere(snapshot, (0.0, 0.0, 0.0), 0.71)
        self.assertEqual(selection.indices, (0, 1, 2, 3, 4))
        self.assertEqual(selection.count, 5)
        self.assertEqual(selection.version_hash, snapshot.content_hash)
        self.assertEqual(selection.bounds, Bounds((-0.5, -0.5, 0.0), (0.5, 0.5, 0.0)))
        validate_selection(snapshot, selection)

    def test_world_rectangle_and_screen_rectangle(self) -> None:
        snapshot = snap()
        world = select_world_rectangle(snapshot, (-1.0, 0.0, -1.0), (1.0, 1.0, 1.0))
        self.assertEqual(world.indices, (0, 1, 4))
        screen = select_screen_rectangle(
            snapshot,
            np.eye(4),
            0.0,
            0.0,
            50.0,
            50.0,
            100,
            100,
        )
        self.assertEqual(screen.indices, (0, 4))

    def test_empty_selection_is_valid_but_has_no_bounds(self) -> None:
        snapshot = snap()
        selection = Selection.empty(snapshot)
        validate_selection(snapshot, selection)
        self.assertTrue(selection.is_empty)
        self.assertIsNone(selection.bounds)

    def test_selection_is_immutable_and_stale_selection_is_rejected(self) -> None:
        snapshot = snap()
        selection = select_sphere(snapshot, (0.0, 0.0, 0.0), 0.7)
        before = snapshot.cloud.xyz.copy()
        _ = selection.indices
        np.testing.assert_array_equal(snapshot.cloud.xyz, before)
        changed = PointCloud.from_arrays(
            snapshot.cloud.xyz + np.float32(1.0), snapshot.cloud.rgb
        )
        other = PointCloudSnapshot(2, changed.content_hash, changed)
        with self.assertRaises(SelectionInvalid):
            validate_selection(other, selection)

    def test_invalid_indices_are_rejected(self) -> None:
        snapshot = snap()
        with self.assertRaises(SelectionInvalid):
            Selection.from_indices(snapshot, [0, 99])
        with self.assertRaises(SelectionInvalid):
            select_sphere(snapshot, (0.0, 0.0, 0.0), 0.0)


if __name__ == "__main__":
    unittest.main()
