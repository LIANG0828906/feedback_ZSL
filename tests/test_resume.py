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

import numpy as np

from gallery_louvers.compare import compare_runs
from gallery_louvers.runner import run_render

from .helpers import small_scene_raw, write_scene


ROOT = Path(__file__).resolve().parents[1]


class ResumeTests(unittest.TestCase):
    def test_interrupted_resume_is_byte_identical_to_uninterrupted(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = write_scene(root / "scene.json", small_scene_raw())
            reference = root / "reference"
            candidate = root / "candidate"

            completed = run_render(scene_path, reference, quiet=True)
            self.assertEqual(completed["status"], "complete")

            interrupted = run_render(
                scene_path,
                candidate,
                stop_after_blocks=2,
                quiet=True,
            )
            self.assertEqual(interrupted["status"], "interrupted")
            self.assertTrue((candidate / "recovery.json").is_file())
            snapshot = json.loads((candidate / "snapshot.json").read_text())
            self.assertEqual(snapshot["completed_blocks"], 2)

            resumed = run_render(scene_path, candidate, quiet=True)
            self.assertEqual(resumed["status"], "complete")
            self.assertGreaterEqual(int(resumed["reused_blocks"]), 2)
            self.assertFalse((candidate / "recovery.json").exists())
            comparison = compare_runs(reference, candidate)
            self.assertTrue(comparison["identical"], comparison["differences"][:5])
            self.assertEqual(comparison["exit_code"], 0)

            altered_path = (
                candidate
                / "renders"
                / "angle_000_rgba_premultiplied_linear.npy"
            )
            altered = np.load(altered_path)
            altered[10, 20, 0] += 0.25
            np.save(altered_path, altered)
            mismatch = compare_runs(reference, candidate)
            self.assertFalse(mismatch["identical"])
            self.assertEqual(mismatch["exit_code"], 4)
            self.assertTrue(
                any(
                    diff["angle"] == 0
                    and diff["pixel"] == [20, 10]
                    and diff["channel"] == "R"
                    for diff in mismatch["differences"]
                )
            )

    def test_corrupt_snapshot_discards_old_blocks(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = write_scene(root / "scene.json", small_scene_raw())
            workdir = root / "run"
            run_render(
                scene_path, workdir, stop_after_blocks=1, quiet=True
            )
            old_tile = workdir / "blocks" / "angle_000" / "block_000.gbt"
            self.assertTrue(old_tile.exists())
            (workdir / "snapshot.json").write_text("{broken", encoding="utf-8")
            result = run_render(
                scene_path, workdir, stop_after_blocks=1, quiet=True
            )
            reset = json.loads((workdir / "resume_reset.json").read_text())
            self.assertEqual(result["reused_blocks"], 0)
            self.assertIn("cannot read snapshot", reset["reason"])

    def test_seed_change_discards_progress(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw = small_scene_raw()
            scene_path = write_scene(root / "scene.json", raw)
            workdir = root / "run"
            run_render(scene_path, workdir, stop_after_blocks=1, quiet=True)
            changed = small_scene_raw()
            changed["leaves"][0]["seed"] += 1
            changed_path = write_scene(root / "changed.json", changed)
            result = run_render(
                changed_path, workdir, stop_after_blocks=1, quiet=True
            )
            self.assertEqual(result["reused_blocks"], 0)
            reset = json.loads((workdir / "resume_reset.json").read_text())
            self.assertIn("seed", reset["reason"].lower())

    def test_real_sigint_commits_current_block_and_exits_zero(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = ROOT / "samples" / "scene.json"
            workdir = root / "signal"
            process = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "gallery_louvers",
                    "render",
                    str(scene_path),
                    "--out",
                    str(workdir),
                    "--quiet",
                ],
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            time.sleep(2.0)
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
            stdout, stderr = process.communicate(timeout=60)
            self.assertEqual(
                process.returncode,
                0,
                f"stdout={stdout}\nstderr={stderr}",
            )
            if (workdir / "recovery.json").exists():
                recovery = json.loads(
                    (workdir / "recovery.json").read_text(encoding="utf-8")
                )
                self.assertEqual(recovery["exit_code"], 0)
                self.assertTrue(recovery["current_block_completed"])
                snapshot = json.loads(
                    (workdir / "snapshot.json").read_text(encoding="utf-8")
                )
                self.assertGreaterEqual(snapshot["completed_blocks"], 1)
                self.assertLess(
                    snapshot["completed_blocks"], snapshot["expected"]["total_blocks"]
                )

    def test_sigkill_restart_uses_only_committed_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw = small_scene_raw(width=160, height=120, angles=[-30.0, 0.0])
            scene_path = write_scene(root / "scene.json", raw)
            reference = root / "reference"
            candidate = root / "candidate"
            run_render(scene_path, reference, quiet=True)

            process = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "gallery_louvers",
                    "render",
                    str(scene_path),
                    "--out",
                    str(candidate),
                    "--quiet",
                ],
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            committed = 0
            deadline = time.time() + 20.0
            while time.time() < deadline and process.poll() is None:
                snapshot_path = candidate / "snapshot.json"
                if snapshot_path.exists():
                    try:
                        snapshot = json.loads(snapshot_path.read_text(encoding="utf-8"))
                        committed = int(snapshot.get("completed_blocks", 0))
                    except (json.JSONDecodeError, OSError):
                        pass
                if committed >= 2:
                    break
                time.sleep(0.02)
            self.assertGreaterEqual(committed, 2)
            process.kill()
            process.communicate(timeout=20)
            self.assertNotEqual(process.returncode, 0)
            self.assertFalse((candidate / "recovery.json").exists())

            resumed = run_render(scene_path, candidate, quiet=True)
            self.assertEqual(resumed["status"], "complete")
            self.assertGreaterEqual(int(resumed["reused_blocks"]), 2)
            comparison = compare_runs(reference, candidate)
            self.assertTrue(
                comparison["identical"], comparison["differences"][:5]
            )


if __name__ == "__main__":
    unittest.main()
