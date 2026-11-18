from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from rosewindow.errors import SceneValidationError
from rosewindow.scene import parse_scene

from tests.helpers import run_cli, small_scene_raw, write_json


class ValidationTests(unittest.TestCase):
    def assert_piece_error(self, raw, code, quadrant, piece_index):
        with self.assertRaises(SceneValidationError) as context:
            parse_scene(raw)
        error = context.exception
        self.assertEqual(error.code, code)
        self.assertEqual(error.quadrant, quadrant)
        self.assertEqual(error.piece_index, piece_index)

    def test_mesh_index_out_of_range(self):
        raw = small_scene_raw()
        raw["glass_pieces"][2]["mesh"]["triangles"][0][1] = 999
        self.assert_piece_error(raw, "RW_E_MESH_INDEX", 0, 2)

    def test_degenerate_triangle(self):
        raw = small_scene_raw()
        raw["glass_pieces"][1]["mesh"]["triangles"][0] = [0, 0, 1]
        self.assert_piece_error(raw, "RW_E_DEGENERATE_TRIANGLE", 0, 1)

    def test_transmittance_range(self):
        raw = small_scene_raw()
        raw["glass_pieces"][4]["transmittance"] = 1.0001
        self.assert_piece_error(raw, "RW_E_RANGE_TRANSMITTANCE", 0, 4)

    def test_reflectance_range(self):
        raw = small_scene_raw()
        raw["glass_pieces"][7]["enamel_reflectance"] = -0.01
        self.assert_piece_error(raw, "RW_E_RANGE_REFLECTANCE", 1, 1)

    def test_duplicate_piece_identity(self):
        raw = small_scene_raw()
        raw["glass_pieces"].append(copy.deepcopy(raw["glass_pieces"][3]))
        self.assert_piece_error(raw, "RW_E_DUPLICATE_PIECE", 0, 3)

    def test_duplicate_quadrant_declaration(self):
        raw = small_scene_raw()
        raw["quadrants"] = [0, 0, 1, 2, 3]
        with self.assertRaises(SceneValidationError) as context:
            parse_scene(raw)
        self.assertEqual(context.exception.code, "RW_E_DUPLICATE_QUADRANT")
        self.assertEqual(context.exception.quadrant, 0)

    def test_cli_rejects_without_partial_image(self):
        raw = small_scene_raw()
        raw["glass_pieces"][5]["mesh"]["triangles"][0][2] = 400
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "bad.json"
            output_dir = directory / "output"
            write_json(scene_path, raw)
            result = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(output_dir),
            )
            self.assertEqual(result.returncode, 2, result.stderr)
            error = json.loads(result.stderr)
            self.assertEqual(error["error"]["code"], "RW_E_MESH_INDEX")
            self.assertEqual(error["error"]["quadrant"], 0)
            self.assertEqual(error["error"]["piece_index"], 5)
            self.assertFalse((output_dir / "rgb.png").exists())
            self.assertFalse((output_dir / "rgb.npy").exists())


if __name__ == "__main__":
    unittest.main()
