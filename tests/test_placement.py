from __future__ import annotations

import unittest
from dataclasses import replace

from tactical_pick.errors import (
    E_REGION_COVER_OVERLAP,
    E_REGION_INVALID,
    E_REGION_UNIT_OVERLAP,
    E_REGION_UNWALKABLE,
    PickError,
)
from tactical_pick.placement import check_placement
from tactical_pick.versioning import VersionStore

from tests.helpers import capsule_unit, make_grid, simple_scene


def rect(x0: float, y0: float, x1: float, y1: float):
    return [[x0, y0], [x1, y0], [x1, y1], [x0, y1]]


class PlacementTests(unittest.TestCase):
    def setUp(self) -> None:
        self.store = VersionStore(simple_scene())

    def test_valid_region_and_version_binding(self) -> None:
        before = self.store.current_snapshot()
        result = check_placement(self.store, rect(2.1, 2.1, 3.9, 3.9))
        self.assertTrue(result.valid)
        self.assertIsNone(result.failure_code)
        self.assertEqual(result.version, before.version)
        self.assertEqual(result.content_hash, before.content_hash)
        self.assertGreater(len(result.covered_cells), 0)
        self.assertEqual(self.store.current_snapshot(), before)

    def test_unwalkable_region_names_cell_and_vertex(self) -> None:
        result = check_placement(self.store, rect(0.05, 0.05, 0.95, 0.95))
        self.assertFalse(result.valid)
        self.assertEqual(result.failure_code, E_REGION_UNWALKABLE)
        self.assertEqual(result.conflict_entity, "grid:0,0")
        self.assertIsInstance(result.failed_vertex, int)

    def test_unit_overlap_names_unit(self) -> None:
        scene = replace(
            simple_scene(),
            grid=make_grid(5, 5, [(4, 0), (4, 4)]),
        )
        store = VersionStore(scene)
        result = check_placement(store, rect(0.7, 0.7, 1.3, 1.3))
        self.assertFalse(result.valid)
        self.assertEqual(result.failure_code, E_REGION_UNIT_OVERLAP)
        self.assertEqual(result.conflict_entity, "unit:10")
        self.assertIsInstance(result.failed_vertex, int)

    def test_capsule_overlap_names_unit(self) -> None:
        scene = simple_scene()
        capsule = capsule_unit(
            20,
            (2.4, 2.5, 0.3),
            (3.0, 2.5, 0.3),
            0.25,
        )
        store = VersionStore(replace(scene, units=(capsule,)))
        result = check_placement(store, rect(2.0, 2.0, 3.5, 3.0))
        self.assertFalse(result.valid)
        self.assertEqual(result.failure_code, E_REGION_UNIT_OVERLAP)
        self.assertEqual(result.conflict_entity, "unit:20")

    def test_cover_overlap_names_cover(self) -> None:
        result = check_placement(self.store, rect(0.2, 4.05, 1.2, 4.7))
        self.assertFalse(result.valid)
        self.assertEqual(result.failure_code, E_REGION_COVER_OVERLAP)
        self.assertEqual(result.conflict_entity, "cover:100")

    def test_self_intersection_and_bounds_are_region_invalid(self) -> None:
        with self.assertRaises(PickError) as caught:
            check_placement(
                self.store,
                [[2.0, 2.0], [3.0, 3.0], [2.0, 3.0], [3.0, 2.0]],
            )
        self.assertEqual(caught.exception.code, E_REGION_INVALID)
        self.assertEqual(caught.exception.location["vertex_index"], 2)

        with self.assertRaises(PickError) as caught:
            check_placement(
                self.store,
                [[-0.1, 1.0], [1.0, 1.0], [1.0, 2.0]],
            )
        self.assertEqual(caught.exception.code, E_REGION_INVALID)
        self.assertEqual(caught.exception.location["vertex_index"], 0)

    def test_placement_does_not_publish_on_failure(self) -> None:
        before = self.store.current_snapshot()
        result = check_placement(self.store, rect(0.05, 0.05, 0.95, 0.95))
        self.assertFalse(result.valid)
        self.assertEqual(self.store.current_snapshot(), before)
        self.assertEqual(self.store.versions(), (1,))


if __name__ == "__main__":
    unittest.main()
