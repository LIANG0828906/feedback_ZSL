from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from paper_lantern.cli import main
from paper_lantern.errors import SceneValidationError
from paper_lantern.generator import generate_scene
from paper_lantern.scene import validate_scene_data


def _codes(error: SceneValidationError) -> set[str]:
    return {issue.code for issue in error.issues}


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = generate_scene(
            width=48, height=32, tile_width=16, tile_height=16, shadow_samples=2
        )

    def test_duplicate_layer_index(self) -> None:
        bad = copy.deepcopy(self.scene)
        bad["layers"][1]["layer_index"] = 0
        with self.assertRaises(SceneValidationError) as context:
            validate_scene_data(bad)
        issue = next(
            issue for issue in context.exception.issues if issue.code == "DUPLICATE_LAYER_INDEX"
        )
        self.assertEqual(issue.layer_index, 0)

    def test_mesh_index_out_of_range(self) -> None:
        bad = copy.deepcopy(self.scene)
        bad["layers"][2]["mesh"]["triangles"][0][1] = 999
        with self.assertRaises(SceneValidationError) as context:
            validate_scene_data(bad)
        issue = next(
            issue
            for issue in context.exception.issues
            if issue.code == "MESH_INDEX_OUT_OF_RANGE"
        )
        self.assertEqual(issue.layer_index, 2)
        self.assertIn("999", issue.reason)

    def test_degenerate_triangle(self) -> None:
        bad = copy.deepcopy(self.scene)
        bad["layers"][1]["mesh"]["triangles"][0] = [0, 0, 1]
        with self.assertRaises(SceneValidationError) as context:
            validate_scene_data(bad)
        self.assertIn("TRIANGLE_DEGENERATE", _codes(context.exception))

    def test_transmittance_out_of_range(self) -> None:
        bad = copy.deepcopy(self.scene)
        bad["layers"][3]["transmittance"] = 1.25
        with self.assertRaises(SceneValidationError) as context:
            validate_scene_data(bad)
        issue = next(
            issue
            for issue in context.exception.issues
            if issue.code == "TRANSMITTANCE_OUT_OF_RANGE"
        )
        self.assertEqual(issue.layer_index, 3)

    def test_light_inside_closed_layer_mesh(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["layers"] = [
            {
                "layer_index": 0,
                "transmittance": 0.5,
                "scatter_color": [1.0, 1.0, 1.0],
                "texture_resolution": 16,
                "mesh": {
                    "vertices": [
                        [-1.0, -1.0, -1.0],
                        [1.0, -1.0, -1.0],
                        [0.0, 1.0, -1.0],
                        [0.0, 0.0, 1.0],
                    ],
                    "triangles": [[0, 2, 1], [0, 1, 3], [1, 2, 3], [2, 0, 3]],
                    "uvs": [[0.0, 0.0], [1.0, 0.0], [0.0, 1.0], [1.0, 1.0]],
                },
            }
        ]
        scene["light"]["position"] = [0.0, 0.0, 0.0]
        with self.assertRaises(SceneValidationError) as context:
            validate_scene_data(scene)
        issue = next(
            issue
            for issue in context.exception.issues
            if issue.code == "LIGHT_INSIDE_LAYER"
        )
        self.assertEqual(issue.layer_index, 0)

    def test_cli_validation_failure_writes_report_but_no_image(self) -> None:
        bad = copy.deepcopy(self.scene)
        bad["layers"][0]["mesh"]["triangles"][0] = [0, 1, 9999]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "bad.json"
            scene_path.write_text(json.dumps(bad), encoding="utf-8")
            output = root / "out"
            code = main(["render", str(scene_path), "-o", str(output)])
            self.assertEqual(code, 2)
            report = json.loads((output / "report.json").read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "error")
            self.assertFalse(report["image_written"])
            self.assertIn(
                "MESH_INDEX_OUT_OF_RANGE",
                [issue["code"] for issue in report["issues"]],
            )
            self.assertFalse((output / "image.rgba").exists())


if __name__ == "__main__":
    unittest.main()

