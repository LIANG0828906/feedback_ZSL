from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from veilrender.comparison_helpers import compare_artifacts
from veilrender.sample_scene import make_sample_scene, write_scene

ROOT = Path(__file__).resolve().parents[1]


def _command(scene: Path, output: Path) -> list[str]:
    return [
        sys.executable,
        "-m",
        "veilrender",
        "render",
        "--scene",
        str(scene),
        "--output-dir",
        str(output),
        "--quiet",
    ]


def _wait_for_commit(output: Path, minimum: int = 2, timeout: float = 30.0) -> int:
    manifest = output / "snapshot" / "manifest.json"
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if manifest.is_file():
            try:
                data = json.loads(manifest.read_text(encoding="utf-8"))
                count = int(data.get("commit_count", 0))
                if count >= minimum:
                    return count
            except (OSError, ValueError, json.JSONDecodeError):
                pass
        time.sleep(0.003)
    raise AssertionError("timed out waiting for a committed tile")


class RecoveryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temporary.name)
        cls.scene_path = cls.root / "scene.json"
        write_scene(
            make_sample_scene(
                width=192,
                height=128,
                tile_width=16,
                tile_height=16,
                camera_samples=1,
            ),
            cls.scene_path,
        )
        cls.baseline = cls.root / "baseline"
        result = subprocess.run(
            _command(cls.scene_path, cls.baseline),
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=120,
        )
        if result.returncode != 0:
            raise AssertionError(result.stderr)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def test_forced_termination_resumes_byte_identically(self) -> None:
        output = self.root / "killed"
        process = subprocess.Popen(
            _command(self.scene_path, output),
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        committed = _wait_for_commit(output, minimum=2)
        os.kill(process.pid, signal.SIGKILL)
        process.communicate(timeout=30)
        self.assertLess(process.returncode, 0)
        manifest = json.loads(
            (output / "snapshot" / "manifest.json").read_text(encoding="utf-8")
        )
        self.assertGreaterEqual(int(manifest["commit_count"]), committed)
        self.assertLess(int(manifest["commit_count"]), int(manifest["image"]["total_tiles"]))
        self.assertFalse((output / "rgba.png").exists())

        resumed = subprocess.run(
            _command(self.scene_path, output),
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        compare_artifacts(self.baseline, output)
        self.assertEqual(
            (self.baseline / "report.json").read_bytes(),
            (output / "report.json").read_bytes(),
        )

    def test_ctrl_c_finishes_tile_and_exits_zero(self) -> None:
        output = self.root / "interrupted"
        process = subprocess.Popen(
            _command(self.scene_path, output),
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        _wait_for_commit(output, minimum=1)
        os.kill(process.pid, signal.SIGINT)
        stdout, stderr = process.communicate(timeout=120)
        self.assertEqual(process.returncode, 0, stderr)
        recovery = json.loads((output / "recovery.json").read_text(encoding="utf-8"))
        self.assertTrue(recovery["interrupted"])
        self.assertEqual(recovery["exit_code"], 0)
        self.assertGreaterEqual(recovery["completed_tile_count"], 1)
        self.assertLess(recovery["completed_tile_count"], recovery["total_tiles"])

        resumed = subprocess.run(
            _command(self.scene_path, output),
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        compare_artifacts(self.baseline, output)

    def test_corrupt_snapshot_is_discarded_and_recomputed(self) -> None:
        output = self.root / "corrupt"
        process = subprocess.Popen(
            _command(self.scene_path, output),
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        _wait_for_commit(output, minimum=2)
        os.kill(process.pid, signal.SIGKILL)
        process.communicate(timeout=30)
        manifest = json.loads(
            (output / "snapshot" / "manifest.json").read_text(encoding="utf-8")
        )
        first_tile = manifest["completed"][sorted(manifest["completed"], key=int)[0]]["file"]
        (output / "snapshot" / "tiles" / first_tile).write_bytes(b"corrupt")

        resumed = subprocess.run(
            _command(self.scene_path, output),
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        metrics = json.loads((output / "run_metrics.json").read_text(encoding="utf-8"))
        self.assertEqual(metrics["snapshot_load_mode"], "reset")
        self.assertIn("checksum", metrics["snapshot_invalidation_reason"])
        compare_artifacts(self.baseline, output)

    def test_comparison_reports_tile_pixel_and_channel(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene = root / "scene.json"
            write_scene(
                make_sample_scene(
                    width=64,
                    height=40,
                    tile_width=32,
                    tile_height=20,
                    camera_samples=1,
                ),
                scene,
            )
            expected = root / "expected"
            actual = root / "actual"
            for output in (expected, actual):
                result = subprocess.run(
                    _command(scene, output),
                    cwd=ROOT,
                    capture_output=True,
                    text=True,
                    timeout=60,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
            from PIL import Image
            import numpy as np

            image = np.asarray(Image.open(actual / "rgba.png").convert("RGBA")).copy()
            image[3, 5, 0] = int(image[3, 5, 0]) ^ 1
            Image.fromarray(image, "RGBA").save(actual / "rgba.png")
            compared = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "veilrender",
                    "compare",
                    "--expected",
                    str(expected),
                    "--actual",
                    str(actual),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(compared.returncode, 3)
            differences = json.loads((actual / "differences.json").read_text(encoding="utf-8"))
            self.assertEqual(differences["difference_count"], 1)
            item = differences["differences"][0]
            self.assertEqual(item["tile_id"], 0)
            self.assertEqual(item["pixel"], {"x": 5, "y": 3})
            self.assertEqual(item["channel"], "R")


if __name__ == "__main__":
    unittest.main()

