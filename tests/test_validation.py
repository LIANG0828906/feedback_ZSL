from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from leather_raster.errors import SceneValidationError
from leather_raster.sample import make_sample_scene
from leather_raster.scene import validate_scene_object

from .support import first_solid_triangle, run_cli


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = make_sample_scene(width=48, height=36, block_width=24, block_height=18)

    def assert_issue(self, scene: dict, code: str, layer_index: int | None = None, path_fragment: str | None = None) -> None:
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene_object(scene)
        matching = [
            issue
            for issue in caught.exception.issues
            if issue.code == code
            and (layer_index is None or issue.layer_index == layer_index)
            and (path_fragment is None or path_fragment in issue.path)
        ]
        self.assertTrue(matching, msg=[issue.to_dict() for issue in caught.exception.issues])
        self.assertTrue(all(issue.path for issue in caught.exception.issues))

    def test_valid_sample(self) -> None:
        scene = validate_scene_object(self.scene)
        self.assertEqual(len(scene.layers), 4)
        self.assertEqual(len(scene.lights), 3)
        self.assertTrue(scene.digest)

    def test_mesh_index_out_of_range(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][1]["mesh"]["triangles"][7][2] = 999999
        self.assert_issue(scene, "MESH_INDEX_OUT_OF_RANGE", layer_index=1, path_fragment="triangles[7]")

    def test_degenerate_triangle(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][2]["mesh"]["triangles"][3] = [4, 4, 4]
        self.assert_issue(scene, "DEGENERATE_TRIANGLE", layer_index=2, path_fragment="triangles[3]")

    def test_transmittance_out_of_range(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][0]["transmittance"] = 1.25
        self.assert_issue(
            scene,
            "VALUE_OUT_OF_RANGE",
            layer_index=0,
            path_fragment="$.layers[0].transmittance",
        )

    def test_duplicate_layer_index(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][1]["layer_index"] = 0
        self.assert_issue(scene, "DUPLICATE_LAYER_INDEX", layer_index=0, path_fragment="$.layers")

    def test_missing_layer_index(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][2]["layer_index"] = 5
        self.assert_issue(scene, "MISSING_LAYER_INDEX", layer_index=2, path_fragment="$.layers")

    def test_duplicate_cutout_id(self) -> None:
        scene = copy.deepcopy(self.scene)
        duplicate = copy.deepcopy(scene["layers"][0]["cutouts"][0])
        duplicate["triangle_indices"] = []
        scene["layers"][0]["cutouts"].append(duplicate)
        self.assert_issue(scene, "DUPLICATE_CUTOUT_ID", layer_index=0, path_fragment="cutouts")

    def test_missing_cutout_id(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][3]["cutouts"] = [
            cutout for cutout in scene["layers"][3]["cutouts"] if cutout["cutout_id"] != 2
        ]
        self.assert_issue(scene, "MISSING_CUTOUT_ID", layer_index=3, path_fragment="$.layers[3].cutouts")

    def test_light_inside_geometry(self) -> None:
        scene = copy.deepcopy(self.scene)
        a, b, c = first_solid_triangle(scene)
        vertices = scene["layers"][0]["mesh"]["vertices"]
        centroid = [
            (vertices[a][axis] + vertices[b][axis] + vertices[c][axis]) / 3.0
            for axis in range(3)
        ]
        scene["lights"][0]["position"] = centroid
        self.assert_issue(
            scene,
            "LIGHT_INSIDE_GEOMETRY",
            layer_index=0,
            path_fragment="$.lights[0].position",
        )

    def test_invalid_camera_and_screen(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["camera"]["fov_degrees"] = 0
        scene["camera"]["up"] = [0.0, 0.0, 1.0]
        scene["screen"]["v_axis"] = [1.0, 0.0, 0.0]
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene_object(scene)
        codes = {issue.code for issue in caught.exception.issues}
        self.assertIn("INVALID_CAMERA_BASIS", codes)
        self.assertIn("INVALID_SCREEN_BASIS", codes)

    def test_cli_validation_failure_is_structured_and_nonzero(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][0]["mesh"]["triangles"][0][0] = -4
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "bad.json"
            path.write_text(json.dumps(scene), encoding="utf-8")
            result = run_cli("validate", str(path))
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertFalse(payload["ok"])
            issue = payload["error"]["issues"][0]
            self.assertEqual(issue["code"], "MESH_INDEX_OUT_OF_RANGE")
            self.assertEqual(issue["layer_index"], 0)
            self.assertIn("$.layers[0].mesh.triangles[0]", issue["path"])


if __name__ == "__main__":
    unittest.main()

