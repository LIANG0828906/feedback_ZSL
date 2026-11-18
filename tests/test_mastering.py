from __future__ import annotations

import hashlib
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import wave
from pathlib import Path

import numpy as np

from mastering.dsp import compute_ducking, fit_to_length, lookahead_limit, true_peak_dbtp
from mastering.loudness import integrated_loudness
from mastering.synth import create_demo_workspace


ROOT = Path(__file__).resolve().parents[1]


def run_cli(*args: str, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    merged = os.environ.copy()
    merged.pop("MASTERING_SIMULATE_SIGINT_AFTER_BLOCKS", None)
    merged.pop("MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS", None)
    if env:
        merged.update(env)
    return subprocess.run(
        [sys.executable, "-m", "mastering", *args],
        cwd=ROOT,
        env=merged,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class CoreDSPTests(unittest.TestCase):
    def test_integrated_loudness_matches_reference_sine(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate * 3, dtype=np.float64) / sample_rate
        signal = np.repeat((0.1 * np.sin(2.0 * np.pi * 1000.0 * time))[:, None], 2, axis=1)
        measured = integrated_loudness(signal, sample_rate)
        self.assertIsNotNone(measured)
        self.assertAlmostEqual(float(measured), -20.252837, delta=0.01)

    def test_true_peak_oversampling_exceeds_sample_peak(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate, dtype=np.float64) / sample_rate
        signal = (0.9 * np.sin(2.0 * np.pi * 997.0 * time))[:, None]
        sample_peak = 20.0 * np.log10(float(np.max(np.abs(signal))))
        measured = true_peak_dbtp(signal, 4)
        self.assertGreater(measured, sample_peak)
        self.assertLess(measured, sample_peak + 0.2)

    def test_lookahead_limiter_is_smooth_and_bounded(self) -> None:
        sample_rate = 48000
        signal = np.zeros((sample_rate, 1), dtype=np.float64)
        signal[1000::4800] = 0.98
        result = lookahead_limit(signal, sample_rate, -1.0, 5.0, 80.0, 4, 0.05)
        self.assertLessEqual(result.measured_true_peak_dbtp, -1.0)
        self.assertGreater(result.triggered_samples, 0)
        self.assertEqual(result.samples.shape, signal.shape)
        self.assertLess(float(np.max(np.abs(np.diff(result.gain)))), 0.1)

    def test_ducking_curve_has_no_step_and_reaches_depth(self) -> None:
        sample_rate = 48000
        voice = np.zeros((sample_rate * 2, 1), dtype=np.float64)
        time = np.arange(12000, dtype=np.float64) / sample_rate
        voice[12000:24000, 0] = 0.2 * np.sin(2.0 * np.pi * 180.0 * time)
        result = compute_ducking(voice, sample_rate, 12.0, -34.0, 18.0, 260.0, 10.0)
        self.assertAlmostEqual(result.max_attenuation_db, 12.0, delta=1e-3)
        self.assertGreater(result.ducked_samples, 0)
        self.assertLess(result.ducked_samples, voice.shape[0])
        self.assertLess(float(np.max(np.abs(np.diff(result.attenuation_db)))), 0.03)
        self.assertGreater(float(result.attenuation_db[15000]), 10.0)
        self.assertLess(float(result.gain[15000]), 0.35)
        self.assertGreater(float(result.attenuation_db[-1]), 0.0)
        self.assertLess(float(result.attenuation_db[-1]), 1.0)

    def test_fit_only_trims_trailing_silence(self) -> None:
        sample_rate = 48000
        track = np.zeros((sample_rate * 2, 1), dtype=np.float64)
        track[:4800] = 0.1
        fitted, error = fit_to_length(track, sample_rate, sample_rate, -60.0)
        self.assertIsNone(error)
        self.assertIsNotNone(fitted)
        self.assertEqual(fitted.trimmed_samples, sample_rate)
        audible = track.copy()
        audible[-1, 0] = 0.5
        refused, error = fit_to_length(audible, sample_rate, sample_rate, -60.0)
        self.assertIsNone(refused)
        self.assertIn("audible", error or "")


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.root = Path(tempfile.mkdtemp(prefix="uid211-e2e-"))
        cls.baseline = cls.root / "baseline"
        cls.project, cls.rules = create_demo_workspace(cls.baseline, compact=True)
        result = run_cli("run", "--project", str(cls.project))
        if result.returncode != 0:
            raise RuntimeError(result.stdout + result.stderr)
        cls.report_bytes = (cls.baseline / "output" / "mix_report.json").read_bytes()
        cls.master_bytes = {
            language: (cls.baseline / "output" / f"master_{language}.wav").read_bytes()
            for language in ("en", "ja", "de")
        }

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.root, ignore_errors=True)

    def _new_workspace(self, name: str) -> Path:
        path = self.root / name
        create_demo_workspace(path, compact=True)
        return path

    def test_delivery_metrics_headers_report_and_verifier(self) -> None:
        output = self.baseline / "output"
        report = json.loads(self.report_bytes)
        self.assertEqual(report["sample_rate"], 48000)
        self.assertEqual(report["channels"], 2)
        self.assertEqual(report["bit_depth"], 24)
        self.assertEqual(report["total_frames"], 115200)
        self.assertEqual([entry["language"] for entry in report["languages"]], ["en", "ja", "de"])
        self.assertNotIn(str(self.baseline), self.report_bytes.decode("utf-8"))
        self.assertNotIn("timestamp", self.report_bytes.decode("utf-8").lower())
        self.assertEqual(
            list(report.keys())[:6],
            ["schema", "project_digest", "sample_rate", "channels", "bit_depth", "total_frames"],
        )

        by_language = {entry["language"]: entry for entry in report["languages"]}
        self.assertGreater(by_language["en"]["voice"]["padded_samples"], 0)
        self.assertGreater(by_language["ja"]["voice"]["trimmed_samples"], 0)
        self.assertEqual(by_language["ja"]["voice"]["padded_samples"], 0)
        self.assertGreater(by_language["de"]["voice"]["padded_samples"], 0)
        self.assertTrue(any(entry["master"]["limiter_triggered_samples"] > 0 for entry in report["languages"]))

        for entry in report["languages"]:
            language = entry["language"]
            self.assertEqual(entry["voice"]["adapted_frames"], report["total_frames"])
            self.assertLessEqual(abs(entry["master"]["loudness_lufs"] - entry["master"]["target_lufs"]), 0.5)
            self.assertLessEqual(
                entry["master"]["true_peak_dbtp"],
                entry["master"]["true_peak_limit_dbtp"] + 1e-6,
            )
            self.assertGreater(entry["ducking"]["music_bus_ducked_seconds"], 0.0)
            self.assertLessEqual(
                entry["ducking"]["max_attenuation_db"],
                entry["ducking"]["depth_db"] + 1e-6,
            )
            self.assertEqual(entry["master"]["total_frames"], report["total_frames"])
            path = output / entry["file"]["path"]
            self.assertEqual(sha256(path), entry["file"]["sha256"])
            with wave.open(str(path), "rb") as handle:
                self.assertEqual(handle.getframerate(), 48000)
                self.assertEqual(handle.getnchannels(), 2)
                self.assertEqual(handle.getsampwidth(), 3)
                self.assertEqual(handle.getnframes(), report["total_frames"])
                self.assertEqual(
                    len(handle.readframes(report["total_frames"])),
                    report["total_frames"] * 6,
                )

        verify = run_cli("verify", "--report", str(output / "mix_report.json"))
        self.assertEqual(verify.returncode, 0, verify.stdout + verify.stderr)
        self.assertEqual(json.loads(verify.stdout)["status"], "ok")

    def test_repeat_run_is_byte_identical(self) -> None:
        rerun = run_cli("run", "--project", str(self.project))
        self.assertEqual(rerun.returncode, 0, rerun.stdout + rerun.stderr)
        for language, expected in self.master_bytes.items():
            self.assertEqual(
                (self.baseline / "output" / f"master_{language}.wav").read_bytes(),
                expected,
            )
        self.assertEqual(
            (self.baseline / "output" / "mix_report.json").read_bytes(),
            self.report_bytes,
        )

    def test_forced_termination_resume_is_byte_identical(self) -> None:
        workspace = self._new_workspace("sigkill")
        project = workspace / "project.json"
        killed = run_cli(
            "run",
            "--project",
            str(project),
            env={"MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS": "2"},
        )
        self.assertNotEqual(killed.returncode, 0)
        self.assertFalse((workspace / "output" / "mix_report.json").exists())

        resumed = run_cli("run", "--project", str(project))
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        for language, expected in self.master_bytes.items():
            self.assertEqual((workspace / "output" / f"master_{language}.wav").read_bytes(), expected)
        self.assertEqual((workspace / "output" / "mix_report.json").read_bytes(), self.report_bytes)
        recovery = json.loads((workspace / "output" / "recovery_report.json").read_text())
        self.assertEqual(recovery["status"], "completed")
        self.assertGreaterEqual(recovery["recovery_count"], 1)
        self.assertEqual(recovery["completed_blocks"], recovery["total_blocks"])
        self.assertEqual(recovery["discarded_snapshot_versions"], [])

    def test_graceful_interrupt_commits_snapshot_and_exits_zero(self) -> None:
        workspace = self._new_workspace("sigint")
        project = workspace / "project.json"
        interrupted = run_cli(
            "run",
            "--project",
            str(project),
            env={"MASTERING_SIMULATE_SIGINT_AFTER_BLOCKS": "2"},
        )
        self.assertEqual(interrupted.returncode, 0, interrupted.stdout + interrupted.stderr)
        self.assertEqual(json.loads(interrupted.stdout)["status"], "interrupted")
        recovery = json.loads((workspace / "output" / "recovery_report.json").read_text())
        self.assertEqual(recovery["status"], "interrupted")
        self.assertEqual(recovery["completed_blocks"], 2)
        self.assertFalse((workspace / "output" / "mix_report.json").exists())

        resumed = run_cli("run", "--project", str(project))
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        for language, expected in self.master_bytes.items():
            self.assertEqual((workspace / "output" / f"master_{language}.wav").read_bytes(), expected)
        self.assertEqual((workspace / "output" / "mix_report.json").read_bytes(), self.report_bytes)
        final_recovery = json.loads((workspace / "output" / "recovery_report.json").read_text())
        self.assertGreaterEqual(final_recovery["recovery_count"], 1)
        self.assertEqual(final_recovery["status"], "completed")

    def test_corrupt_snapshot_is_discarded_from_zero(self) -> None:
        workspace = self._new_workspace("corrupt")
        project = workspace / "project.json"
        killed = run_cli(
            "run",
            "--project",
            str(project),
            env={"MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS": "2"},
        )
        self.assertNotEqual(killed.returncode, 0)
        snapshots = sorted((workspace / "output" / ".mastering-state").glob("snapshot_v*.json"))
        self.assertTrue(snapshots)
        version = int(snapshots[-1].stem.split("_v")[1])
        snapshots[-1].write_text("{broken", encoding="utf-8")

        resumed = run_cli("run", "--project", str(project))
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        for language, expected in self.master_bytes.items():
            self.assertEqual((workspace / "output" / f"master_{language}.wav").read_bytes(), expected)
        recovery = json.loads((workspace / "output" / "recovery_report.json").read_text())
        self.assertIn(version, recovery["discarded_snapshot_versions"])
        self.assertEqual(recovery["recovery_count"], 0)
        self.assertEqual(recovery["completed_blocks"], recovery["total_blocks"])

    def test_project_digest_mismatch_discards_state(self) -> None:
        workspace = self._new_workspace("digest")
        project = workspace / "project.json"
        killed = run_cli(
            "run",
            "--project",
            str(project),
            env={"MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS": "2"},
        )
        self.assertNotEqual(killed.returncode, 0)
        snapshots = sorted((workspace / "output" / ".mastering-state").glob("snapshot_v*.json"))
        version = int(snapshots[-1].stem.split("_v")[1])
        rules_path = workspace / "rules.json"
        rules = json.loads(rules_path.read_text())
        rules["ducking"]["depth_db"] = 13.0
        rules_path.write_text(json.dumps(rules, indent=2) + "\n", encoding="utf-8")

        rerun = run_cli("run", "--project", str(project))
        self.assertEqual(rerun.returncode, 0, rerun.stdout + rerun.stderr)
        recovery = json.loads((workspace / "output" / "recovery_report.json").read_text())
        self.assertIn(version, recovery["discarded_snapshot_versions"])
        self.assertEqual(recovery["recovery_count"], 0)
        report = json.loads((workspace / "output" / "mix_report.json").read_text())
        self.assertAlmostEqual(report["languages"][0]["ducking"]["depth_db"], 13.0)

    def test_silent_voice_is_warned_and_not_amplified(self) -> None:
        workspace = self._new_workspace("silent")
        project = workspace / "project.json"
        voice_path = workspace / "input" / "voice_en.wav"
        with wave.open(str(voice_path), "rb") as handle:
            params = handle.getparams()
            frames = handle.getnframes()
        with wave.open(str(voice_path), "wb") as handle:
            handle.setparams(params)
            handle.writeframes(b"\x00" * frames * params.nchannels * params.sampwidth)
        result = run_cli("run", "--project", str(project))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        report = json.loads((workspace / "output" / "mix_report.json").read_text())
        entry = next(item for item in report["languages"] if item["language"] == "en")
        self.assertTrue(entry["voice"]["silent"])
        self.assertEqual(entry["voice"]["gain_db"], 0.0)
        self.assertEqual(entry["voice"]["warnings"][0]["code"], "SILENT_TRACK")
        self.assertLessEqual(
            entry["master"]["true_peak_dbtp"],
            entry["master"]["true_peak_limit_dbtp"] + 1e-6,
        )

    def _assert_rejected(self, workspace: Path, expected_code: str, expected_track: str) -> None:
        result = run_cli("run", "--project", str(workspace / "project.json"))
        self.assertNotEqual(result.returncode, 0)
        payload = json.loads(result.stdout)
        self.assertEqual(payload["status"], "error")
        matches = [
            item
            for item in payload["errors"]
            if item["code"] == expected_code and item["track"] == expected_track
        ]
        self.assertTrue(matches, payload)
        self.assertEqual(list((workspace / "output").glob("master_*.wav")), [])

    def test_invalid_inputs_are_rejected_with_business_codes(self) -> None:
        missing = self._new_workspace("missing")
        (missing / "input" / "voice_en.wav").unlink()
        self._assert_rejected(missing, "TRACK_MISSING", "voice_en")

        rate = self._new_workspace("rate")
        voice = rate / "input" / "voice_en.wav"
        raw = bytearray(voice.read_bytes())
        struct.pack_into("<I", raw, 24, 44100)
        voice.write_bytes(raw)
        self._assert_rejected(rate, "SAMPLE_RATE_MISMATCH", "voice_en")

        duplicate = self._new_workspace("duplicate")
        project_path = duplicate / "project.json"
        project = json.loads(project_path.read_text())
        project["tracks"]["voice_de"]["language"] = "en"
        project_path.write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")
        self._assert_rejected(duplicate, "DUPLICATE_LANGUAGE", "voice_de")

        missing_language = self._new_workspace("missing-language")
        project_path = missing_language / "project.json"
        project = json.loads(project_path.read_text())
        del project["tracks"]["voice_de"]
        project_path.write_text(json.dumps(project, indent=2) + "\n", encoding="utf-8")
        self._assert_rejected(missing_language, "MISSING_LANGUAGE", "de")

        depth = self._new_workspace("depth")
        voice = depth / "input" / "voice_en.wav"
        raw = bytearray(voice.read_bytes())
        struct.pack_into("<I", raw, 28, 48000 * 2 * 2)
        struct.pack_into("<H", raw, 32, 4)
        struct.pack_into("<H", raw, 34, 16)
        voice.write_bytes(raw)
        self._assert_rejected(depth, "BIT_DEPTH_MISMATCH", "voice_en")

        channels = self._new_workspace("channels")
        voice = channels / "input" / "voice_en.wav"
        raw = bytearray(voice.read_bytes())
        struct.pack_into("<I", raw, 28, 48000 * 3)
        struct.pack_into("<H", raw, 22, 1)
        struct.pack_into("<H", raw, 32, 3)
        voice.write_bytes(raw)
        self._assert_rejected(channels, "CHANNEL_COUNT_MISMATCH", "voice_en")


if __name__ == "__main__":
    unittest.main()

