from __future__ import annotations

import copy
import json
import tempfile
import unittest
from pathlib import Path

from glass_verify.errors import SceneValidationError
from glass_verify.sample import make_sample_scene, write_sample_scene
from glass_verify.scene import validate_scene

from tests.helpers import run_cli, write_json


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.base = make_sample_scene()

    def assert_rejected(self, scene: dict, code: str, sector_id: int = 0) -> None:
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene(scene)
        matching = [issue for issue in caught.exception.issues if issue.code == code]
        self.assertTrue(matching, caught.exception.as_dict())
        self.assertEqual(matching[0].sector_id, sector_id)
        self.assertTrue(matching[0].reason)

    def test_index_out_of_range(self) -> None:
        scene = copy.deepcopy(self.base)
        scene["panes"][0]["triangles"][0] = [0, 1, 999]
        self.assert_rejected(scene, "index_out_of_range")

    def test_degenerate_triangle(self) -> None:
        scene = copy.deepcopy(self.base)
        pane = scene["panes"][0]
        pane["vertices"] = [
            [-0.62, 0.35, 0.06],
            [-0.62, 0.35, 0.06],
            [0.62, 0.35, 0.06],
        ]
        pane["triangles"] = [[0, 1, 2]]
        self.assert_rejected(scene, "degenerate_triangle")

    def test_material_ranges(self) -> None:
        scene = copy.deepcopy(self.base)
        scene["panes"][0]["transmittance"] = 1.01
        self.assert_rejected(scene, "transmittance_range")
        scene = copy.deepcopy(self.base)
        scene["panes"][1]["glaze_reflectance"] = -0.01
        self.assert_rejected(scene, "reflectance_range", sector_id=0)

    def test_missing_and_duplicate_sector(self) -> None:
        scene = copy.deepcopy(self.base)
        scene["panes"] = [pane for pane in scene["panes"] if pane["sector_id"] != 1]
        self.assert_rejected(scene, "sector_missing", sector_id=1)

        scene = copy.deepcopy(self.base)
        scene["sectors"][1]["sector_id"] = 0
        self.assert_rejected(scene, "sector_duplicate", sector_id=0)

    def test_parallel_light(self) -> None:
        scene = copy.deepcopy(self.base)
        scene["directional_light"]["direction"] = [1.0, 0.0, 0.0]
        self.assert_rejected(scene, "light_parallel_to_window_plane")

    def test_sample_scene_generation_is_byte_stable(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            first = Path(temporary) / "first.json"
            second = Path(temporary) / "second.json"
            write_sample_scene(first, 12345)
            write_sample_scene(second, 12345)
            self.assertEqual(first.read_bytes(), second.read_bytes())

    def test_cli_rejection_is_structured_and_nonzero(self) -> None:
        scene = copy.deepcopy(self.base)
        scene["panes"][0]["triangles"][0] = [0, 1, 99]
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "bad.json"
            write_json(path, scene)
            result = run_cli("validate", "--scene", str(path))
        self.assertEqual(result.returncode, 2)
        payload = json.loads(result.stdout)
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["error_kind"], "scene_validation")
        self.assertEqual(payload["errors"][0]["sector_id"], 0)
        self.assertIn("reason", payload["errors"][0])


if __name__ == "__main__":
    unittest.main()
