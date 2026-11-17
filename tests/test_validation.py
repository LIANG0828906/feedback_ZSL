from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from biomemaster.errors import MemoryLimit
from biomemaster.project import load_project
from tests.support import run_cli
from tools.generate_sample import generate


class ValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temporary.name) / "project"
        generate(cls.root, 2.0)
        cls.project = cls.root / "project.json"

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def _copy_project(self, name: str) -> tuple[Path, dict]:
        directory = Path(self._temporary.name) / name
        directory.mkdir()
        for layer in (self.root / "layers").iterdir():
            (directory / "layers").mkdir(exist_ok=True)
            (directory / "layers" / layer.name).write_bytes(layer.read_bytes())
        value = json.loads(self.project.read_text(encoding="utf-8"))
        path = directory / "project.json"
        path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
        return path, value

    def test_invalid_json_returns_business_error_and_exit_two(self) -> None:
        bad = Path(self._temporary.name) / "invalid.json"
        bad.write_text("{not json", encoding="utf-8")
        result = run_cli(
            "render",
            str(bad),
            "--output",
            str(Path(self._temporary.name) / "x.wav"),
            "--report",
            str(Path(self._temporary.name) / "x.json"),
        )
        self.assertEqual(result.returncode, 2)
        payload = json.loads(result.stderr)
        self.assertEqual(payload["code"], "E_PROJECT_INVALID")
        self.assertNotIn("Traceback", result.stderr)

    def test_curve_range_is_validated(self) -> None:
        path, value = self._copy_project("curve")
        value["biomes"][0]["layers"][0]["curve"][0]["gain_db"] = 13.0
        path.write_text(json.dumps(value), encoding="utf-8")
        result = run_cli(
            "render",
            str(path),
            "--output",
            str(path.parent / "x.wav"),
            "--report",
            str(path.parent / "x.json"),
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stderr)["code"], "E_PROJECT_INVALID")

    def test_curve_times_are_non_decreasing(self) -> None:
        path, value = self._copy_project("times")
        value["biomes"][0]["layers"][0]["curve"][0]["time"] = 2.0
        value["biomes"][0]["layers"][0]["curve"][1]["time"] = 1.0
        path.write_text(json.dumps(value), encoding="utf-8")
        result = run_cli(
            "render",
            str(path),
            "--output",
            str(path.parent / "x.wav"),
            "--report",
            str(path.parent / "x.json"),
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stderr)["code"], "E_PROJECT_INVALID")

    def test_layer_sample_rate_must_match(self) -> None:
        path, value = self._copy_project("rate")
        value["sample_rate"] = 44100
        path.write_text(json.dumps(value), encoding="utf-8")
        result = run_cli(
            "render",
            str(path),
            "--output",
            str(path.parent / "x.wav"),
            "--report",
            str(path.parent / "x.json"),
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stderr)["code"], "E_PROJECT_INVALID")

    def test_memory_budget_is_enforced_before_loop_load(self) -> None:
        with patch("biomemaster.project.MAX_WORKING_MEMORY_BYTES", 1):
            with self.assertRaises(MemoryLimit) as raised:
                load_project(self.project, workers=1)
        self.assertEqual(raised.exception.code, "E_MEMORY_LIMIT")
        self.assertEqual(raised.exception.exit_code, 2)


if __name__ == "__main__":
    unittest.main()
