from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from celadon_museum.errors import StructuredError
from celadon_museum.meshgen import make_scene
from celadon_museum.scene import validate_scene


def sample_document():
    return make_scene(
        width=128,
        height=96,
        tile_size=32,
        texture_resolution=64,
        radial_segments=20,
        rings=5,
    )


class SceneValidationTests(unittest.TestCase):
    def assert_business_error(self, document, code, plate_id=None, layer_id=None):
        with self.assertRaises(StructuredError) as caught:
            validate_scene(document)
        error = caught.exception
        self.assertEqual(error.code, code)
        self.assertEqual(error.plate_id, plate_id)
        self.assertEqual(error.layer_id, layer_id)
        self.assertTrue(error.reason)

    def test_mesh_index_out_of_range(self):
        document = sample_document()
        document["layers"][0]["triangles"][2][0] = 99999
        self.assert_business_error(document, "E_MESH_INDEX_OUT_OF_RANGE", 0, 0)

    def test_degenerate_triangle(self):
        document = sample_document()
        triangle = document["layers"][1]["triangles"][4]
        triangle[1] = triangle[0]
        self.assert_business_error(document, "E_DEGENERATE_TRIANGLE", 0, 1)

    def test_transmittance_out_of_range(self):
        document = sample_document()
        document["layers"][2]["transmittance"] = 1.01
        self.assert_business_error(document, "E_TRANSMITTANCE_OUT_OF_RANGE", 0, 2)

    def test_duplicate_plate_and_layer(self):
        document = sample_document()
        document["plates"].append(copy.deepcopy(document["plates"][0]))
        self.assert_business_error(document, "E_DUPLICATE_PLATE_ID", 0, None)

        document = sample_document()
        document["layers"].append(copy.deepcopy(document["layers"][0]))
        self.assert_business_error(document, "E_DUPLICATE_LAYER", 0, 0)

    def test_missing_plate_and_layer(self):
        document = sample_document()
        document["layers"] = [
            entry
            for entry in document["layers"]
            if not (entry["plate_id"] == 1 and entry["layer_id"] == 2)
        ]
        self.assert_business_error(document, "E_MISSING_LAYER", 1, 2)

        document = sample_document()
        document["plates"] = [entry for entry in document["plates"] if entry["plate_id"] != 2]
        self.assert_business_error(document, "E_MISSING_PLATE", 2, 0)

        document = sample_document()
        document["plates"] = [entry for entry in document["plates"] if entry["plate_id"] != 1]
        document["layers"] = [
            entry for entry in document["layers"] if entry["plate_id"] != 1
        ]
        self.assert_business_error(document, "E_MISSING_PLATE_ID", 1, None)

    def test_light_inside_layer(self):
        document = sample_document()
        document["lights"][0]["position"] = document["plates"][0]["center"]
        self.assert_business_error(document, "E_LIGHT_INSIDE_LAYER", 0, 1)

    def test_validation_failure_does_not_emit_partial_image(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "bad.json"
            output_path = root / "out"
            document = sample_document()
            document["layers"][0]["transmittance"] = 2.0
            scene_path.write_text(json.dumps(document), encoding="utf-8")
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "celadon_museum",
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out",
                    str(output_path),
                ],
                cwd=Path(__file__).resolve().parents[1],
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertNotEqual(completed.returncode, 0)
            payload = json.loads(completed.stdout)
            self.assertEqual(payload["error"]["code"], "E_TRANSMITTANCE_OUT_OF_RANGE")
            self.assertFalse(output_path.exists())


if __name__ == "__main__":
    unittest.main()
