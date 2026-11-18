from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import numpy as np

from tests.helpers import ROOT, compact_scene, run_cli, write_json


class RecoveryTests(unittest.TestCase):
    def _prepare(self, root: Path, seed: int = 424242) -> Path:
        scene_path = root / "scene.json"
        write_json(scene_path, compact_scene(seed))
        return scene_path

    def _clean_reference(self, scene_path: Path, output: Path) -> None:
        result = run_cli(
            "render",
            "--scene",
            str(scene_path),
            "--out",
            str(output),
            "--fresh",
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def _assert_equal_outputs(self, scene_path: Path, reference: Path, candidate: Path) -> None:
        result = run_cli(
            "compare",
            "--scene",
            str(scene_path),
            "--reference",
            str(reference),
            "--candidate",
            str(candidate),
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(json.loads(result.stdout)["ok"])
        for name in ("color.png", "attribution.png", "projection_hits.json"):
            self.assertEqual(
                (reference / name).read_bytes(),
                (candidate / name).read_bytes(),
                name,
            )

    def test_graceful_interrupt_resume_is_pixel_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = self._prepare(root)
            reference = root / "reference"
            resumed = root / "resumed"
            self._clean_reference(scene_path, reference)
            interrupted = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(resumed),
                "--fresh",
                "--stop-after-chunk",
                "2",
            )
            self.assertEqual(interrupted.returncode, 0, interrupted.stdout)
            report = json.loads(
                (resumed / "recovery_report.json").read_text(encoding="utf-8")
            )
            self.assertEqual(report["status"], "interrupted")
            self.assertEqual(report["completed_chunks"], [0, 1])
            self.assertTrue((resumed / "snapshot.npz").exists())
            completed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(resumed),
                "--reference",
                str(reference),
            )
            self.assertEqual(completed.returncode, 0, completed.stdout)
            stats = json.loads((resumed / "stats.json").read_text(encoding="utf-8"))
            self.assertEqual(stats["recovery"]["recovery_count"], 1)
            self.assertEqual(stats["recovery"]["resumed_chunk_count"], 2)
            self.assertEqual(stats["recovery"]["snapshot_status"], "valid")
            self.assertFalse((resumed / "recovery_report.json").exists())
            self._assert_equal_outputs(scene_path, reference, resumed)

    def test_two_fresh_runs_are_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = self._prepare(root, seed=98765)
            first = root / "first"
            second = root / "second"
            self._clean_reference(scene_path, first)
            self._clean_reference(scene_path, second)
            self._assert_equal_outputs(scene_path, first, second)
            self.assertEqual(
                (first / "textures.npy").read_bytes(),
                (second / "textures.npy").read_bytes(),
            )
            self.assertEqual(
                (first / "stats.json").read_bytes(),
                (second / "stats.json").read_bytes(),
            )

    def test_force_kill_resume_is_pixel_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = self._prepare(root)
            reference = root / "reference"
            killed = root / "killed"
            self._clean_reference(scene_path, reference)
            command = [
                sys.executable,
                "-m",
                "glass_verify",
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(killed),
                "--fresh",
                "--chunk-delay-ms",
                "90",
            ]
            process = subprocess.Popen(
                command,
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            deadline = time.time() + 30.0
            completed = 0
            while time.time() < deadline:
                try:
                    with np.load(killed / "snapshot.npz", allow_pickle=False) as archive:
                        meta = json.loads(bytes(archive["meta"]).decode("utf-8"))
                    completed = len(meta["completed_chunks"])
                    if completed >= 2:
                        break
                except Exception:
                    pass
                if process.poll() is not None:
                    stdout, stderr = process.communicate()
                    self.fail(f"render exited before kill: {stdout}\n{stderr}")
                time.sleep(0.03)
            self.assertGreaterEqual(completed, 2)
            process.kill()
            process.communicate(timeout=10)
            resumed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(killed),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
            stats = json.loads((killed / "stats.json").read_text(encoding="utf-8"))
            self.assertGreaterEqual(stats["recovery"]["resumed_chunk_count"], 2)
            self._assert_equal_outputs(scene_path, reference, killed)

    def test_corrupt_snapshot_restarts_from_zero(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = self._prepare(root)
            reference = root / "reference"
            candidate = root / "candidate"
            self._clean_reference(scene_path, reference)
            result = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(candidate),
                "--fresh",
                "--stop-after-chunk",
                "1",
            )
            self.assertEqual(result.returncode, 0)
            (candidate / "snapshot.npz").write_bytes(b"not a valid snapshot")
            resumed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(candidate),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
            stats = json.loads((candidate / "stats.json").read_text(encoding="utf-8"))
            self.assertEqual(stats["recovery"]["snapshot_status"], "discarded_restart")
            self.assertIsNotNone(stats["recovery"]["snapshot_discarded_reason"])
            self.assertEqual(stats["recovery"]["resumed_chunk_count"], 0)
            self._assert_equal_outputs(scene_path, reference, candidate)

    def test_changed_scene_digest_discards_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = self._prepare(root)
            candidate = root / "candidate"
            result = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(candidate),
                "--fresh",
                "--stop-after-chunk",
                "1",
            )
            self.assertEqual(result.returncode, 0)
            changed_scene = compact_scene(424243)
            write_json(scene_path, changed_scene)
            reference = root / "reference_changed"
            self._clean_reference(scene_path, reference)
            resumed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out",
                str(candidate),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
            stats = json.loads((candidate / "stats.json").read_text(encoding="utf-8"))
            self.assertEqual(stats["recovery"]["snapshot_status"], "discarded_restart")
            self.assertIn("digest", stats["recovery"]["snapshot_discarded_reason"])
            self._assert_equal_outputs(scene_path, reference, candidate)


if __name__ == "__main__":
    unittest.main()
