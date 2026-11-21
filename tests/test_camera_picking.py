from __future__ import annotations

import math
import unittest
from dataclasses import replace

from tactical_pick.camera import Camera
from tactical_pick.errors import E_RAY_INVALID, PickError
from tactical_pick.model import Cover, Scene
from tactical_pick.picking import pick_from_store, pick_snapshot
from tactical_pick.versioning import VersionStore, build_snapshot

from tests.helpers import aabb_unit, capsule_unit, center_camera, make_grid


def _scene(units=(), covers=(), vertices=()) -> Scene:
    return Scene(
        structure_version=1,
        ground_height=0.0,
        grid=make_grid(5, 5),
        vertices=tuple(vertices),
        units=tuple(units),
        covers=tuple(covers),
    )


class CameraPickingTests(unittest.TestCase):
    def test_center_ray_and_repeatability(self) -> None:
        camera = center_camera()
        ray = camera.screen_to_ray([400, 300])
        self.assertAlmostEqual(ray.origin[0], 0.0)
        self.assertAlmostEqual(ray.direction[0], 0.0)
        self.assertAlmostEqual(ray.direction[1], 0.0)
        self.assertAlmostEqual(ray.direction[2], -1.0)
        ground = _scene()
        store = VersionStore(ground)
        first = pick_from_store(store, camera, [400, 300])
        second = pick_from_store(store, camera, [400, 300])
        self.assertEqual(first.to_dict(), second.to_dict())
        self.assertEqual(first.hit.kind, "ground")
        self.assertEqual(first.hit.point, (0.0, 0.0, 0.0))
        self.assertEqual(first.hit.distance, 5.0)

    def test_aabb_unit_is_nearest_hit(self) -> None:
        scene = _scene(units=(aabb_unit(10, (0.0, 0.0)),))
        snapshot = build_snapshot(scene, 2)
        result = pick_snapshot(snapshot, center_camera(), [400, 300])
        self.assertEqual(result.hit.kind, "unit")
        self.assertEqual(result.hit.entity_id, 10)
        self.assertEqual(result.hit.point, (0.0, 0.0, 1.5))
        self.assertEqual(result.hit.normal, (0.0, 0.0, 1.0))
        self.assertAlmostEqual(result.hit.distance or 0.0, 3.5)
        self.assertEqual(result.version, 2)

    def test_capsule_unit_hit(self) -> None:
        scene = _scene(
            units=(
                capsule_unit(
                    11,
                    (-0.4, 0.0, 0.5),
                    (0.4, 0.0, 0.5),
                    0.5,
                ),
            )
        )
        result = pick_snapshot(build_snapshot(scene, 1), center_camera(), [400, 300])
        self.assertEqual(result.hit.kind, "unit")
        self.assertEqual(result.hit.entity_id, 11)
        self.assertAlmostEqual(result.hit.point[2] if result.hit.point else 0.0, 1.0)

    def test_cover_is_closer_than_unit(self) -> None:
        vertices = (
            (-1.0, -1.0, 2.0),
            (1.0, -1.0, 2.0),
            (0.0, 1.0, 2.0),
        )
        scene = _scene(
            units=(aabb_unit(10, (0.0, 0.0)),),
            covers=(Cover(100, 0, 1, 2, "roof"),),
            vertices=vertices,
        )
        result = pick_snapshot(build_snapshot(scene, 1), center_camera(), [400, 300])
        self.assertEqual(result.hit.kind, "cover")
        self.assertEqual(result.hit.entity_id, 100)
        self.assertEqual(result.hit.point, (0.0, 0.0, 2.0))

    def test_camera_rejects_degenerate_intrinsics_and_extrinsics(self) -> None:
        with self.assertRaises(PickError) as caught:
            center_camera(fov=0.0).screen_to_ray([0, 0])
        self.assertEqual(caught.exception.code, E_RAY_INVALID)
        self.assertEqual(caught.exception.location["field"], "camera.fov_y_deg")
        with self.assertRaises(PickError) as caught:
            replace(center_camera(), target=(0.0, 0.0, 5.0)).screen_to_ray([400, 300])
        self.assertEqual(caught.exception.code, E_RAY_INVALID)
        self.assertEqual(caught.exception.location["field"], "camera.target")

    def test_screen_range_and_non_finite_screen_are_invalid(self) -> None:
        camera = center_camera()
        for screen in ([-1, 0], [801, 300], [400, 601], [math.nan, 300]):
            with self.assertRaises(PickError) as caught:
                camera.screen_to_ray(screen)
            self.assertEqual(caught.exception.code, E_RAY_INVALID)
            self.assertEqual(caught.exception.location["field"], "screen")

    def test_malformed_camera_dictionary_has_ray_business_code(self) -> None:
        with self.assertRaises(PickError) as caught:
            Camera.from_dict(
                {
                    "camera_id": "bad",
                    "position": [0.0, 0.0],
                    "target": [0.0, 0.0, 0.0],
                    "up": [0.0, 1.0, 0.0],
                    "fov_y_deg": 60.0,
                    "near": 0.1,
                    "far": 100.0,
                    "viewport_width": 800,
                    "viewport_height": 600,
                }
            )
        self.assertEqual(caught.exception.code, E_RAY_INVALID)
        self.assertEqual(caught.exception.location["field"], "camera")

    def test_ground_parallel_ray_returns_no_hit(self) -> None:
        camera = center_camera(
            position=(0.0, 0.0, 5.0),
            target=(0.0, 0.0, 6.0),
            up=(0.0, 1.0, 0.0),
        )
        result = pick_snapshot(build_snapshot(_scene(), 1), camera, [400, 300])
        self.assertEqual(result.hit.kind, "none")
        self.assertIsNone(result.hit.point)


if __name__ == "__main__":
    unittest.main()
