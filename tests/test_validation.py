from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from shadowbox.errors import SceneValidationError
from shadowbox.scene import load_scene

from tests.helpers import invoke_cli, small_scene, write_scene_dict


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.scene = small_scene()
        self.scene_path = self.directory / "scene.json"
        write_scene_dict(self.scene_path, self.scene)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def assert_issue(self, scene: dict, code: str, layer_index: int) -> None:
        write_scene_dict(self.scene_path, scene)
        with self.assertRaises(SceneValidationError) as context:
            load_scene(self.scene_path)
        matches = [
            issue
            for issue in context.exception.issues
            if issue.code == code and issue.layer_index == layer_index
        ]
        self.assertTrue(matches, context.exception.issues)

    def test_mesh_index_out_of_range(self) -> None:
        self.scene["layers"][0]["mesh"]["triangles"][3] = [0, 1, 9999]
        self.assert_issue(self.scene, "MESH_INDEX_OUT_OF_RANGE", 0)

    def test_degenerate_triangle(self) -> None:
        self.scene["layers"][2]["mesh"]["triangles"][1] = [4, 4, 5]
        self.assert_issue(self.scene, "DEGENERATE_TRIANGLE", 2)

    def test_transmittance_out_of_range(self) -> None:
        self.scene["layers"][3]["transmittance"] = 1.25
        self.assert_issue(self.scene, "TRANSMITTANCE_OUT_OF_RANGE", 3)

    def test_duplicate_layer_index(self) -> None:
        self.scene["layers"][1]["layer_index"] = 0
        self.assert_issue(self.scene, "DUPLICATE_LAYER_INDEX", 0)

    def test_missing_layer_index(self) -> None:
        del self.scene["layers"][2]
        self.assert_issue(self.scene, "MISSING_LAYER_INDEX", 2)

    def test_light_inside_layer_geometry(self) -> None:
        self.scene["light"]["position_mm"] = [0.0, 0.0, 24.0]
        self.assert_issue(self.scene, "LIGHT_INSIDE_LAYER_GEOMETRY", 0)

    def test_invalid_scene_writes_no_image(self) -> None:
        self.scene["layers"][4]["mesh"]["triangles"][0] = [0, 1, 500]
        write_scene_dict(self.scene_path, self.scene)
        output = self.directory / "rejected"
        code, stdout, stderr = invoke_cli(
            ["render", "--scene", str(self.scene_path), "--out", str(output)]
        )
        self.assertEqual(code, 2)
        self.assertEqual(stdout, "")
        payload = json.loads(stderr)
        self.assertFalse(payload["ok"])
        self.assertIn(
            "MESH_INDEX_OUT_OF_RANGE",
            [error["code"] for error in payload["errors"]],
        )
        self.assertTrue(
            any(
                error["layer_index"] == 4
                and error["code"] == "MESH_INDEX_OUT_OF_RANGE"
                for error in payload["errors"]
            )
        )
        self.assertFalse((output / "rgba.png").exists())
        self.assertFalse((output / "rgba.raw").exists())

    def test_valid_scene_has_five_layers_and_derived_z(self) -> None:
        scene = load_scene(self.scene_path)
        self.assertEqual([layer.layer_index for layer in scene.layers], [0, 1, 2, 3, 4])
        self.assertEqual(
            [layer.world_z_mm for layer in scene.layers],
            [24.0, 28.0, 34.0, 42.0, 52.0],
        )


if __name__ == "__main__":
    unittest.main()
