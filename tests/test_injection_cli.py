from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from volrender.binary import build_scene
from volrender.config import load_config

from tests.helpers import config, tiny_scene

ROOT = Path(__file__).resolve().parents[1]


class InjectionCliTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory(prefix="volrender-inject-")
        self.root = Path(self.tempdir.name)
        self.scene_path = self.root / "scene.vscn"
        self.scene_path.write_bytes(build_scene(tiny_scene()))
        self.config_path = self.root / "config.json"
        self.config_path.write_text(
            json.dumps(config(sample_budget=500_000).to_dict()),
            encoding="utf-8",
        )

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def _render_with_injection(self, injection: str) -> tuple[dict, Path]:
        outdir = self.root / f"out-{injection}"
        completed = subprocess.run(
            [
                sys.executable,
                "-m",
                "volrender",
                "render",
                "--scene",
                str(self.scene_path),
                "--config",
                str(self.config_path),
                "--outdir",
                str(outdir),
                "--inject",
                injection,
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        payload = json.loads(completed.stdout)
        return payload, outdir

    def test_five_injection_classes_return_distinct_codes_before_output(self) -> None:
        expected = {
            "chunk_index": "E_CHUNK_INDEX",
            "missing_media": "E_MEDIA_MISSING",
            "camera_nonfinite": "E_CAMERA_NONFINITE",
            "density_truncated": "E_DENSITY_TRUNCATED",
            "config_budget": "E_CONFIG_BUDGET",
        }
        codes = set()
        for injection, code in expected.items():
            payload, outdir = self._render_with_injection(injection)
            self.assertFalse(payload["ok"])
            self.assertEqual(payload["code"], code)
            self.assertIn("field", payload)
            self.assertGreaterEqual(payload["offset"], 0)
            self.assertFalse((outdir / "image.ppm").exists())
            self.assertFalse((outdir / "summary.json").exists())
            codes.add(payload["code"])
        self.assertEqual(len(codes), 5)


if __name__ == "__main__":
    unittest.main()
