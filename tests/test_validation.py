from __future__ import annotations

import copy
import unittest

from bamboo_render.errors import SceneError
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import validate_scene


class SceneValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.raw = make_sample_scene(
            image_width=32,
            image_height=24,
            chunk_width=16,
            chunk_height=12,
        )

    def test_valid_scene_sorts_layers_and_derives_gaps(self) -> None:
        scene = validate_scene(copy.deepcopy(self.raw))
        self.assertEqual(scene.layer_numbers, (1, 2, 3))
        self.assertAlmostEqual(scene.layers[1].plane_z - scene.layers[0].plane_z, 0.22)
        self.assertAlmostEqual(scene.layers[2].plane_z - scene.layers[1].plane_z, 0.26)

    def test_rejects_mesh_index_out_of_range_with_layer(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["layers"][1]["mesh"]["triangles"][3][2] = 99999
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "MESH_INDEX_OUT_OF_RANGE")
        self.assertEqual(context.exception.layer, 2)
        self.assertIn("triangle_index", context.exception.details)

    def test_rejects_degenerate_triangle(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["layers"][0]["mesh"]["triangles"][0] = [0, 1, 1]
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "DEGENERATE_TRIANGLE")
        self.assertEqual(context.exception.layer, 1)

    def test_rejects_duplicate_layer_number(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["layers"][1]["layer"] = 1
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "DUPLICATE_LAYER_NUMBER")
        self.assertEqual(context.exception.layer, 1)

    def test_rejects_transmittance_out_of_range(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["layers"][2]["liner"]["transmittance"] = 1.1
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "TRANSMITTANCE_OUT_OF_RANGE")
        self.assertEqual(context.exception.layer, 3)

    def test_rejects_reflectance_out_of_range(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["inner_wall"]["reflectance"] = -0.1
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "REFLECTANCE_OUT_OF_RANGE")

    def test_rejects_light_inside_layer_strand_volume(self) -> None:
        raw = copy.deepcopy(self.raw)
        vertex = raw["layers"][1]["mesh"]["vertices"][0]
        raw["light"]["position"] = list(vertex)
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "LIGHT_INSIDE_LAYER_GEOMETRY")
        self.assertEqual(context.exception.layer, 2)

    def test_rejects_invalid_camera_up(self) -> None:
        raw = copy.deepcopy(self.raw)
        raw["camera"]["up"] = [-0.55, 0.40, 4.95]
        with self.assertRaises(SceneError) as context:
            validate_scene(raw)
        self.assertEqual(context.exception.code, "CAMERA_INVALID")


if __name__ == "__main__":
    unittest.main()
