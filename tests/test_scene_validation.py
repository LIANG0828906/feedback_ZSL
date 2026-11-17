from __future__ import annotations

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from serene_render.errors import SceneValidationError
from serene_render.scene import make_sample_scene, parse_scene


class SceneValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = make_sample_scene()

    def _issues(self, mutator) -> list:
        raw = copy.deepcopy(self.scene)
        mutator(raw)
        with self.assertRaises(SceneValidationError) as caught:
            parse_scene(raw)
        return caught.exception.issues

    def test_sample_scene_has_expected_five_layers(self) -> None:
        scene = parse_scene(self.scene)
        self.assertEqual([layer.layer_index for layer in scene.layers], [0, 1, 2, 3, 4])
        self.assertEqual(
            [layer.kind for layer in scene.layers],
            ["silk", "mullion", "silk", "mullion", "silk"],
        )
        self.assertAlmostEqual(float((scene.sun.direction**2).sum()), 1.0, places=12)

    def test_duplicate_layer_index(self) -> None:
        def mutate(raw):
            raw["layers"][2]["layer_index"] = 1

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "layer_index" and issue.layer_index == 1 for issue in issues)
        )

    def test_missing_layer_index(self) -> None:
        def mutate(raw):
            raw["layers"].pop(3)

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "layer_index" and issue.layer_index == 3 for issue in issues)
        )

    def test_duplicate_mesh_id(self) -> None:
        def mutate(raw):
            raw["layers"][1]["id"] = raw["layers"][0]["id"]

        issues = self._issues(mutate)
        self.assertTrue(any(issue.field == "id" for issue in issues))

    def test_triangle_index_out_of_range(self) -> None:
        def mutate(raw):
            raw["layers"][0]["triangles"][0][0] = 9999

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "triangles" and issue.layer_index == 0 for issue in issues)
        )

    def test_degenerate_triangle(self) -> None:
        def mutate(raw):
            raw["layers"][0]["triangles"][0] = [0, 1, 1]

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "triangles" and issue.layer_index == 0 for issue in issues)
        )

    def test_transmittance_out_of_range(self) -> None:
        def mutate(raw):
            raw["layers"][0]["transmittance"] = 1.2

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "transmittance" and issue.layer_index == 0 for issue in issues)
        )

    def test_reflectance_out_of_range(self) -> None:
        def mutate(raw):
            raw["layers"][1]["reflectance"] = -0.01

        issues = self._issues(mutate)
        self.assertTrue(
            any(issue.field == "reflectance" and issue.layer_index == 1 for issue in issues)
        )

    def test_sun_direction_must_be_unit_length(self) -> None:
        def mutate(raw):
            raw["sun"]["direction"] = [0.2, 0.3, 0.4]

        issues = self._issues(mutate)
        self.assertTrue(any(issue.field == "sun.direction" for issue in issues))

    def test_sun_must_not_be_parallel_to_a_layer(self) -> None:
        def mutate(raw):
            raw["sun"]["direction"] = [1.0, 0.0, 0.0]

        issues = self._issues(mutate)
        parallel = [issue for issue in issues if issue.field == "sun.direction"]
        self.assertGreaterEqual(len(parallel), 5)
        self.assertTrue(all(issue.layer_index is not None for issue in parallel[:5]))

    def test_camera_legality(self) -> None:
        def mutate(raw):
            raw["camera"]["up"] = [1.0, 0.0, 0.0]
            raw["camera"]["target"] = [4.0, 2.55, 7.2]

        issues = self._issues(mutate)
        self.assertTrue(any(issue.field == "camera.up" for issue in issues))

    def test_ground_bounds_legality(self) -> None:
        def mutate(raw):
            raw["ground"]["bounds"] = [1.0, 1.0, -2.0, 2.0]

        issues = self._issues(mutate)
        self.assertTrue(any(issue.field == "ground.bounds" for issue in issues))

    def test_cli_validation_error_is_structured_and_nonzero(self) -> None:
        raw = copy.deepcopy(self.scene)
        raw["layers"][2]["transmittance"] = 3.0
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scene_path = root / "invalid.json"
            out = root / "must_not_render"
            scene_path.write_text(json.dumps(raw), encoding="utf-8")
            completed = subprocess.run(
                [
                    sys.executable,
                    str(Path(__file__).resolve().parents[1] / "render.py"),
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out",
                    str(out),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 2)
            payload = json.loads(completed.stderr)
            self.assertEqual(payload["code"], "SCENE_VALIDATION_ERROR")
            target = [
                issue
                for issue in payload["errors"]
                if issue["field"] == "transmittance"
                and issue["layer_index"] == 2
            ]
            self.assertEqual(len(target), 1)
            self.assertFalse((out / "render.png").exists())


if __name__ == "__main__":
    unittest.main()
