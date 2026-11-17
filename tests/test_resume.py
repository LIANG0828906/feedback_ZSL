from __future__ import annotations

import json
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

import numpy as np

from serene_render.compare import compare_outputs
from serene_render.pipeline import run_render
from serene_render.scene import write_sample_scene


ROOT = Path(__file__).resolve().parents[1]


class ResumeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.scene_path = self.root / "scene.json"
        write_sample_scene(self.scene_path)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_interrupt_finishes_tile_and_resume_matches_baseline(self) -> None:
        baseline = self.root / "baseline"
        resumed = self.root / "resumed"
        run_render(self.scene_path, baseline, fresh=True)

        process = subprocess.Popen(
            [
                sys.executable,
                str(ROOT / "render.py"),
                "render",
                "--scene",
                str(self.scene_path),
                "--out",
                str(resumed),
                "--fresh",
                "--block-delay-ms",
                "60",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        interrupted_at = None
        for _ in range(300):
            time.sleep(0.01)
            checkpoint_path = resumed / "checkpoint.json"
            if checkpoint_path.exists():
                try:
                    checkpoint = json.loads(checkpoint_path.read_text())
                except (OSError, json.JSONDecodeError):
                    continue
                completed = int(checkpoint.get("completed_count", 0))
                if 1 <= completed < int(checkpoint["image"]["tile_count"]):
                    process.send_signal(signal.SIGINT)
                    interrupted_at = completed
                    break
            if process.poll() is not None:
                break
        self.assertIsNotNone(interrupted_at, "renderer finished before the test interrupt")
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0, stderr)
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "interrupted")
        recovery = json.loads((resumed / "recovery_report.json").read_text())
        self.assertEqual(recovery["status"], "interrupted")
        self.assertEqual(recovery["completed_count"], recovery["next_block"])
        self.assertGreaterEqual(recovery["completed_count"], interrupted_at)
        self.assertGreater(recovery["completed_count"], 0)
        self.assertLess(recovery["completed_count"], recovery["tile_count"])

        final = run_render(
            self.scene_path,
            resumed,
            verify_baseline=baseline,
        )
        self.assertEqual(final.status, "complete")
        self.assertEqual(compare_outputs(baseline, resumed), [])
        self.assertEqual(
            (baseline / "render_rgba.npy").read_bytes(),
            (resumed / "render_rgba.npy").read_bytes(),
        )

    def test_corrupt_snapshot_is_discarded_and_rerendered(self) -> None:
        baseline = self.root / "baseline"
        damaged = self.root / "damaged"
        run_render(self.scene_path, baseline, fresh=True)
        run_render(self.scene_path, damaged, fresh=True)
        checkpoint_path = damaged / "checkpoint.json"
        checkpoint = json.loads(checkpoint_path.read_text())
        checkpoint["completed_blocks"][0]["rgba_sha256"] = "0" * 64
        checkpoint_path.write_text(json.dumps(checkpoint), encoding="utf-8")
        result = run_render(self.scene_path, damaged)
        self.assertEqual(result.status, "complete")
        recovery = json.loads((damaged / "recovery_report.json").read_text())
        self.assertEqual(recovery["resume_status"], "discarded")
        self.assertEqual(compare_outputs(baseline, damaged), [])

    @unittest.skipUnless(hasattr(signal, "SIGKILL"), "SIGKILL is not available")
    def test_forced_kill_restarts_only_from_committed_prefix(self) -> None:
        baseline = self.root / "kill_baseline"
        interrupted = self.root / "kill_resume"
        run_render(self.scene_path, baseline, fresh=True)
        process = subprocess.Popen(
            [
                sys.executable,
                str(ROOT / "render.py"),
                "render",
                "--scene",
                str(self.scene_path),
                "--out",
                str(interrupted),
                "--fresh",
                "--block-delay-ms",
                "50",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        committed = None
        for _ in range(300):
            time.sleep(0.01)
            checkpoint_path = interrupted / "checkpoint.json"
            if checkpoint_path.exists():
                try:
                    checkpoint = json.loads(checkpoint_path.read_text())
                except (OSError, json.JSONDecodeError):
                    continue
                count = int(checkpoint.get("completed_count", 0))
                if 1 <= count < int(checkpoint["image"]["tile_count"]):
                    committed = count
                    process.kill()
                    break
        self.assertIsNotNone(committed)
        process.communicate(timeout=20)
        self.assertLess(process.returncode, 0)
        checkpoint = json.loads((interrupted / "checkpoint.json").read_text())
        self.assertEqual(checkpoint["completed_count"], committed)
        result = run_render(
            self.scene_path,
            interrupted,
            verify_baseline=baseline,
        )
        self.assertEqual(result.status, "complete")
        self.assertEqual(compare_outputs(baseline, interrupted), [])

    def test_changed_scene_digest_discards_old_offsets(self) -> None:
        first = self.root / "first"
        run_render(self.scene_path, first, fresh=True)
        raw = json.loads(self.scene_path.read_text())
        raw["exposure"] = 0.90
        self.scene_path.write_text(json.dumps(raw), encoding="utf-8")
        result = run_render(self.scene_path, first)
        self.assertEqual(result.status, "complete")
        recovery = json.loads((first / "recovery_report.json").read_text())
        self.assertEqual(recovery["resume_status"], "discarded")
        self.assertIn("scene digest", recovery["resume_reason"])

    def test_compare_reports_block_pixel_and_channel_on_mismatch(self) -> None:
        reference = self.root / "reference"
        candidate = self.root / "candidate"
        run_render(self.scene_path, reference, fresh=True)
        run_render(self.scene_path, candidate, fresh=True)
        image_path = candidate / "render_rgba.npy"
        array = np.load(image_path, allow_pickle=False)
        before = int(array[10, 70, 1])
        array[10, 70, 1] = (before + 1) % 256
        np.save(image_path, array, allow_pickle=False)
        differences = compare_outputs(reference, candidate)
        self.assertTrue(differences)
        pixel_difference = next(
            difference
            for difference in differences
            if difference.get("pixel") == [70, 10]
        )
        self.assertEqual(pixel_difference["block_index"], 1)
        self.assertIn("G", pixel_difference["channel"])
        on_disk = json.loads((candidate / "resume_diff.json").read_text())
        self.assertEqual(on_disk["code"], "RESUME_OUTPUT_MISMATCH")
        completed = subprocess.run(
            [
                sys.executable,
                str(ROOT / "render.py"),
                "compare",
                "--reference",
                str(reference),
                "--candidate",
                str(candidate),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
        )
        self.assertEqual(completed.returncode, 4)
        self.assertEqual(json.loads(completed.stderr)["code"], "RESUME_OUTPUT_MISMATCH")


if __name__ == "__main__":
    unittest.main()
