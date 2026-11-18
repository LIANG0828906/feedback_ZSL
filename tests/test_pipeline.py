from __future__ import annotations

import copy
import json
import math
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
import soundfile as sf

from choir_master.compensation import apply_tenor_compensation
from choir_master.detection import detect_for_project, false_positive_audit
from choir_master.errors import MasterError
from choir_master.loudness import finalize_loudness, integrated_loudness
from choir_master.manifest import load_and_validate_audio
from choir_master.render import render_project
from choir_master.synth import create_demo_project, load_generated_truth, verify_reference_tracks
from choir_master.util import atomic_write_json, canonical_json_bytes, sha256_bytes


ROOT = Path(__file__).resolve().parents[1]
PYTHON = sys.executable


def _sha(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


class PipelineAcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp_root = Path(tempfile.mkdtemp(prefix="choir-master-tests-"))
        cls.addClassCleanup(shutil.rmtree, cls.temp_root, True)
        cls.demo_dir = cls.temp_root / "demo"
        cls.generation = create_demo_project(cls.demo_dir)
        cls.manifest_path = cls.demo_dir / "project.json"
        cls.loaded = load_and_validate_audio(cls.manifest_path)
        cls.truth = load_generated_truth(cls.manifest_path)
        cls.full_dir = cls.temp_root / "full"
        cls.full_dir.mkdir()
        cls.full_master = cls.full_dir / "master.wav"
        cls.full_report_path = cls.full_dir / "report.json"
        cls.full_snapshot = cls.full_dir / "snapshot.json"
        cls.full_result = render_project(
            manifest_path=cls.manifest_path,
            output_path=cls.full_master,
            snapshot_path=cls.full_snapshot,
            report_path=cls.full_report_path,
            fresh=True,
        )
        cls.full_report = json.loads(cls.full_report_path.read_text(encoding="utf-8"))

    @staticmethod
    def _manifest_with_abs_paths(source_manifest: Path) -> dict:
        raw = json.loads(source_manifest.read_text(encoding="utf-8"))
        for voice in raw["voices"]:
            for track in voice["tracks"]:
                track["path"] = str((source_manifest.parent / track["path"]).resolve())
        return raw

    def _write_manifest(self, name: str, raw: dict) -> Path:
        directory = self.temp_root / "validation"
        directory.mkdir(exist_ok=True)
        path = directory / f"{name}.json"
        atomic_write_json(path, raw)
        return path

    def test_01_generation_is_byte_deterministic_and_references_match(self) -> None:
        second = self.temp_root / "demo-second"
        create_demo_project(second)
        self.assertEqual(
            (self.demo_dir / "project.json").read_bytes(),
            (second / "project.json").read_bytes(),
        )
        for first in sorted((self.demo_dir / "audio").glob("*.wav")):
            other = second / "audio" / first.name
            self.assertEqual(first.read_bytes(), other.read_bytes(), first.name)
        verification = verify_reference_tracks(self.manifest_path)
        self.assertTrue(verification["all_reference_tracks_match"])
        self.assertEqual(set(verification["voices"]), {"soprano", "alto", "tenor", "bass"})

    def test_02_structured_validation_errors_stop_rendering(self) -> None:
        base = self._manifest_with_abs_paths(self.manifest_path)
        audio_dir = self.demo_dir / "audio"
        bad_rate = self.temp_root / "bad-rate.wav"
        bad_depth = self.temp_root / "bad-depth.wav"
        bad_channels = self.temp_root / "bad-channels.wav"
        sf.write(str(bad_rate), np.zeros(128), 44_100, subtype="PCM_24", format="WAV")
        sf.write(str(bad_depth), np.zeros(128), 48_000, subtype="PCM_16", format="WAV")
        sf.write(
            str(bad_channels),
            np.zeros((128, 2)),
            48_000,
            subtype="PCM_24",
            format="WAV",
        )

        cases = [
            ("missing", str(self.temp_root / "does-not-exist.wav"), "E_FILE_MISSING"),
            ("rate", str(bad_rate), "E_SAMPLE_RATE_MISMATCH"),
            ("depth", str(bad_depth), "E_BIT_DEPTH_MISMATCH"),
            ("channels", str(bad_channels), "E_CHANNEL_COUNT_MISMATCH"),
        ]
        for name, replacement, expected_code in cases:
            raw = copy.deepcopy(base)
            raw["voices"][0]["tracks"][0]["path"] = replacement
            path = self._write_manifest(name, raw)
            with self.assertRaises(MasterError) as caught:
                load_and_validate_audio(path)
            error = caught.exception
            self.assertEqual(error.code, "E_VALIDATION_FAILED")
            self.assertTrue(error.issues)
            issue = next(issue for issue in error.issues if issue.code == expected_code)
            self.assertEqual(issue.path, replacement)

        duplicate = copy.deepcopy(base)
        duplicate["voices"].append(copy.deepcopy(duplicate["voices"][0]))
        duplicate_path = self._write_manifest("duplicate", duplicate)
        with self.assertRaises(MasterError) as caught:
            load_and_validate_audio(duplicate_path)
        self.assertEqual(caught.exception.issues[0].code, "E_VOICE_DUPLICATE")

        missing_voice = copy.deepcopy(base)
        missing_voice["voices"] = [
            voice for voice in missing_voice["voices"] if voice["voice"] != "alto"
        ]
        missing_path = self._write_manifest("missing-voice", missing_voice)
        with self.assertRaises(MasterError) as caught:
            load_and_validate_audio(missing_path)
        self.assertIn(
            "E_VOICE_MISSING",
            {issue.code for issue in caught.exception.issues},
        )

    def test_03_detection_accuracy_drop_and_no_false_positives(self) -> None:
        events = detect_for_project(self.loaded)
        truth = [
            (item["start_s"], item["end_s"]) for item in self.truth["intervals"]
        ]
        self.assertEqual(len(events), len(truth))
        boundaries = []
        for event, (start, end) in zip(events, truth):
            boundaries.extend([event.start_s - start, event.end_s - end])
        rmse = math.sqrt(float(np.mean(np.square(boundaries))))
        self.assertLessEqual(rmse, 0.050)
        for event in events:
            self.assertGreater(event.band_energy_drop_db, 8.0)
        audit = false_positive_audit(self.loaded)
        self.assertEqual(sum(len(items) for items in audit.values()), 0, audit)

    def test_04_compensation_and_determinism(self) -> None:
        events = detect_for_project(self.loaded)
        first_tracks, first_report = apply_tenor_compensation(self.loaded, events)
        second_tracks, second_report = apply_tenor_compensation(self.loaded, events)
        self.assertEqual(set(first_tracks), set(self.loaded.voice_track_ids("tenor")))
        for track_id in first_tracks:
            self.assertTrue(np.array_equal(first_tracks[track_id], second_tracks[track_id]))
        self.assertEqual(first_report, second_report)
        self.assertTrue(first_report["outside_detected_intervals_bit_exact"])
        for interval in first_report["intervals"]:
            self.assertGreaterEqual(interval["improvement_db"], 8.0)
            self.assertLessEqual(
                interval["post_band_drop_db"],
                interval["pre_band_drop_db"] - 8.0,
            )
        tenor_ids = set(self.loaded.voice_track_ids("tenor"))
        for voice_name in ("soprano", "alto", "bass"):
            self.assertTrue(tenor_ids.isdisjoint(self.loaded.voice_track_ids(voice_name)))

    def test_05_master_loudness_peak_balance_and_length(self) -> None:
        report = self.full_report
        self.assertEqual(report["status"], "ok")
        total_samples = max(len(item) for item in self.loaded.tracks.values())
        self.assertEqual(report["master"]["channels"], 2)
        self.assertEqual(report["master"]["samples"], total_samples)
        self.assertEqual(report["alignment"]["max_time_shift_samples"], 0)
        self.assertTrue(report["alignment"]["sample_alignment_preserved"])
        self.assertLessEqual(abs(report["loudness"]["delta_lu"]), 0.5)
        self.assertLessEqual(
            report["loudness"]["measured_true_peak_dbtp"],
            report["loudness"]["true_peak_limit_dbtp"] + 0.01,
        )
        self.assertLessEqual(report["final_energy_max_abs_error"], 0.02)
        self.assertLessEqual(report["balance"]["max_abs_error"], 0.02)
        self.assertTrue(report["loudness"]["sample_count_preserved"])
        self.assertEqual(_sha(self.full_master), report["master"]["sha256"])

    def test_06_forced_termination_resumes_to_identical_master(self) -> None:
        directory = self.temp_root / "sigterm"
        directory.mkdir()
        output = directory / "master.wav"
        snapshot = directory / "snapshot.json"
        report = directory / "report.json"
        command = [
            PYTHON,
            "-m",
            "choir_master",
            "render",
            "--manifest",
            str(self.manifest_path),
            "--output",
            str(output),
            "--snapshot",
            str(snapshot),
            "--report",
            str(report),
            "--fresh",
        ]
        process = subprocess.Popen(
            command,
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        observed = 0
        deadline = time.monotonic() + 20
        while process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.02)
            try:
                observed = int(json.loads(snapshot.read_text())["completed_blocks"])
            except (FileNotFoundError, ValueError, KeyError, json.JSONDecodeError):
                continue
            if observed >= 3:
                process.send_signal(signal.SIGTERM)
                break
        stdout, stderr = process.communicate(timeout=20)
        self.assertNotEqual(process.returncode, 0, (stdout, stderr))
        self.assertGreaterEqual(observed, 3)
        self.assertTrue(snapshot.is_file())
        resumed = subprocess.run(
            command[:-1],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(output.read_bytes(), self.full_master.read_bytes())
        resumed_report = json.loads(report.read_text(encoding="utf-8"))
        self.assertTrue(resumed_report["resume"]["resumed_from_snapshot"])

    def test_07_sigint_writes_recovery_report_then_resumes_identically(self) -> None:
        directory = self.temp_root / "sigint"
        directory.mkdir()
        output = directory / "master.wav"
        snapshot = directory / "snapshot.json"
        report = directory / "report.json"
        recovery = directory / "recovery.json"
        command = [
            PYTHON,
            "-m",
            "choir_master",
            "render",
            "--manifest",
            str(self.manifest_path),
            "--output",
            str(output),
            "--snapshot",
            str(snapshot),
            "--report",
            str(report),
            "--recovery-report",
            str(recovery),
            "--fresh",
        ]
        process = subprocess.Popen(
            command,
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        observed = 0
        deadline = time.monotonic() + 20
        while process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.02)
            try:
                observed = int(json.loads(snapshot.read_text())["completed_blocks"])
            except (FileNotFoundError, ValueError, KeyError, json.JSONDecodeError):
                continue
            if observed >= 3:
                process.send_signal(signal.SIGINT)
                break
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0, (stdout, stderr))
        self.assertGreaterEqual(observed, 3)
        self.assertTrue(recovery.is_file(), stderr)
        recovery_payload = json.loads(recovery.read_text(encoding="utf-8"))
        self.assertEqual(recovery_payload["code"], "E_INTERRUPTED_RESUMABLE")
        self.assertGreaterEqual(recovery_payload["completed_blocks"], observed)
        self.assertFalse(output.exists())
        resumed = subprocess.run(
            command[:-1],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(output.read_bytes(), self.full_master.read_bytes())

    def test_08_resume_mismatch_is_discarded_with_business_code(self) -> None:
        directory = self.temp_root / "mismatch"
        directory.mkdir()
        snapshot = directory / "snapshot.json"
        shutil.copyfile(self.full_snapshot, snapshot)
        manifest = self._manifest_with_abs_paths(self.manifest_path)
        manifest["target_loudness_lufs"] = -17.0
        manifest_path = directory / "project.json"
        atomic_write_json(manifest_path, manifest)
        report = directory / "report.json"
        process = subprocess.run(
            [
                PYTHON,
                "-m",
                "choir_master",
                "render",
                "--manifest",
                str(manifest_path),
                "--output",
                str(directory / "master.wav"),
                "--snapshot",
                str(snapshot),
                "--report",
                str(report),
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(process.returncode, 3, process.stderr)
        payload = json.loads(report.read_text(encoding="utf-8"))
        self.assertEqual(payload["code"], "E_RESUME_MISMATCH")
        self.assertFalse(snapshot.exists())

    def test_09_bs1770_calibration_and_true_peak_limiter_length(self) -> None:
        sample_rate = 48_000
        time_axis = np.arange(sample_rate * 3, dtype=np.float64) / sample_rate
        calibration = 0.1 * np.sin(2.0 * np.pi * 997.0 * time_axis)
        stereo_calibration = np.column_stack((calibration, calibration))
        self.assertAlmostEqual(
            integrated_loudness(stereo_calibration, sample_rate),
            -20.0,
            delta=0.1,
        )

        programme = np.column_stack(
            (
                0.2 * np.sin(2.0 * np.pi * 440.0 * time_axis + 0.1),
                0.2 * np.sin(2.0 * np.pi * 660.0 * time_axis + 0.7),
            )
        )
        limited = finalize_loudness(programme, sample_rate, -2.0, -1.0)
        self.assertEqual(limited.audio.shape, programme.shape)
        self.assertGreater(limited.limiter.triggered_samples, 0)
        self.assertLessEqual(limited.true_peak_dbtp, -1.0 + 0.01)
        self.assertLessEqual(abs(limited.delta_lu), 0.5)
        self.assertTrue(limited.limiter.delay_compensated)


if __name__ == "__main__":
    unittest.main()

