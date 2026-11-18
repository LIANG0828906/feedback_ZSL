from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from canopy_render.cli import main
from canopy_render.scene import generate_scene
from canopy_render.util import write_json_atomic
from canopy_render.validation import validate_scene


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = generate_scene(48, 36, 24, 18)

    def _issues(self, scene: dict) -> list:
        result = validate_scene(scene)
        self.assertFalse(result.ok)
        return result.issues

    def test_generated_scene_is_valid(self) -> None:
        result = validate_scene(self.scene)
        self.assertTrue(result.ok, [issue.as_dict() for issue in result.issues])
        self.assertEqual(len(result.data.layers), 3)
        self.assertEqual(len(result.data.lights), 3)
        self.assertEqual(len(result.data.tassels), 6)

    def test_mesh_index_out_of_range_has_location(self) -> None:
        scene = copy.deepcopy(self.scene)
        fold = scene["layers"][0]["folds"][0]
        fold["triangles"][0][0] = 999999
        issues = self._issues(scene)
        issue = next(item for item in issues if item.code == "MESH_INDEX_OUT_OF_RANGE")
        self.assertEqual(issue.layer_index, 1)
        self.assertEqual(issue.fold_id, fold["fold_id"])
        payload = issue.as_dict()
        self.assertEqual(
            list(payload),
            [
                "code",
                "layer_index",
                "fold_id",
                "light_id",
                "tassel_id",
                "message",
            ],
        )

    def test_degenerate_triangle_is_rejected(self) -> None:
        scene = copy.deepcopy(self.scene)
        fold = scene["layers"][1]["folds"][2]
        fold["triangles"][0] = [0, 0, 0]
        issues = self._issues(scene)
        issue = next(item for item in issues if item.code == "DEGENERATE_TRIANGLE")
        self.assertEqual(issue.layer_index, 2)
        self.assertEqual(issue.fold_id, fold["fold_id"])

    def test_transmittance_and_duplicate_ids_are_rejected(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][0]["transmittance"] = 1.25
        scene["layers"][1]["layer_index"] = scene["layers"][0]["layer_index"]
        scene["layers"][2]["folds"][0]["fold_id"] = scene["layers"][0]["folds"][0][
            "fold_id"
        ]
        codes = {issue.code for issue in self._issues(scene)}
        self.assertIn("TRANSMITTANCE_OUT_OF_RANGE", codes)
        self.assertIn("DUPLICATE_LAYER_INDEX", codes)
        self.assertIn("DUPLICATE_FOLD_ID", codes)

    def test_light_on_fabric_surface_is_rejected_with_layer_and_fold(self) -> None:
        scene = copy.deepcopy(self.scene)
        layer = scene["layers"][0]
        fold = layer["folds"][0]
        triangle = fold["triangles"][0]
        centroid = np.mean(
            np.asarray(
                [fold["vertices"][index] for index in triangle],
                dtype=np.float64,
            ),
            axis=0,
        )
        scene["lights"][0]["position"] = centroid.tolist()
        issues = self._issues(scene)
        issue = next(item for item in issues if item.code == "LIGHT_INSIDE_FABRIC")
        self.assertEqual(issue.layer_index, layer["layer_index"])
        self.assertEqual(issue.fold_id, fold["fold_id"])
        self.assertEqual(issue.light_id, scene["lights"][0]["light_id"])

    def test_light_inside_closed_fabric_volume_is_rejected(self) -> None:
        scene = copy.deepcopy(self.scene)
        center = np.asarray(scene["lights"][1]["position"], dtype=np.float64)
        vertices = np.array(
            [
                [-0.2, -0.2, -0.2],
                [0.2, -0.2, -0.2],
                [0.2, 0.2, -0.2],
                [-0.2, 0.2, -0.2],
                [-0.2, -0.2, 0.2],
                [0.2, -0.2, 0.2],
                [0.2, 0.2, 0.2],
                [-0.2, 0.2, 0.2],
            ],
            dtype=np.float64,
        ) + center
        triangles = [
            [0, 2, 1],
            [0, 3, 2],
            [4, 5, 6],
            [4, 6, 7],
            [0, 1, 5],
            [0, 5, 4],
            [3, 7, 6],
            [3, 6, 2],
            [0, 4, 7],
            [0, 7, 3],
            [1, 2, 6],
            [1, 6, 5],
        ]
        scene["layers"] = [
            {
                "layer_index": 1,
                "thickness": 0.01,
                "transmittance": 0.3,
                "scatter_color": [0.7, 0.3, 0.2],
                "pattern_seed": 17,
                "folds": [
                    {
                        "fold_id": 7001,
                        "vertices": vertices.tolist(),
                        "uvs": [[0.0, 0.0] for _ in vertices],
                        "triangles": triangles,
                    }
                ],
            }
        ]
        issues = self._issues(scene)
        issue = next(item for item in issues if item.code == "LIGHT_INSIDE_FABRIC")
        self.assertEqual(issue.layer_index, 1)
        self.assertEqual(issue.fold_id, 7001)
        self.assertEqual(issue.light_id, "lamp_center")

    def test_cli_rejection_writes_no_image(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"][0]["thickness"] = -1.0
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "scene.json"
            out_dir = root / "out"
            write_json_atomic(scene_path, scene)
            code = main(
                [
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out",
                    str(out_dir),
                ]
            )
            self.assertEqual(code, 2)
            self.assertFalse((out_dir / "render.png").exists())
            with open(
                out_dir / "validation_errors.json", "r", encoding="utf-8"
            ) as handle:
                report = json.load(handle)
            self.assertEqual(report["status"], "rejected")
            self.assertEqual(report["business_code"], "INVALID_SCENE")
            self.assertGreater(report["issue_count"], 0)


if __name__ == "__main__":
    unittest.main()

