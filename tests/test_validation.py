from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from elytron.scene import SceneValidationError, generate_scene, validate_scene

ROOT = Path(__file__).resolve().parents[1]


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.scene = generate_scene(254)

    def assert_code(self, scene, code: str, face_id: int | None = None) -> None:
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene(scene)
        matches = [w for w in caught.exception.warnings if w["code"] == code]
        self.assertTrue(matches, caught.exception.warnings)
        if face_id is not None:
            self.assertIn(face_id, {warning["face_id"] for warning in matches})
        for warning in matches:
            self.assertIn("face_id", warning)

    def test_mesh_index_out_of_bounds(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["mesh"]["triangles"][0][2] = len(scene["mesh"]["vertices"]) + 100
        self.assert_code(scene, "E_MESH_INDEX_OOB", 1)

    def test_degenerate_triangle(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["mesh"]["triangles"][2] = [0, 0, 1]
        self.assert_code(scene, "E_DEGENERATE_TRIANGLE", 3)

    def test_face_id_missing(self) -> None:
        scene = copy.deepcopy(self.scene)
        removed = scene["mesh"]["face_ids"].pop()
        self.assert_code(scene, "E_FACE_ID_MISSING", removed)

    def test_face_id_duplicate(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["mesh"]["face_ids"][-1] = scene["mesh"]["face_ids"][0]
        self.assert_code(scene, "E_FACE_ID_DUPLICATE", 1)

    def test_film_thickness_out_of_bounds(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["film"]["base_thickness_nm"] = 900.0
        self.assert_code(scene, "E_FILM_THICKNESS_RANGE", 1)

    def test_film_refractive_index_out_of_bounds(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["film"]["refractive_index"] = 3.4
        self.assert_code(scene, "E_FILM_REFRACTIVE_RANGE", 1)

    def test_light_inside_closed_mesh(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["lights"][0]["position"] = [0.0, 0.13, 0.0]
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene(scene)
        warnings = [
            warning
            for warning in caught.exception.warnings
            if warning["code"] == "E_LIGHT_INSIDE_ELYTROM"
        ]
        self.assertTrue(warnings)
        self.assertGreater(warnings[0]["face_id"], 0)
        self.assertEqual(warnings[0]["details"]["source_id"], "L1")

    def test_invalid_camera(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["cameras"][0]["target"] = list(scene["cameras"][0]["position"])
        self.assert_code(scene, "E_CAMERA_INVALID")

    def test_validation_failure_writes_no_images(self) -> None:
        scene = copy.deepcopy(self.scene)
        scene["mesh"]["triangles"][4][0] = 999999
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "invalid.json"
            scene_path.write_text(json.dumps(scene), encoding="utf-8")
            out = root / "out"
            result = subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "elytron_render.py"),
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out",
                    str(out),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(result.returncode, 2, result.stderr)
            report = json.loads((out / "validation_report.json").read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "aborted")
            self.assertFalse(report["image_written"])
            self.assertIn("E_MESH_INDEX_OOB", report["business_codes"])
            images = list(out.glob("*.png")) + list(out.glob("*.npy"))
            self.assertEqual(images, [])


if __name__ == "__main__":
    unittest.main()
