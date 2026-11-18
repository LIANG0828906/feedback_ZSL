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

from rosewindow.verify import compare_outputs

from tests.helpers import ROOT, run_cli, small_scene_raw, write_json


def _completed_snapshot_chunks(snapshot: Path) -> int:
    if not snapshot.exists():
        return 0
    try:
        with np.load(snapshot, allow_pickle=False) as archive:
            metadata = json.loads(archive["meta"].tobytes().decode("utf-8"))
        return len(metadata["completed_chunks"])
    except Exception:
        return 0


class ResumeTests(unittest.TestCase):
    def test_forced_kill_resume_is_byte_identical(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            write_json(scene_path, small_scene_raw())
            baseline = directory / "baseline"
            candidate = directory / "candidate"
            baseline_run = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(baseline),
            )
            self.assertEqual(baseline_run.returncode, 0, baseline_run.stderr)

            process = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "rosewindow",
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out-dir",
                    str(candidate),
                    "--chunk-delay-ms",
                    "30",
                ],
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            snapshot = candidate / ".rosewindow_snapshot" / "snapshot.npz"
            observed = 0
            deadline = time.time() + 30.0
            while time.time() < deadline:
                observed = _completed_snapshot_chunks(snapshot)
                if observed >= 3 or process.poll() is not None:
                    break
                time.sleep(0.01)
            self.assertIsNone(process.poll(), "renderer exited before forced kill")
            self.assertGreaterEqual(observed, 3)
            process.kill()
            process.communicate(timeout=10)
            self.assertNotEqual(process.returncode, 0)

            resumed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(candidate),
                "--reference",
                str(baseline),
                "--diff-output",
                str(candidate / "resume_diff.json"),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
            diff = json.loads((candidate / "resume_diff.json").read_text())
            self.assertTrue(diff["ok"], diff)
            comparison = compare_outputs(baseline, candidate)
            self.assertTrue(comparison["ok"], comparison)
            run_info = json.loads((candidate / "run.json").read_text())
            self.assertEqual(run_info["snapshot_load_status"], "resumed")
            self.assertGreater(run_info["resumed_chunk_count"], 0)

    def test_sigint_commits_current_chunk_and_exits_zero(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            write_json(scene_path, small_scene_raw())
            baseline = directory / "baseline"
            interrupted = directory / "interrupted"
            baseline_run = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(baseline),
            )
            self.assertEqual(baseline_run.returncode, 0, baseline_run.stderr)
            popen_options = {}
            if os.name == "nt":
                popen_options["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
            process = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "rosewindow",
                    "render",
                    "--scene",
                    str(scene_path),
                    "--out-dir",
                    str(interrupted),
                    "--chunk-delay-ms",
                    "30",
                ],
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                **popen_options,
            )
            snapshot = interrupted / ".rosewindow_snapshot" / "snapshot.npz"
            deadline = time.time() + 30.0
            observed = 0
            while time.time() < deadline:
                observed = _completed_snapshot_chunks(snapshot)
                if observed >= 3 or process.poll() is not None:
                    break
                time.sleep(0.01)
            self.assertGreaterEqual(observed, 3)
            if os.name == "nt":
                process.send_signal(signal.CTRL_C_EVENT)
            else:
                process.send_signal(signal.SIGINT)
            stdout, stderr = process.communicate(timeout=20)
            self.assertEqual(process.returncode, 0, stdout + stderr)
            summary = json.loads(stdout)
            self.assertEqual(summary["status"], "interrupted")
            recovery = json.loads(
                (interrupted / "recovery_report.json").read_text()
            )
            self.assertEqual(recovery["exit_code"], 0)
            self.assertTrue(recovery["snapshot_committed"])
            self.assertGreaterEqual(recovery["completed_chunk_count"], 3)
            self.assertFalse((interrupted / "rgb.npy").exists())
            resumed = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(interrupted),
                "--reference",
                str(baseline),
                "--diff-output",
                str(interrupted / "resume_diff.json"),
            )
            self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
            self.assertTrue(
                json.loads((interrupted / "resume_diff.json").read_text())["ok"]
            )

    def test_corrupt_snapshot_is_discarded(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            write_json(scene_path, small_scene_raw())
            output = directory / "render"
            first = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(output),
            )
            self.assertEqual(first.returncode, 0, first.stderr)
            snapshot = output / ".rosewindow_snapshot" / "snapshot.npz"
            snapshot.write_bytes(b"not a valid snapshot")
            second = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(output),
            )
            self.assertEqual(second.returncode, 0, second.stderr)
            run_info = json.loads((output / "run.json").read_text())
            self.assertEqual(
                run_info["snapshot_load_status"], "discarded_corrupt"
            )
            self.assertEqual(
                run_info["completed_chunk_count"],
                len(json.loads((output / "report.json").read_text())["chunks"]["summaries"]),
            )

    def test_scene_digest_mismatch_discards_snapshot(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            scene_path = directory / "scene.json"
            changed_scene_path = directory / "scene_changed.json"
            output = directory / "render"
            raw = small_scene_raw()
            write_json(scene_path, raw)
            first = run_cli(
                "render",
                "--scene",
                str(scene_path),
                "--out-dir",
                str(output),
            )
            self.assertEqual(first.returncode, 0, first.stderr)
            changed = small_scene_raw()
            changed["name"] = "changed-scene-digest"
            write_json(changed_scene_path, changed)
            second = run_cli(
                "render",
                "--scene",
                str(changed_scene_path),
                "--out-dir",
                str(output),
            )
            self.assertEqual(second.returncode, 0, second.stderr)
            run_info = json.loads((output / "run.json").read_text())
            self.assertEqual(
                run_info["snapshot_load_status"],
                "discarded_scene_mismatch",
            )
            self.assertEqual(
                len(run_info["computed_chunks_this_run"]),
                run_info["total_chunk_count"],
            )


if __name__ == "__main__":
    unittest.main()
