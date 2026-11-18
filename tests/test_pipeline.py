from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "elytron_render.py"


def _run(command: list[str], timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        command,
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def _snapshot_count(out: Path) -> int:
    try:
        envelope = json.loads((out / "progress_snapshot.json").read_text(encoding="utf-8"))
        return int(envelope["payload"]["next_block_index"])
    except (OSError, KeyError, ValueError, json.JSONDecodeError):
        return -1


class PipelineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.scene = cls.root / "scene.json"
        generated = _run(
            [sys.executable, str(SCRIPT), "generate", "--seed", "254", "--out", str(cls.scene)]
        )
        if generated.returncode != 0:
            raise RuntimeError(generated.stderr)
        cls.baseline = cls.root / "baseline"
        rendered = _run(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(cls.scene),
                "--out",
                str(cls.baseline),
            ],
            timeout=90,
        )
        if rendered.returncode != 0:
            raise RuntimeError(rendered.stdout + rendered.stderr)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def _start_interruptible(self, out: Path) -> subprocess.Popen[str]:
        return subprocess.Popen(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(self.scene),
                "--out",
                str(out),
                "--block-delay-ms",
                "80",
            ],
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def _wait_for_blocks(self, process: subprocess.Popen[str], out: Path, count: int) -> int:
        deadline = time.time() + 20.0
        latest = -1
        while time.time() < deadline:
            latest = _snapshot_count(out)
            if latest >= count:
                return latest
            if process.poll() is not None:
                break
            time.sleep(0.01)
        self.fail(f"renderer did not commit {count} blocks; last snapshot={latest}")

    def test_01_uninterrupted_runs_are_byte_identical(self) -> None:
        second = self.root / "second"
        result = _run(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(self.scene),
                "--out",
                str(second),
            ],
            timeout=90,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        names = sorted(
            path.name
            for path in self.baseline.iterdir()
            if path.suffix in {".npy", ".png"} or path.name in {"report.json", "progress_snapshot.json"}
        )
        self.assertGreater(len(names), 20)
        for name in names:
            self.assertEqual(
                (self.baseline / name).read_bytes(),
                (second / name).read_bytes(),
                name,
            )

    def test_02_forced_kill_resume_matches_baseline(self) -> None:
        interrupted = self.root / "killed"
        process = self._start_interruptible(interrupted)
        committed = self._wait_for_blocks(process, interrupted, 5)
        process.kill()
        process.communicate(timeout=10)
        self.assertNotEqual(process.returncode, 0)
        self.assertGreaterEqual(committed, 5)
        final_committed = _snapshot_count(interrupted)
        self.assertGreaterEqual(final_committed, committed)
        self.assertEqual(list(interrupted.glob("*.npy")), [])

        resumed = _run(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(self.scene),
                "--out",
                str(interrupted),
                "--resume",
                "--baseline",
                str(self.baseline),
            ],
            timeout=90,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        resume_report = json.loads((interrupted / "resume_report.json").read_text(encoding="utf-8"))
        self.assertEqual(resume_report["action"], "resumed")
        self.assertEqual(resume_report["restored_blocks"], final_committed)
        difference = json.loads((interrupted / "difference_report.json").read_text(encoding="utf-8"))
        self.assertEqual(difference["status"], "identical")
        self.assertEqual(
            (interrupted / "report.json").read_bytes(),
            (self.baseline / "report.json").read_bytes(),
        )

    def test_03_sigint_finishes_block_and_exits_zero(self) -> None:
        interrupted = self.root / "sigint"
        process = self._start_interruptible(interrupted)
        committed = self._wait_for_blocks(process, interrupted, 4)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=15)
        self.assertEqual(process.returncode, 0, stdout + stderr)
        self.assertEqual(list(interrupted.glob("*.npy")), [])
        recovery = json.loads((interrupted / "interrupt_report.json").read_text(encoding="utf-8"))
        self.assertEqual(recovery["status"], "interrupted")
        self.assertEqual(recovery["exit_code"], 0)
        self.assertTrue(recovery["snapshot_committed"])
        self.assertGreaterEqual(recovery["completed_block_count"], committed)
        snapshots = json.loads(
            (interrupted / "progress_snapshot.json").read_text(encoding="utf-8")
        )
        self.assertEqual(
            snapshots["payload"]["next_block_index"],
            recovery["completed_block_count"],
        )
        resumed = _run(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(self.scene),
                "--out",
                str(interrupted),
                "--resume",
                "--baseline",
                str(self.baseline),
            ],
            timeout=90,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        difference = json.loads((interrupted / "difference_report.json").read_text(encoding="utf-8"))
        self.assertEqual(difference["status"], "identical")

    def test_04_mismatch_writes_structured_diff_and_exits_three(self) -> None:
        mismatch = self.root / "mismatch"
        bad_baseline = self.root / "bad_baseline"
        shutil.copytree(self.baseline, mismatch)
        shutil.copytree(self.baseline, bad_baseline)
        image_path = bad_baseline / "C00.rgba.npy"
        array = np.load(image_path, allow_pickle=False)
        array[0, 0, 0] ^= 1
        np.save(image_path, array, allow_pickle=False)
        result = _run(
            [
                sys.executable,
                str(SCRIPT),
                "render",
                "--scene",
                str(self.scene),
                "--out",
                str(mismatch),
                "--resume",
                "--baseline",
                str(bad_baseline),
            ],
            timeout=60,
        )
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        difference = json.loads((mismatch / "difference_report.json").read_text(encoding="utf-8"))
        self.assertEqual(difference["status"], "different")
        self.assertGreater(difference["difference_count"], 0)
        pixel_diff = next(
            record
            for record in difference["differences"]
            if record["kind"] == "image_pixel"
        )
        self.assertIn("block_id", pixel_diff)
        self.assertTrue(pixel_diff["block_id"].startswith("C00/"))
        self.assertEqual(pixel_diff["pixel"], [0, 0])
        self.assertIn(pixel_diff["channel"], {"R", "G", "B", "A"})


if __name__ == "__main__":
    unittest.main()
