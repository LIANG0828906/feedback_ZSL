from __future__ import annotations

import hashlib
import json
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from drum_bus.synth import DemoConfig, generate_demo_project


ROOT = Path(__file__).resolve().parents[1]
RECOVERY_CONFIG = DemoConfig(
    duration_seconds=1.5,
    reference_times=(0.2, 0.5, 0.8, 1.1),
    block_size=512,
    target_lufs=-16.0,
    true_peak_db=-1.0,
)


def _hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _command(manifest: Path, output: Path, delay_ms: float = 0.0) -> list[str]:
    command = [
        sys.executable,
        "-m",
        "drum_bus",
        "render",
        "--manifest",
        str(manifest),
        "--output",
        str(output),
        "--block-size",
        "512",
    ]
    if delay_ms > 0.0:
        command.extend(["--block-delay-ms", str(delay_ms)])
    return command


def _wait_for_blocks(snapshot: Path, minimum: int, process: subprocess.Popen) -> dict:
    deadline = time.time() + 30.0
    while time.time() < deadline:
        try:
            value = json.loads(snapshot.read_text(encoding="utf-8"))
            if value["completed_blocks"] >= minimum and value["completed_blocks"] < value[
                "total_blocks"
            ]:
                return value
        except (FileNotFoundError, json.JSONDecodeError, KeyError):
            pass
        if process.poll() is not None:
            break
        time.sleep(0.01)
    raise AssertionError("render did not commit expected blocks in time")


class RecoveryTests(unittest.TestCase):
    def test_ctrl_c_commits_current_block_and_exits_zero(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = generate_demo_project(root / "case", RECOVERY_CONFIG)
            full = root / "full"
            subprocess.run(_command(manifest, full), cwd=ROOT, check=True, capture_output=True)
            partial = root / "partial"
            process = subprocess.Popen(
                _command(manifest, partial, delay_ms=5.0),
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            snapshot_path = partial / ".render_state" / "snapshot.json"
            committed = _wait_for_blocks(snapshot_path, 2, process)
            process.send_signal(signal.SIGINT)
            stdout, stderr = process.communicate(timeout=60)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertIn('"status": "interrupted"', stdout)
            recovery = json.loads((partial / "recovery_report.json").read_text())
            self.assertEqual(recovery["exit_code"], 0)
            committed_after = json.loads(snapshot_path.read_text())["completed_blocks"]
            self.assertGreaterEqual(committed_after, committed["completed_blocks"])
            subprocess.run(_command(manifest, partial), cwd=ROOT, check=True, capture_output=True)
            self.assertEqual(_hash(partial / "bus.wav"), _hash(full / "bus.wav"))

    def test_sigkill_has_no_report_and_resume_is_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = generate_demo_project(root / "case", RECOVERY_CONFIG)
            full = root / "full"
            subprocess.run(_command(manifest, full), cwd=ROOT, check=True, capture_output=True)
            partial = root / "killed"
            process = subprocess.Popen(
                _command(manifest, partial, delay_ms=5.0),
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            snapshot_path = partial / ".render_state" / "snapshot.json"
            _wait_for_blocks(snapshot_path, 2, process)
            process.kill()
            process.communicate(timeout=10)
            self.assertNotEqual(process.returncode, 0)
            self.assertFalse((partial / "recovery_report.json").exists())
            subprocess.run(_command(manifest, partial), cwd=ROOT, check=True, capture_output=True)
            self.assertEqual(_hash(partial / "bus.wav"), _hash(full / "bus.wav"))
            snapshot = json.loads(snapshot_path.read_text())
            self.assertEqual(snapshot["status"], "complete")

    def test_manifest_change_discards_old_progress(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = generate_demo_project(root / "case", RECOVERY_CONFIG)
            partial = root / "partial"
            process = subprocess.Popen(
                _command(manifest, partial, delay_ms=5.0),
                cwd=ROOT,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            snapshot_path = partial / ".render_state" / "snapshot.json"
            old_snapshot = _wait_for_blocks(snapshot_path, 2, process)
            process.kill()
            process.communicate(timeout=10)
            old_digest = old_snapshot["manifest_digest"]
            manifest_value = json.loads(manifest.read_text(encoding="utf-8"))
            manifest_value["master"]["target_lufs"] = -15.0
            manifest.write_text(
                json.dumps(manifest_value, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            subprocess.run(_command(manifest, partial), cwd=ROOT, check=True, capture_output=True)
            new_snapshot = json.loads(snapshot_path.read_text())
            self.assertNotEqual(new_snapshot["manifest_digest"], old_digest)
            self.assertEqual(new_snapshot["status"], "complete")
            report = json.loads((partial / "render_report.json").read_text())
            self.assertEqual(report["master"]["target_lufs"], -15.0)
            self.assertLessEqual(abs(report["master"]["deviation_lu"]), 0.5)


if __name__ == "__main__":
    unittest.main()
