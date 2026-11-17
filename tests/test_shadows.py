from __future__ import annotations

import copy
import unittest

import numpy as np

from serene_render.scene import make_sample_scene, parse_scene
from serene_render.shadows import (
    combined_shadow,
    penumbra_width,
    projection_report,
    soft_coverage_from_signed_distance,
)


class ShadowTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = parse_scene(make_sample_scene())
        self.report = projection_report(self.scene)

    def test_projection_points_recompute_from_report(self) -> None:
        for case in self.report["cases"]:
            for triangle in case["projected_triangles"]:
                if not triangle["valid"]:
                    continue
                if case["target_kind"] == "layer":
                    target = self.scene.layer_by_index(case["target_layer_index"])
                    for landing in triangle["landing_points"]:
                        point = np.asarray(landing, dtype=np.float64)
                        self.assertAlmostEqual(
                            float(np.dot(point - target.plane_point, target.normal)),
                            0.0,
                            places=8,
                        )
                else:
                    for landing in triangle["landing_points"]:
                        self.assertAlmostEqual(
                            float(landing[1]), self.scene.ground.y, places=8
                        )

    def test_projection_shift_tracks_layer_distance_and_direction(self) -> None:
        raw = copy.deepcopy(self.scene.raw)
        for layer in raw["layers"]:
            layer["vertices"] = [
                [x, y, z * 2.0] for x, y, z in layer["vertices"]
            ]
            layer["thickness"] = 0.36
        shifted = parse_scene(raw)
        shifted_report = projection_report(shifted)
        first = self.report["cases"][0]["projected_triangles"][0]["landing_points"][0]
        moved = shifted_report["cases"][0]["projected_triangles"][0]["landing_points"][0]
        self.assertGreater(abs(float(moved[2] - first[2])), 0.1)
        self.assertNotAlmostEqual(float(moved[0]), float(first[0]), places=6)

    def test_penumbra_width_increases_with_both_distances(self) -> None:
        widths = [
            item["penumbra_width"]
            for item in self.report["penumbra_trend"]["receiver_distance_cases"]
        ]
        light_widths = [
            item["penumbra_width"]
            for item in self.report["penumbra_trend"]["light_distance_cases"]
        ]
        self.assertTrue(all(a < b for a, b in zip(widths, widths[1:])))
        self.assertTrue(
            all(a < b for a, b in zip(light_widths, light_widths[1:]))
        )
        self.assertLess(
            penumbra_width(self.scene, 0.05, 8.0),
            penumbra_width(self.scene, 0.10, 12.0),
        )

    def test_soft_edge_is_monotonic_and_has_no_hard_single_step(self) -> None:
        width = penumbra_width(self.scene, 0.2, 12.0)
        sd = np.linspace(-abs(width), abs(width), 500)
        first = soft_coverage_from_signed_distance(
            sd, width, self.scene.shadow.sample_density
        )
        second = soft_coverage_from_signed_distance(
            sd, width, self.scene.shadow.sample_density
        )
        np.testing.assert_array_equal(first, second)
        self.assertGreaterEqual(float(first[0]), 0.99)
        self.assertLessEqual(float(first[-1]), 0.01)
        self.assertTrue(bool(np.all(np.diff(first) <= 1.0e-12)))
        self.assertLess(float(np.max(np.abs(np.diff(first)))), 0.10)

    def test_rendered_silk_shadow_edges_are_resolved_soft(self) -> None:
        from serene_render.renderer import Renderer

        renderer = Renderer(self.scene)
        for raster in renderer.rasters:
            if not raster.layer.is_silk:
                continue
            shadow = np.full(
                (self.scene.height, self.scene.width), np.nan, dtype=np.float64
            )
            shadow[raster.mask] = combined_shadow(
                self.scene, raster.world[raster.mask]
            )[0]
            neighbor_differences = []
            for dy, dx in ((0, 1), (1, 0)):
                a = shadow[: self.scene.height - dy, : self.scene.width - dx]
                b = shadow[dy:, dx:]
                valid = np.isfinite(a) & np.isfinite(b)
                neighbor_differences.append(np.abs(a - b)[valid])
            values = np.concatenate(neighbor_differences)
            if values.size:
                self.assertLess(float(values.max()), 0.25)
                self.assertLess(float(np.percentile(values, 99)), 0.20)

    def test_out_of_bounds_ground_projection_is_ignored(self) -> None:
        raw = copy.deepcopy(self.scene.raw)
        raw["ground"]["bounds"] = [-0.02, 0.02, -0.02, 0.02]
        scene = parse_scene(raw)
        report = projection_report(scene)
        ground_cases = [
            case for case in report["cases"] if case["target_kind"] == "ground"
        ]
        self.assertTrue(ground_cases)
        for case in ground_cases:
            self.assertEqual(
                sum(
                    1
                    for triangle in case["projected_triangles"]
                    if triangle["all_vertices_in_bounds"]
                ),
                0,
            )
            for triangle in case["projected_triangles"]:
                self.assertFalse(triangle["all_vertices_in_bounds"])
        points = np.array([[1.0, scene.ground.y, 0.0]], dtype=np.float64)
        shadow, _ = combined_shadow(scene, points, ground=True)
        self.assertEqual(float(shadow[0]), 0.0)


if __name__ == "__main__":
    unittest.main()
