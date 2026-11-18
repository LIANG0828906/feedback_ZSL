from __future__ import annotations

import copy
import unittest
from pathlib import Path

import numpy as np

from paper_lantern.generator import generate_scene
from paper_lantern.pipeline import render_state_fresh
from paper_lantern.render import RenderEngine
from paper_lantern.report import build_success_report
from paper_lantern.scene import validate_scene_data
from paper_lantern.state import build_output_artifacts


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene_data = generate_scene(
            width=64,
            height=48,
            tile_width=32,
            tile_height=24,
            shadow_samples=8,
        )
        cls.scene = validate_scene_data(cls.scene_data)

    def test_complete_render_is_deterministic(self) -> None:
        first = build_output_artifacts(self.scene, render_state_fresh(self.scene))
        second = build_output_artifacts(self.scene, render_state_fresh(self.scene))
        self.assertEqual(first.payloads, second.payloads)
        self.assertEqual(first.digests, second.digests)

    def test_report_is_stable_and_has_no_time_or_absolute_path(self) -> None:
        first_state = render_state_fresh(self.scene)
        second_state = render_state_fresh(self.scene)
        first = build_success_report(
            self.scene,
            first_state,
            build_output_artifacts(self.scene, first_state),
            "none",
        )
        second = build_success_report(
            self.scene,
            second_state,
            build_output_artifacts(self.scene, second_state),
            "none",
        )
        self.assertEqual(
            list(first.keys()),
            [
                "status",
                "exit_code",
                "scene_digest",
                "image",
                "layers",
                "soft_shadow",
                "tiles",
                "textures",
                "recovery_count",
                "snapshot_status",
            ],
        )
        self.assertEqual(first, second)
        serialized = repr(first)
        self.assertNotIn("timestamp", serialized)
        self.assertNotIn(str(Path.cwd()), serialized)

    def test_layer_statistics_and_premultiplied_output(self) -> None:
        state = render_state_fresh(self.scene)
        artifacts = build_output_artifacts(self.scene, state)
        self.assertEqual(state.visible_fragments.shape, (5,))
        self.assertTrue(np.all(state.visible_fragments > 0))
        self.assertTrue(np.all(state.paper_fragments > 0))
        self.assertTrue(np.all(state.effective_transmission_sum >= 0.0))
        self.assertTrue(np.all(artifacts.rgba_u8[..., :3] <= artifacts.rgba_u8[..., 3:4]))
        self.assertTrue(np.all(np.isfinite(artifacts.depth_f32)))
        self.assertTrue(np.all(artifacts.depth_f32 >= 0.0))

    def test_input_layer_order_does_not_change_pixels(self) -> None:
        reordered = copy.deepcopy(self.scene_data)
        reordered["layers"] = list(reversed(reordered["layers"]))
        reordered_scene = validate_scene_data(reordered)
        baseline = build_output_artifacts(self.scene, render_state_fresh(self.scene))
        changed = build_output_artifacts(
            reordered_scene, render_state_fresh(reordered_scene)
        )
        self.assertEqual(baseline.payloads["image.rgba"], changed.payloads["image.rgba"])
        self.assertEqual(baseline.payloads["depth.f32"], changed.payloads["depth.f32"])

    def test_two_sided_shading_is_orientation_independent(self) -> None:
        flipped_data = copy.deepcopy(self.scene_data)
        for layer in flipped_data["layers"]:
            layer["mesh"]["triangles"] = [
                [triangle[0], triangle[2], triangle[1]]
                for triangle in layer["mesh"]["triangles"]
            ]
        flipped_scene = validate_scene_data(flipped_data)
        baseline = build_output_artifacts(self.scene, render_state_fresh(self.scene))
        flipped = build_output_artifacts(
            flipped_scene, render_state_fresh(flipped_scene)
        )
        self.assertEqual(baseline.payloads["image.rgba"], flipped.payloads["image.rgba"])
        self.assertEqual(baseline.payloads["depth.f32"], flipped.payloads["depth.f32"])

    def test_inner_cutouts_project_variable_light_onto_outer_layer(self) -> None:
        engine = RenderEngine(self.scene)
        outer_position = len(self.scene.layers) - 1
        outer_layer = self.scene.layers[outer_position]
        points = outer_layer.mesh.vertices[outer_layer.mesh.triangles].mean(axis=1)
        visibility = engine.visibility_at(points, outer_position)
        self.assertGreater(float(np.max(visibility) - np.min(visibility)), 0.3)
        repeat = engine.visibility_at(points, outer_position)
        self.assertTrue(np.array_equal(visibility, repeat))
        self.assertGreater(float(np.max(visibility)), 0.55)
        self.assertLess(float(np.min(visibility)), 0.2)

    def test_softness_controls_shadow_transition(self) -> None:
        hard_data = copy.deepcopy(self.scene_data)
        hard_data["shadow"]["softness"] = 0.0
        soft_data = copy.deepcopy(self.scene_data)
        soft_data["shadow"]["softness"] = 0.12
        hard_scene = validate_scene_data(hard_data)
        soft_scene = validate_scene_data(soft_data)
        hard_engine = RenderEngine(hard_scene)
        soft_engine = RenderEngine(soft_scene)
        outer_position = len(self.scene.layers) - 1
        outer_layer = self.scene.layers[outer_position]
        points = outer_layer.mesh.vertices[outer_layer.mesh.triangles].mean(axis=1)
        hard = hard_engine.visibility_at(points, outer_position)
        soft = soft_engine.visibility_at(points, outer_position)
        self.assertFalse(np.array_equal(hard, soft))
        self.assertLess(float(np.std(soft)), float(np.std(hard)))

    def test_projection_missing_inner_sheet_is_unoccluded(self) -> None:
        scene_data = generate_scene(
            width=32,
            height=24,
            tile_width=16,
            tile_height=12,
            layer_count=2,
            shadow_samples=4,
        )
        small_triangle = {
            "vertices": [
                [-0.2, -0.2, 0.0],
                [0.2, -0.2, 0.0],
                [0.2, 0.2, 0.0],
                [-0.2, 0.2, 0.0],
            ],
            "triangles": [[0, 1, 2], [0, 2, 3]],
            "uvs": [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]],
        }
        large_triangle = {
            "vertices": [
                [-1.0, -1.0, 1.0],
                [1.0, -1.0, 1.0],
                [1.0, 1.0, 1.0],
                [-1.0, 1.0, 1.0],
            ],
            "triangles": [[0, 1, 2], [0, 2, 3]],
            "uvs": [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0], [0.0, 1.0]],
        }
        scene_data["layers"][0]["mesh"] = small_triangle
        scene_data["layers"][1]["mesh"] = large_triangle
        scene_data["light"]["position"] = [0.0, 0.0, -1.0]
        scene = validate_scene_data(scene_data)
        engine = RenderEngine(scene)
        intensity = engine.visibility_at(
            np.asarray([[0.0, 0.0, 1.0], [1.0, 0.5, 1.0]]), 1
        )
        self.assertLess(float(intensity[0]), 0.9)
        self.assertGreater(float(intensity[1]), 0.999)


if __name__ == "__main__":
    unittest.main()

