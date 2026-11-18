from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

from fogvol.errors import FogError
from fogvol.scene import parse_scene

from .common import ROOT, compact_scene, write_json


class SceneValidationTests(unittest.TestCase):
    def assert_scene_error(
        self,
        raw: dict,
        code: str,
        path: str,
    ) -> None:
        with self.assertRaises(FogError) as caught:
            parse_scene(raw)
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(caught.exception.path, path)
        self.assertTrue(caught.exception.message)

    def test_density_length_and_finiteness(self) -> None:
        raw = compact_scene()
        raw["volume"]["density"] = [0.1] * 7
        self.assert_scene_error(
            raw, "DENSITY_LENGTH_MISMATCH", "volume.density"
        )

        raw = compact_scene()
        count = int(np.prod(raw["volume"]["dimensions"]))
        density = [0.25] * count
        density[17] = float("nan")
        raw["volume"]["density"] = density
        self.assert_scene_error(raw, "NONFINITE_DENSITY", "volume.density")

        raw = compact_scene()
        density = [0.25] * count
        density[9] = 1.5
        raw["volume"]["density"] = density
        self.assert_scene_error(
            raw, "DENSITY_OUT_OF_RANGE", "volume.density"
        )

    def test_camera_lights_and_sampling_fields(self) -> None:
        raw = compact_scene()
        raw["camera"]["fov_deg"] = 180.0
        self.assert_scene_error(raw, "INVALID_FOV", "camera.fov_deg")

        raw = compact_scene()
        raw["camera"]["aspect"] = 2.0
        self.assert_scene_error(
            raw, "ASPECT_MISMATCH", "camera.aspect"
        )

        raw = compact_scene()
        raw["lights"][0]["position"] = [2.0, 2.0, 2.0]
        self.assert_scene_error(
            raw, "LIGHT_INSIDE_VOLUME", "lights[0].position"
        )

        raw = compact_scene()
        raw["render"]["step"] = 0
        self.assert_scene_error(raw, "INVALID_INTEGER", "render.step")

        raw = compact_scene()
        raw["render"]["max_samples"] = 0
        self.assert_scene_error(
            raw, "INVALID_INTEGER", "render.max_samples"
        )

    def test_embedded_density_is_accepted(self) -> None:
        raw = compact_scene()
        scene = parse_scene(raw)
        density = scene.volume.density.reshape(-1)
        embedded = copy.deepcopy(raw)
        embedded["volume"]["density"] = [
            round(float(value), 9) for value in density
        ]
        parsed = parse_scene(embedded)
        self.assertEqual(
            parsed.volume.density.shape, scene.volume.density.shape
        )
        self.assertTrue(np.all(np.isfinite(parsed.volume.density)))

    def test_invalid_scene_writes_no_image_and_nonzero_exit(self) -> None:
        raw = compact_scene()
        raw["lights"][1]["position"] = [4.0, 4.0, 4.0]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "invalid.json"
            out_dir = root / "out"
            write_json(scene_path, raw)
            result = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "fogvol",
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out",
                    str(out_dir),
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertNotEqual(result.returncode, 0)
            error = json.loads(result.stderr)
            self.assertFalse(error["ok"])
            self.assertEqual(
                error["error"]["code"], "LIGHT_INSIDE_VOLUME"
            )
            self.assertEqual(
                error["error"]["path"], "lights[1].position"
            )
            self.assertFalse((out_dir / "rgba.png").exists())
            self.assertFalse((out_dir / "optical_depth.npy").exists())


if __name__ == "__main__":
    unittest.main()

