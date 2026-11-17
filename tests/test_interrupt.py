"""Real signal-delivery semantics: SIGINT graceful exit vs SIGKILL resume."""

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

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class InterruptSemanticsTests(unittest.TestCase):
    def setUp(self):
        self.base = tempfile.mkdtemp(prefix="sn_signal_")
        self.target = os.path.join(self.base, "configs")
        shutil.copytree(os.path.join(ROOT, "configs"), self.target)
        env = dict(os.environ)
        env["PYTHONPATH"] = ROOT
        self.env = env
        self.chunk_env = dict(env)
        # Deterministic hook: keep the process inside the chunk-rendering
        # window long enough for the test signal to land there.
        self.chunk_env["STAGE_NOISE_CHUNK_DELAY_MS"] = "120"
        self.manifest = os.path.join(self.target, "demo_manifest.json")
        subprocess.run([sys.executable, "-m", "stage_noise.cli", "generate",
                        self.manifest], env=env, check=True,
                       stdout=subprocess.DEVNULL)

    def tearDown(self):
        shutil.rmtree(self.base, ignore_errors=True)

    def _render_args(self):
        return [sys.executable, "-m", "stage_noise.cli", "render",
                self.manifest]

    def test_sigint_finishes_chunk_and_exits_zero(self):
        proc = subprocess.Popen(self._render_args(), cwd=self.base,
                                env=self.chunk_env, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True)
        time.sleep(1.0)
        proc.send_signal(signal.SIGINT)
        stdout, stderr = proc.communicate(timeout=30)
        self.assertEqual(proc.returncode, 0, msg=stderr)
        self.assertIn("interrupted_resumable", stdout)
        snapshot = os.path.join(self.target, ".render_state",
                                "snapshot.json")
        self.assertTrue(os.path.isfile(snapshot))
        recovery = os.path.join(self.target, "recovery_report.json")
        self.assertTrue(os.path.isfile(recovery))
        with open(recovery, encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertEqual(payload["status"], "interrupted_resumable")
        self.assertGreater(payload["completed_chunks"], 0)

        resumed = subprocess.run(self._render_args(), cwd=self.base,
                                 env=self.chunk_env, capture_output=True,
                                 text=True)
        self.assertEqual(resumed.returncode, 0, msg=resumed.stderr)
        self.assertTrue(os.path.isfile(
            os.path.join(self.target, "master_stereo.wav")))

    def test_sigkill_resumes_without_exit_report(self):
        recovery = os.path.join(self.target, "recovery_report.json")
        if os.path.exists(recovery):
            os.remove(recovery)
        proc = subprocess.Popen(self._render_args(), cwd=self.base,
                                env=self.chunk_env, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL)
        time.sleep(1.0)
        proc.send_signal(signal.SIGKILL)
        proc.wait()
        snapshot = os.path.join(self.target, ".render_state",
                                "snapshot.json")
        self.assertTrue(os.path.isfile(snapshot))
        # Forced termination makes no exit/recovery report promises.
        self.assertFalse(os.path.isfile(recovery))
        resumed = subprocess.run(self._render_args(), cwd=self.base,
                                 env=self.chunk_env, capture_output=True,
                                 text=True)
        self.assertEqual(resumed.returncode, 0, msg=resumed.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
