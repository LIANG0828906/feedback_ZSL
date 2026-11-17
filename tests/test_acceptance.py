from __future__ import annotations

import hashlib
import json
import re
import shutil
import signal
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from biomemaster.report import verify_report
from tests.support import ENV, render_command
from tools.generate_sample import generate


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class AcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temporary.name)
        cls.fixture = cls.root / "fixture"
        generate(cls.fixture, 4.0)
        cls.reference = cls.root / "reference"
        cls.reference.mkdir()
        shutil.copytree(cls.fixture / "layers", cls.reference / "layers")
        shutil.copy2(cls.fixture / "project.json", cls.reference / "project.json")
        result = subprocess.run(
            render_command(
                cls.reference / "project.json",
                cls.reference / "master.wav",
                cls.reference / "qc.json",
                cls.reference / "work",
                1,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        if result.returncode != 0:
            raise RuntimeError(f"reference render failed: {result.stderr}")

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def _copy_fixture(self, name: str) -> Path:
        destination = self.root / name
        destination.mkdir()
        shutil.copytree(self.fixture / "layers", destination / "layers")
        shutil.copy2(self.fixture / "project.json", destination / "project.json")
        return destination

    def _start(
        self, directory: Path, workers: int, delay_ms: int
    ) -> subprocess.Popen[str]:
        environment = ENV.copy()
        environment["BIOMEMASTER_CHUNK_DELAY_MS"] = str(delay_ms)
        return subprocess.Popen(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                workers,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def _wait_for_chunks(
        self, checkpoint: Path, count: int, process: subprocess.Popen[str]
    ) -> int:
        deadline = time.time() + 20.0
        completed = 0
        while time.time() < deadline:
            if checkpoint.exists():
                try:
                    completed = int(
                        json.loads(checkpoint.read_text(encoding="utf-8"))[
                            "completed_chunks"
                        ]
                    )
                except (OSError, json.JSONDecodeError, KeyError):
                    completed = 0
                if completed >= count:
                    return completed
            if process.poll() is not None:
                break
            time.sleep(0.01)
        raise AssertionError(f"checkpoint did not reach {count} chunks")

    def test_parallel_biomes_are_byte_deterministic(self) -> None:
        directory = self._copy_fixture("parallel")
        result = subprocess.run(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                4,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            _digest(self.reference / "master.wav"),
            _digest(directory / "master.wav"),
        )
        report = json.loads((directory / "qc.json").read_text(encoding="utf-8"))
        self.assertLessEqual(abs(report["integrated_loudness_lufs"] - (-23.0)), 0.3)
        self.assertLessEqual(report["true_peak_dbtp"], -1.0)
        self.assertGreater(report["limiter_trigger_samples"], 0)
        self.assertGreater(report["crossfade_count"], 0)
        self.assertFalse((directory / "work").exists())

    def test_sigint_checkpoints_and_resume_is_byte_identical(self) -> None:
        directory = self._copy_fixture("sigint")
        process = self._start(directory, 3, 35)
        completed = self._wait_for_chunks(
            directory / "work" / "checkpoint.json", 2, process
        )
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertIn('"status":"interrupted"', stdout)
        checkpoint = json.loads(
            (directory / "work" / "checkpoint.json").read_text(encoding="utf-8")
        )
        self.assertGreaterEqual(checkpoint["completed_chunks"], completed)
        resumed = subprocess.run(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                2,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            _digest(self.reference / "master.wav"),
            _digest(directory / "master.wav"),
        )
        report = json.loads((directory / "qc.json").read_text(encoding="utf-8"))
        self.assertEqual(report["checkpoint_resume_count"], 1)
        self.assertFalse((directory / "work").exists())

    def test_force_kill_recovers_from_persisted_checkpoint(self) -> None:
        directory = self._copy_fixture("sigkill")
        process = self._start(directory, 2, 35)
        self._wait_for_chunks(directory / "work" / "checkpoint.json", 3, process)
        process.kill()
        process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGKILL)
        resumed = subprocess.run(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                4,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            _digest(self.reference / "master.wav"),
            _digest(directory / "master.wav"),
        )
        report = json.loads((directory / "qc.json").read_text(encoding="utf-8"))
        self.assertGreaterEqual(report["checkpoint_resume_count"], 1)

    def test_inconsistent_loudness_intermediate_is_rejected(self) -> None:
        directory = self._copy_fixture("corrupt")
        process = self._start(directory, 2, 30)
        self._wait_for_chunks(directory / "work" / "checkpoint.json", 2, process)
        process.send_signal(signal.SIGINT)
        process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0)
        checkpoint_path = directory / "work" / "checkpoint.json"
        checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
        checkpoint["loudness"]["intermediate_lufs"] = -12.345678
        checkpoint_path.write_text(json.dumps(checkpoint), encoding="utf-8")
        result = subprocess.run(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                2,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stderr)["code"], "E_CHECKPOINT_CORRUPT")

    def test_malformed_checkpoint_is_discarded_and_restarted(self) -> None:
        directory = self._copy_fixture("malformed")
        process = self._start(directory, 2, 30)
        self._wait_for_chunks(directory / "work" / "checkpoint.json", 2, process)
        process.send_signal(signal.SIGINT)
        process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0)
        (directory / "work" / "checkpoint.json").write_text("{broken", encoding="utf-8")
        resumed = subprocess.run(
            render_command(
                directory / "project.json",
                directory / "master.wav",
                directory / "qc.json",
                directory / "work",
                2,
            ),
            cwd=Path(__file__).resolve().parents[1],
            env=ENV,
            capture_output=True,
            text=True,
            timeout=90,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            _digest(self.reference / "master.wav"),
            _digest(directory / "master.wav"),
        )
        report = json.loads((directory / "qc.json").read_text(encoding="utf-8"))
        self.assertEqual(report["checkpoint_resume_count"], 0)

    def test_qc_is_fixed_precision_and_verifies_against_wav(self) -> None:
        text = (self.reference / "qc.json").read_text(encoding="utf-8")
        float_literals = re.findall(r":\s*(-?\d+\.\d+)", text)
        self.assertGreater(len(float_literals), 5)
        self.assertTrue(all(len(item.split(".")[1]) == 6 for item in float_literals))
        result = verify_report(
            self.reference / "qc.json", self.reference / "master.wav"
        )
        self.assertTrue(result["ok"])


if __name__ == "__main__":
    unittest.main()
