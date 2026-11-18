from __future__ import annotations

import copy
import hashlib
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
from typing import Any

import numpy as np
import soundfile as sf

from wuchang.dsp import estimate_onset
from wuchang.generator import write_demo
from wuchang.manifest import (
    load_manifest,
    validate_audio_and_load,
)
from wuchang.process import process_channels

ROOT = Path(__file__).resolve().parents[1]
PYTHON = sys.executable


def run_cli(*args: str, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    process_env = dict(os.environ)
    if env:
        process_env.update(env)
    return subprocess.run(
        [PYTHON, "-m", "wuchang", *args],
        cwd=ROOT,
        env=process_env,
        text=True,
        capture_output=True,
        check=False,
    )


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def walk_strings(value: Any):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield from walk_strings(key)
            yield from walk_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from walk_strings(item)


class SystemAcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = Path(tempfile.mkdtemp(prefix=".wuchang-test-", dir=ROOT))
        cls.session = cls.temp / "session"
        cls.manifest_path, _ = write_demo(cls.session)
        cls.manifest = json.loads(cls.manifest_path.read_text(encoding="utf-8"))
        cls.one_shot = cls.temp / "one_shot.wav"
        cls.one_shot_report = cls.temp / "one_shot.report.json"
        result = run_cli(
            "render",
            "--manifest",
            str(cls.manifest_path),
            "--out",
            str(cls.one_shot),
            "--report",
            str(cls.one_shot_report),
            "--state-dir",
            str(cls.temp / ".state_one_shot"),
            "--fresh",
            "--quiet",
        )
        if result.returncode != 0:
            raise RuntimeError(result.stdout + result.stderr)
        cls.one_shot_bytes = cls.one_shot.read_bytes()
        cls.one_shot_report_data = json.loads(
            cls.one_shot_report.read_text(encoding="utf-8")
        )

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.temp, ignore_errors=True)

    def test_01_generation_is_byte_deterministic_and_offsets_recompute(self) -> None:
        before = {
            channel["file"]: digest(self.session / channel["file"])
            for channel in self.manifest["channels"]
        }
        generated = run_cli(
            "generate", "--manifest", str(self.manifest_path)
        )
        self.assertEqual(generated.returncode, 0, generated.stderr)
        after = {
            channel["file"]: digest(self.session / channel["file"])
            for channel in self.manifest["channels"]
        }
        self.assertEqual(before, after)

        for channel in self.manifest["channels"]:
            data, sample_rate = sf.read(
                self.session / channel["file"], dtype="float64"
            )
            physical_offset = int(channel["start_offset_samples"])
            detected = []
            for cue in channel["cues"]:
                center = (
                    round(float(cue) * sample_rate) + physical_offset
                )
                onset = estimate_onset(data, center, 8, 8)
                self.assertIsNotNone(onset, channel["name"])
                detected.append(int(onset) - center)
            self.assertTrue(
                all(abs(value) <= 1 for value in detected),
                (channel["name"], detected),
            )

    def test_02_alignment_and_reflection_targets(self) -> None:
        document, load_errors = load_manifest(self.manifest_path)
        self.assertEqual(load_errors, [])
        assert document is not None
        loaded, audio_errors = validate_audio_and_load(
            document, self.manifest_path
        )
        self.assertEqual(audio_errors, [])
        processed, warnings = process_channels(loaded, document)
        self.assertEqual(warnings, [])
        self.assertEqual(len(processed), 5)
        for channel in processed:
            self.assertEqual(channel["alignment_status"], "detected")
            self.assertLessEqual(
                channel["max_abs_deviation_samples"], 1, channel["name"]
            )
            self.assertEqual(
                channel["alignment_offset_samples"],
                -channel["declared_start_offset_samples"],
            )
            reflection = channel["reflection"]
            if reflection["enabled"]:
                self.assertEqual(
                    reflection["estimation_status"], "estimated"
                )
                self.assertGreaterEqual(reflection["reduction_db"], 12.0)
                self.assertLessEqual(
                    reflection["direct_peak_loss_db"], 1.0
                )
                declared = channel["reflection"]["declared_gain_db"]
                self.assertAlmostEqual(
                    reflection["estimated_gain_db"], declared, delta=0.05
                )

    def test_03_hard_validation_errors_are_structured_and_master_free(self) -> None:
        base = copy.deepcopy(self.manifest)
        for channel in base["channels"]:
            channel["file"] = (
                self.session / channel["file"]
            ).resolve().as_posix()

        missing = copy.deepcopy(base)
        missing["channels"][1]["file"] = (
            self.session / "audio" / "does-not-exist.wav"
        ).as_posix()

        duplicate = copy.deepcopy(base)
        duplicate["channels"][1]["instrument"] = duplicate["channels"][0][
            "instrument"
        ]

        cues = copy.deepcopy(base)
        cues["channels"][0]["cues"][0:2] = [0.9, 0.55]

        rate = copy.deepcopy(base)
        bad_rate = self.session / "audio" / "wrong_rate.wav"
        sf.write(bad_rate, np.zeros(4800), 44100, subtype="PCM_24")
        rate["channels"][0]["file"] = bad_rate.resolve().as_posix()

        bits = copy.deepcopy(base)
        bad_bits = self.session / "audio" / "wrong_bits.wav"
        sf.write(bad_bits, np.zeros(48000), 48000, subtype="PCM_16")
        bits["channels"][0]["file"] = bad_bits.resolve().as_posix()

        cases = {
            "missing": (missing, "MISSING_FILE", "daluo"),
            "duplicate": (duplicate, "DUPLICATE_INSTRUMENT", "daluo"),
            "cues": (cues, "INVALID_CUE_TIME", "bangu"),
            "rate": (rate, "SAMPLE_RATE_MISMATCH", "bangu"),
            "bits": (bits, "BIT_DEPTH_MISMATCH", "bangu"),
        }
        for label, (document, code, channel_name) in cases.items():
            manifest_path = self.temp / f"{label}.json"
            manifest_path.write_text(
                json.dumps(document, ensure_ascii=False), encoding="utf-8"
            )
            output = self.temp / f"{label}.wav"
            report_path = self.temp / f"{label}.report.json"
            result = run_cli(
                "render",
                "--manifest",
                str(manifest_path),
                "--out",
                str(output),
                "--report",
                str(report_path),
                "--state-dir",
                str(self.temp / f".state_{label}"),
                "--fresh",
                "--quiet",
            )
            self.assertNotEqual(result.returncode, 0, label)
            self.assertFalse(output.exists(), label)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "error")
            self.assertFalse(report["master_created"])
            self.assertIn(
                (code, channel_name),
                [
                    (item["code"], item["channel"])
                    for item in report["hard_errors"]
                ],
                label,
            )

    def test_04_one_shot_is_deterministic_and_delivery_compliant(self) -> None:
        second = self.temp / "second.wav"
        report_path = self.temp / "second.report.json"
        result = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(second),
            "--report",
            str(report_path),
            "--state-dir",
            str(self.temp / ".state_second"),
            "--fresh",
            "--quiet",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.one_shot_bytes, second.read_bytes())
        self.assertFalse((self.temp / ".state_second").exists())

        report = self.one_shot_report_data
        self.assertEqual(report["status"], "ok")
        self.assertEqual(
            list(report.keys()),
            [
                "status",
                "schema_version",
                "hard_errors",
                "warnings",
                "manifest",
                "resume",
                "channels",
                "master",
                "loudness",
                "render",
            ],
        )
        self.assertLessEqual(
            abs(report["loudness"]["deviation_lu"]), 0.5
        )
        self.assertLessEqual(
            report["loudness"]["true_peak_dbtp"],
            report["loudness"]["true_peak_limit_dbtp"],
        )
        self.assertEqual(report["master"]["frames"], 422400)
        self.assertEqual(report["master"]["channels"], 2)
        self.assertEqual(report["master"]["sample_rate"], 48000)
        self.assertEqual(report["master"]["bit_depth"], 24)
        limiter = report["loudness"]["limiter"]
        self.assertTrue(limiter["triggered"])
        self.assertEqual(
            limiter["lookahead_samples"],
            limiter["compensated_delay_samples"],
        )
        self.assertNotIn(
            "timestamp", {key.lower() for key in walk_strings(report)}
        )
        for text in walk_strings(report):
            self.assertFalse(text.startswith("/"), text)

    def test_05_sigkill_resume_is_byte_identical(self) -> None:
        output = self.temp / "resumed.wav"
        report_path = self.temp / "resumed.report.json"
        state_dir = self.temp / ".state_resume"
        killed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--fresh",
            "--quiet",
            env={"WUCHANG_TEST_KILL_AFTER_BLOCKS": "2"},
        )
        self.assertEqual(killed.returncode, 137)
        snapshot = json.loads(
            (state_dir / "state.json").read_text(encoding="utf-8")
        )
        self.assertEqual(snapshot["completed_blocks"], 2)
        self.assertEqual(snapshot["frames_done"], 8192)
        self.assertFalse(output.exists())

        resumed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--quiet",
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(self.one_shot_bytes, output.read_bytes())
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report["resume"]["resume_count"], 1)
        self.assertEqual(report["resume"]["blocks_reused"], 2)
        self.assertEqual(report["resume"]["events"], [])
        self.assertTrue(
            report["render"]["resume_master_byte_identical"]
        )
        self.assertFalse(state_dir.exists())

    def test_06_sigint_commits_snapshot_and_exits_zero(self) -> None:
        output = self.temp / "interrupted.wav"
        report_path = self.temp / "interrupted.report.json"
        recovery_path = report_path.with_name(
            report_path.stem + ".recovery.json"
        )
        state_dir = self.temp / ".state_interrupt"
        process = subprocess.Popen(
            [
                PYTHON,
                "-m",
                "wuchang",
                "render",
                "--manifest",
                str(self.manifest_path),
                "--out",
                str(output),
                "--report",
                str(report_path),
                "--state-dir",
                str(state_dir),
                "--fresh",
                "--quiet",
            ],
            cwd=ROOT,
            env={
                **os.environ,
                "WUCHANG_TEST_BLOCK_DELAY_MS": "30",
            },
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        snapshot_path = state_dir / "state.json"
        deadline = time.time() + 20.0
        while time.time() < deadline:
            if snapshot_path.exists():
                try:
                    snapshot = json.loads(
                        snapshot_path.read_text(encoding="utf-8")
                    )
                    if snapshot.get("completed_blocks", 0) >= 1:
                        break
                except (OSError, json.JSONDecodeError):
                    pass
            if process.poll() is not None:
                break
            time.sleep(0.02)
        self.assertIsNone(process.poll())
        os.kill(process.pid, signal.SIGINT)
        stdout, stderr = process.communicate(timeout=20.0)
        self.assertEqual(process.returncode, 0, stdout + stderr)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        recovery = json.loads(recovery_path.read_text(encoding="utf-8"))
        self.assertEqual(report["status"], "interrupted")
        self.assertEqual(report["exit_code"], 0)
        self.assertGreaterEqual(report["completed_blocks"], 1)
        self.assertEqual(report, recovery)
        self.assertFalse(output.exists())

        final = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--quiet",
        )
        self.assertEqual(final.returncode, 0, final.stderr)
        self.assertEqual(self.one_shot_bytes, output.read_bytes())
        self.assertFalse(state_dir.exists())

    def test_07_resume_mismatch_discards_old_progress(self) -> None:
        manifest_path = self.temp / "changed_manifest.json"
        changed = copy.deepcopy(self.manifest)
        changed["channels"][0]["file"] = (
            self.session / changed["channels"][0]["file"]
        ).resolve().as_posix()
        for channel in changed["channels"][1:]:
            channel["file"] = (
                self.session / channel["file"]
            ).resolve().as_posix()
        manifest_path.write_text(
            json.dumps(changed, ensure_ascii=False), encoding="utf-8"
        )
        output = self.temp / "changed.wav"
        report_path = self.temp / "changed.report.json"
        state_dir = self.temp / ".state_changed"
        killed = run_cli(
            "render",
            "--manifest",
            str(manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--fresh",
            "--quiet",
            env={"WUCHANG_TEST_KILL_AFTER_BLOCKS": "2"},
        )
        self.assertEqual(killed.returncode, 137)
        changed["target_loudness_lufs"] = -15.9
        manifest_path.write_text(
            json.dumps(changed, ensure_ascii=False), encoding="utf-8"
        )
        resumed = run_cli(
            "render",
            "--manifest",
            str(manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--quiet",
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report["resume"]["resume_count"], 0)
        self.assertEqual(report["resume"]["blocks_reused"], 0)
        self.assertEqual(
            [event["code"] for event in report["resume"]["events"]],
            ["E_RESUME_MISMATCH"],
        )
        event = report["resume"]["events"][0]
        self.assertEqual(event["block"], 2)
        self.assertIn("manifest_digest", event["difference_fields"])
        self.assertLessEqual(
            abs(report["loudness"]["deviation_lu"]), 0.5
        )

    def test_08_resume_difference_is_reported_with_nonzero_exit(self) -> None:
        output = self.temp / "different.wav"
        report_path = self.temp / "different.report.json"
        state_dir = self.temp / ".state_difference"
        killed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--fresh",
            "--quiet",
            env={"WUCHANG_TEST_KILL_AFTER_BLOCKS": "80"},
        )
        self.assertEqual(killed.returncode, 137)
        snapshot = json.loads(
            (state_dir / "state.json").read_text(encoding="utf-8")
        )
        entry = snapshot["blocks"][60]
        block_path = state_dir / entry["file"]
        payload = bytearray(block_path.read_bytes())
        payload[5000 * 4 + 2] ^= 0x40
        block_path.write_bytes(payload)
        entry["sha256"] = hashlib.sha256(payload).hexdigest()
        (state_dir / "state.json").write_text(
            json.dumps(snapshot, ensure_ascii=False, separators=(",", ":"))
            + "\n",
            encoding="utf-8",
        )

        resumed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out",
            str(output),
            "--report",
            str(report_path),
            "--state-dir",
            str(state_dir),
            "--quiet",
        )
        self.assertNotEqual(resumed.returncode, 0)
        self.assertFalse(output.exists())
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report["status"], "resume_difference")
        error = report["hard_errors"][0]
        self.assertEqual(error["code"], "E_RESUME_DIFFERENCE")
        self.assertEqual(error["block"], 60)
        self.assertIn("expected_block_sha256", error["difference_fields"])
        self.assertTrue(
            report_path.with_name(
                report_path.stem + ".diff.json"
            ).exists()
        )

    def test_09_detection_failure_uses_declared_fallback(self) -> None:
        document, load_errors = load_manifest(self.manifest_path)
        self.assertEqual(load_errors, [])
        assert document is not None
        loaded, audio_errors = validate_audio_and_load(
            document, self.manifest_path
        )
        self.assertEqual(audio_errors, [])
        target = next(
            channel for channel in loaded if channel["name"] == "daluo"
        )
        first_cue = float(target["manifest"]["cues"][0])
        center = round(first_cue * 48000)
        target["data"] = target["data"].copy()
        target["data"][center - 20000 : center + 20000] = 0.0
        processed, warnings = process_channels(loaded, document)
        target_report = next(
            channel
            for channel in processed
            if channel["name"] == "daluo"
        )
        self.assertEqual(target_report["alignment_status"], "fallback")
        self.assertEqual(
            target_report["alignment_offset_samples"],
            target_report["fallback_alignment_samples"],
        )
        self.assertIn(
            ("DETECTION_FAILED", "daluo"),
            [(item["code"], item["channel"]) for item in warnings],
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)

