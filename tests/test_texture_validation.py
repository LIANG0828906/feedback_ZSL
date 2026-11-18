from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

from veilrender.errors import SceneValidationError
from veilrender.sample_scene import make_invalid_scene, make_sample_scene, write_scene
from veilrender.scene import validate_scene
from veilrender.texture import generate_weave_texture, texture_digest

ROOT = Path(__file__).resolve().parents[1]


class TextureTests(unittest.TestCase):
    def test_seed_and_resolution_are_the_only_inputs(self) -> None:
        first = generate_weave_texture(98765, [73, 41])
        second = generate_weave_texture(98765, [73, 41])
        different_seed = generate_weave_texture(98766, [73, 41])
        different_resolution = generate_weave_texture(98765, [41, 73])
        self.assertEqual(first.tobytes(order="C"), second.tobytes(order="C"))
        self.assertEqual(texture_digest(first), texture_digest(second))
        self.assertNotEqual(first.tobytes(order="C"), different_seed.tobytes(order="C"))
        self.assertNotEqual(first.shape, different_resolution.shape)
        self.assertGreater(int(first.max()) - int(first.min()), 32)

    def test_repeated_generation_has_no_global_rng_dependency(self) -> None:
        expected = generate_weave_texture(17, [32, 32]).tobytes(order="C")
        np.random.seed(1)
        np.random.random(1000)
        actual = generate_weave_texture(17, [32, 32]).tobytes(order="C")
        self.assertEqual(expected, actual)


class ValidationTests(unittest.TestCase):
    def test_numeric_layer_and_light_ids_are_normalized(self) -> None:
        scene = make_sample_scene(
            width=32,
            height=20,
            tile_width=16,
            tile_height=10,
            camera_samples=1,
        )
        scene["layers"][0]["id"] = 101
        scene["lights"][0]["id"] = 7
        validated = validate_scene(scene)
        self.assertEqual(validated["layers"][0]["id"], "101")
        self.assertEqual(validated["lights"][0]["id"], "7")

    def test_required_business_errors_are_structured_and_layer_qualified(self) -> None:
        cases = {
            "mesh_index": ("E_MESH_INDEX_RANGE", 2),
            "degenerate": ("E_TRIANGLE_DEGENERATE", 1),
            "transmittance": ("E_TRANSMITTANCE_RANGE", 1),
            "reflectance": ("E_REFLECTANCE_RANGE", 2),
            "duplicate_layer": ("E_LAYER_ORDER_DUPLICATE", 1),
            "duplicate_id": ("E_LAYER_ID_DUPLICATE", 3),
            "light_inside": ("E_LIGHT_INSIDE_LAYER", 3),
            "camera": ("E_CAMERA_INVALID", None),
        }
        for case, (code, layer) in cases.items():
            with self.subTest(case=case):
                with self.assertRaises(SceneValidationError) as caught:
                    validate_scene(make_invalid_scene(case))
                issue = next(issue for issue in caught.exception.issues if issue.code == code)
                self.assertEqual(issue.layer, layer)
                if layer is not None:
                    self.assertIsNotNone(issue.layer_id)
                structured = caught.exception.as_dict()
                self.assertFalse(structured["ok"])
                self.assertEqual(issue.code, code)

    def test_invalid_cli_exits_nonzero_without_snapshot_or_rgba(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "invalid.json"
            output_dir = root / "out"
            write_scene(make_invalid_scene("mesh_index"), scene_path)
            result = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "veilrender",
                    "render",
                    "--scene",
                    str(scene_path),
                    "--output-dir",
                    str(output_dir),
                    "--quiet",
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertNotEqual(result.returncode, 0)
            error = json.loads(result.stderr)
            self.assertIn("E_MESH_INDEX_RANGE", [issue["code"] for issue in error["issues"]])
            self.assertFalse((output_dir / "rgba.png").exists())
            self.assertFalse((output_dir / "snapshot").exists())
            persisted = json.loads((output_dir / "error.json").read_text(encoding="utf-8"))
            self.assertEqual(persisted["error"], "scene_validation_failed")

    def test_sample_scene_contains_same_tint_neighbouring_weaves(self) -> None:
        scene = make_sample_scene(width=64, height=40, tile_width=32, tile_height=20, camera_samples=1)
        layers = scene["layers"]
        pair = (layers[2], layers[3])
        self.assertEqual(pair[0]["tint"], pair[1]["tint"])
        self.assertNotEqual(pair[0]["weave_seed"], pair[1]["weave_seed"])
        first = generate_weave_texture(pair[0]["weave_seed"], pair[0]["resolution"])
        second = generate_weave_texture(pair[1]["weave_seed"], pair[1]["resolution"])
        self.assertNotEqual(texture_digest(first), texture_digest(second))


if __name__ == "__main__":
    unittest.main()

