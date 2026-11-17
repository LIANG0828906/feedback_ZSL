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

from drum_align.fixture import generate_demo


REPO_ROOT = Path(__file__).resolve().parents[1]


@unittest.skipIf(os.name == "nt", "POSIX signal delivery is exercised on Unix CI")
class ProcessInterruptTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory(prefix="drum-signal-test-")
        cls.root = Path(cls._temporary.name)
        cls.project = generate_demo(cls.root / "fixture", duration_seconds=26.0)
        data = json.loads(cls.project.read_text(encoding="utf-8"))
        data["render"]["block_samples"] = 2_048
        cls.project.write_text(
            json.dumps(data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def _start(self, name: str) -> tuple[subprocess.Popen[str], Path, Path, Path]:
        directory = self.root / name
        directory.mkdir(parents=True, exist_ok=True)
        master = directory / "master.wav"
        report = directory / "report.json"
        state = directory / "state.json"
        env = dict(os.environ)
        env["PYTHONPATH"] = str(REPO_ROOT)
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "drum_align",
                "render",
                "--project",
                str(self.project),
                "--master",
                str(master),
                "--report",
                str(report),
                "--snapshot",
                str(state),
            ],
            cwd=REPO_ROOT,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        return process, master, report, state

    def _wait_for_state(self, state: Path, process: subprocess.Popen[str]) -> None:
        deadline = time.monotonic() + 15.0
        while time.monotonic() < deadline:
            if state.exists() and state.stat().st_size > 0:
                return
            if process.poll() is not None:
                self.fail(f"render exited before checkpoint: {process.returncode}")
            time.sleep(0.005)
        self.fail("timed out waiting for a persisted checkpoint")

    def _oneshot_hash(self, name: str) -> bytes:
        directory = self.root / name
        directory.mkdir(parents=True, exist_ok=True)
        master = directory / "master.wav"
        report = directory / "report.json"
        state = directory / "state.json"
        process = subprocess.run(
            [
                sys.executable,
                "-m",
                "drum_align",
                "render",
                "--project",
                str(self.project),
                "--master",
                str(master),
                "--report",
                str(report),
                "--snapshot",
                str(state),
                "--oneshot",
            ],
            cwd=REPO_ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(process.returncode, 0, process.stderr)
        return master.read_bytes()

    def test_ctrl_c_finishes_block_and_exits_zero(self) -> None:
        process, master, report, state = self._start("sigint")
        self._wait_for_state(state, process)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=20)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertFalse(master.exists())
        self.assertTrue(state.exists())
        summary = json.loads(stdout)
        self.assertEqual(summary["status"], "interrupted")
        recovery_report = json.loads(report.read_text(encoding="utf-8"))
        self.assertEqual(recovery_report["report_kind"], "recovery")
        self.assertGreater(recovery_report["render"]["completed_blocks"], 0)

        resume = subprocess.run(
            [
                sys.executable,
                "-m",
                "drum_align",
                "render",
                "--project",
                str(self.project),
                "--master",
                str(master),
                "--report",
                str(report),
                "--snapshot",
                str(state),
            ],
            cwd=REPO_ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(resume.returncode, 0, resume.stderr)
        self.assertEqual(master.read_bytes(), self._oneshot_hash("sigint-oneshot"))

    @unittest.skipUnless(hasattr(signal, "SIGKILL"), "SIGKILL is unavailable")
    def test_sigkill_resume_uses_only_checkpoint(self) -> None:
        process, master, _report, state = self._start("sigkill")
        self._wait_for_state(state, process)
        process.kill()
        process.communicate(timeout=20)
        self.assertNotEqual(process.returncode, 0)
        self.assertTrue(state.exists())
        self.assertFalse(master.exists())

        resume = subprocess.run(
            [
                sys.executable,
                "-m",
                "drum_align",
                "render",
                "--project",
                str(self.project),
                "--master",
                str(master),
                "--report",
                str(self.root / "sigkill" / "report.json"),
                "--snapshot",
                str(state),
            ],
            cwd=REPO_ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(resume.returncode, 0, resume.stderr)
        self.assertEqual(master.read_bytes(), self._oneshot_hash("sigkill-oneshot"))


if __name__ == "__main__":
    unittest.main()
