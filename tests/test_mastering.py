from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from mastering.dsp import integrated_loudness, lookahead_limiter, true_peak_dbtp
from mastering.errors import BusinessFailure, EXIT_RESUME_MISMATCH, EXIT_VALIDATION
from mastering.manifest import load_manifest
from mastering.pipeline import render_project
from mastering.synthesis import synthesize_manifest
from mastering.util import sha256_file
from mastering.wavfile import read_wav, write_wav


def compact_manifest(root: Path) -> Path:
    manifest = {
        "schema_version": 1,
        "seed": 245,
        "sample_rate": 48000,
        "bit_depth": 24,
        "dialogue_points_seconds": [0.25, 0.55, 0.90, 1.25, 1.60, 1.95],
        "mono_fold": {
            "rule": "sum_0.5",
            "band_hz": [300.0, 3400.0],
            "energy_tolerance": 0.015,
        },
        "loudness": {
            "target_lufs": -17.0,
            "true_peak_dbtp": -1.0,
            "tolerance_lu": 0.5,
        },
        "ducking": {
            "depth_db": -12.0,
            "attack_ms": 15.0,
            "recovery_ms": 160.0,
            "sir_min_improvement_db": 6.0,
        },
        "render": {
            "block_frames": 4096,
            "output_dir": "out",
            "snapshot_path": "state/render.snapshot.json",
            "master_stereo": "master_stereo.wav",
            "master_mono": "master_mono.wav",
            "report": "mastering_report.json",
            "recovery_report": "recovery_report.json",
        },
        "tracks": [
            {
                "name": "dialogue_a",
                "kind": "dialogue",
                "path": "media/dialogue_a.wav",
                "sample_rate": 48000,
                "channels": 1,
                "bit_depth": 24,
                "offset_seconds": 0.123456,
                "synth": {
                    "cue_indices": [0, 2, 4],
                    "jitter_samples": 7,
                    "local_duration_seconds": 2.0,
                    "f0_hz": 148.0,
                    "burst_gain": 0.58,
                    "vowel_gain": 0.27,
                    "gain": 1.0,
                    "pan": 0.0,
                },
            },
            {
                "name": "dialogue_b",
                "kind": "dialogue",
                "path": "media/dialogue_b.wav",
                "sample_rate": 48000,
                "channels": 2,
                "bit_depth": 24,
                "offset_seconds": 0.071111,
                "synth": {
                    "cue_indices": [1, 3, 5],
                    "jitter_samples": -5,
                    "local_duration_seconds": 2.3,
                    "f0_hz": 112.0,
                    "burst_gain": 0.52,
                    "vowel_gain": 0.25,
                    "gain": 1.0,
                    "pan": 0.0,
                },
            },
            {
                "name": "narration",
                "kind": "narration",
                "path": "media/narration.wav",
                "sample_rate": 48000,
                "channels": 1,
                "bit_depth": 24,
                "offset_seconds": 0.234567,
                "synth": {
                    "segments": [
                        {"cue_index": 1, "duration_seconds": 0.55},
                        {"cue_index": 4, "duration_seconds": 0.60},
                    ],
                    "jitter_samples": -3,
                    "local_duration_seconds": 2.2,
                    "f0_hz": 176.0,
                    "gain": 0.42,
                    "pan": 0.0,
                },
            },
            {
                "name": "music_bed",
                "kind": "music",
                "path": "media/music_bed.wav",
                "sample_rate": 48000,
                "channels": 2,
                "bit_depth": 24,
                "offset_seconds": 0.0,
                "synth": {
                    "duration_seconds": 2.4,
                    "chord_hz": [130.81, 164.81, 196.0, 246.94],
                    "gain": 0.18,
                    "width": 0.06,
                    "fade_seconds": 0.06,
                },
            },
        ],
    }
    path = root / "manifest.json"
    path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return path


class DspTests(unittest.TestCase):
    def test_bs1770_reference_sine_and_true_peak(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate, dtype=np.float64) / sample_rate
        sine = 0.1 * np.sin(2.0 * np.pi * 1000.0 * time)
        stereo = np.stack((sine, sine), axis=1)
        loudness = integrated_loudness(stereo, sample_rate)
        self.assertGreater(loudness, -20.2)
        self.assertLess(loudness, -19.8)

        intersample = np.sin(2.0 * np.pi * (sample_rate / 4.0) * time + np.pi / 4.0)
        self.assertGreater(true_peak_dbtp(intersample[:, None]), -0.2)
        self.assertLess(true_peak_dbtp(intersample[:, None]), 0.2)

    def test_limiter_has_no_latency_or_length_change(self) -> None:
        sample_rate = 48000
        time = np.arange(sample_rate, dtype=np.float64) / sample_rate
        signal = np.sin(2.0 * np.pi * 440.0 * time) * 1.2
        stereo = np.stack((signal, signal), axis=1)
        limited, triggered, peak = lookahead_limiter(stereo, -1.0, sample_rate)
        self.assertEqual(limited.shape, stereo.shape)
        self.assertGreater(triggered, 0)
        self.assertLessEqual(peak, -1.0 + 1e-6)


class ProjectTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="mastering-test-")
        self.root = Path(self.temp.name)
        self.manifest_path = compact_manifest(self.root)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _synthesize(self) -> None:
        synthesize_manifest(self.manifest_path)

    def _run_cli(self, *args: str, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        process_env = os.environ.copy()
        process_env["PYTHONPATH"] = str(ROOT)
        if env:
            process_env.update(env)
        return subprocess.run(
            [sys.executable, "-m", "mastering.cli", *args],
            cwd=ROOT,
            env=process_env,
            text=True,
            capture_output=True,
            timeout=120,
            check=False,
        )

    def test_synthesis_is_byte_deterministic(self) -> None:
        self._synthesize()
        first = {
            path.name: sha256_file(path)
            for path in sorted((self.root / "media").glob("*.wav"))
        }
        self._synthesize()
        second = {
            path.name: sha256_file(path)
            for path in sorted((self.root / "media").glob("*.wav"))
        }
        self.assertEqual(first, second)
        self.assertEqual(len(first), 4)

    def test_validation_errors_have_codes_tracks_and_no_partial_master(self) -> None:
        self._synthesize()
        dialogue_path = self.root / "media/dialogue_a.wav"
        loaded = read_wav(dialogue_path)
        write_wav(dialogue_path, loaded.samples, loaded.info.sample_rate + 1, loaded.info.bit_depth)

        with self.assertRaises(BusinessFailure) as context:
            render_project(self.manifest_path, resume=False)
        failure = context.exception
        self.assertEqual(failure.exit_code, EXIT_VALIDATION)
        self.assertTrue(
            any(error.code == "E_SAMPLE_RATE_MISMATCH" and error.track == "dialogue_a" for error in failure.errors)
        )
        self.assertFalse((self.root / "out/master_stereo.wav").exists())
        self.assertFalse((self.root / "out/master_mono.wav").exists())

        result = self._run_cli("render", "--manifest", str(self.manifest_path), "--fresh")
        self.assertEqual(result.returncode, EXIT_VALIDATION)
        payload = json.loads(result.stdout)
        self.assertTrue(
            any(
                error["code"] == "E_SAMPLE_RATE_MISMATCH" and error.get("track") == "dialogue_a"
                for error in payload["errors"]
            )
        )

    def test_full_render_contract_and_repeatability(self) -> None:
        self._synthesize()
        first = render_project(self.manifest_path, resume=False)
        self.assertEqual(first.exit_code, 0)
        report_path = self.root / "out/mastering_report.json"
        report_bytes = report_path.read_bytes()
        report = json.loads(report_bytes)

        self.assertLessEqual(report["alignment"]["max_alignment_error_samples"], 1)
        total_frames = report["alignment"]["total_frames"]
        self.assertEqual(total_frames, report["master"]["frames"])
        self.assertEqual(
            total_frames,
            max(track["aligned_duration_samples"] for track in report["alignment"]["tracks"]),
        )
        self.assertTrue(report["fold"]["within_tolerance"])
        self.assertLessEqual(
            report["fold"]["max_ratio_deviation"],
            report["fold"]["energy_tolerance"],
        )
        self.assertGreaterEqual(report["ducking"]["interval_count"], 1)
        for interval in report["ducking"]["intervals"]:
            self.assertGreaterEqual(
                interval["sir_improvement_db"],
                report["ducking"]["sir_min_improvement_db"] - 1e-9,
            )
        self.assertLessEqual(abs(report["loudness"]["deviation_lu"]), 0.5)
        self.assertLessEqual(report["loudness"]["true_peak_dbtp"], -1.0 + 1e-6)
        self.assertLessEqual(report["master"]["mono_true_peak_dbtp"], -1.0 + 1e-6)
        self.assertGreaterEqual(report["loudness"]["limit_triggered_samples"], 0)
        self.assertEqual(report["snapshot"]["commits"], report["snapshot"]["completed_blocks"])
        self.assertNotIn("elapsed", report_bytes.decode("utf-8"))

        stereo = read_wav(self.root / "out/master_stereo.wav")
        mono = read_wav(self.root / "out/master_mono.wav")
        self.assertEqual(stereo.info.frames, total_frames)
        self.assertEqual(mono.info.frames, total_frames)
        self.assertEqual(stereo.info.channels, 2)
        self.assertEqual(mono.info.channels, 1)

        first_stereo = (self.root / "out/master_stereo.wav").read_bytes()
        first_mono = (self.root / "out/master_mono.wav").read_bytes()
        second = render_project(self.manifest_path, resume=False)
        self.assertEqual(second.exit_code, 0)
        self.assertEqual((self.root / "out/master_stereo.wav").read_bytes(), first_stereo)
        self.assertEqual((self.root / "out/master_mono.wav").read_bytes(), first_mono)
        self.assertEqual(report_path.read_bytes(), report_bytes)

    def test_forced_termination_resume_is_byte_identical(self) -> None:
        self._synthesize()
        reference = render_project(self.manifest_path, resume=False)
        self.assertEqual(reference.exit_code, 0)
        expected_stereo = (self.root / "out/master_stereo.wav").read_bytes()
        expected_mono = (self.root / "out/master_mono.wav").read_bytes()

        crashed = self._run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--fresh",
            env={"MASTERING_TEST_CRASH_AFTER_BLOCK": "5"},
        )
        self.assertEqual(crashed.returncode, 137)
        snapshot = json.loads((self.root / "state/render.snapshot.json").read_text(encoding="utf-8"))
        self.assertEqual(snapshot["completed_blocks"], 5)

        resumed = self._run_cli("render", "--manifest", str(self.manifest_path), "--resume")
        self.assertEqual(resumed.returncode, 0, resumed.stderr + resumed.stdout)
        self.assertEqual((self.root / "out/master_stereo.wav").read_bytes(), expected_stereo)
        self.assertEqual((self.root / "out/master_mono.wav").read_bytes(), expected_mono)
        report = json.loads((self.root / "out/mastering_report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["snapshot"]["resumed_from_block"], 5)

    def test_interrupt_finishes_block_writes_recovery_report_and_resumes(self) -> None:
        self._synthesize()
        reference = render_project(self.manifest_path, resume=False)
        self.assertEqual(reference.exit_code, 0)
        expected_stereo = (self.root / "out/master_stereo.wav").read_bytes()
        expected_mono = (self.root / "out/master_mono.wav").read_bytes()
        (self.root / "out/mastering_report.json").unlink()

        interrupted = self._run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--fresh",
            env={"MASTERING_TEST_INTERRUPT_AFTER_BLOCK": "5"},
        )
        self.assertEqual(interrupted.returncode, 0, interrupted.stderr + interrupted.stdout)
        recovery = json.loads((self.root / "out/recovery_report.json").read_text(encoding="utf-8"))
        self.assertEqual(recovery["code"], "E_INTERRUPTED_RESUMABLE")
        self.assertEqual(recovery["completed_blocks"], 5)
        self.assertFalse((self.root / "out/mastering_report.json").exists())

        resumed = self._run_cli("render", "--manifest", str(self.manifest_path), "--resume")
        self.assertEqual(resumed.returncode, 0, resumed.stderr + resumed.stdout)
        self.assertEqual((self.root / "out/master_stereo.wav").read_bytes(), expected_stereo)
        self.assertEqual((self.root / "out/master_mono.wav").read_bytes(), expected_mono)

    def test_resume_mismatch_discards_snapshot_and_exits_with_business_code(self) -> None:
        self._synthesize()
        crashed = self._run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--fresh",
            env={"MASTERING_TEST_CRASH_AFTER_BLOCK": "3"},
        )
        self.assertEqual(crashed.returncode, 137)
        snapshot_path = self.root / "state/render.snapshot.json"
        self.assertTrue(snapshot_path.exists())

        manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        manifest["loudness"]["target_lufs"] = -18.0
        self.manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        resumed = self._run_cli("render", "--manifest", str(self.manifest_path), "--resume")
        self.assertEqual(resumed.returncode, EXIT_RESUME_MISMATCH)
        payload = json.loads(resumed.stdout)
        self.assertTrue(any(error["code"] == "E_RESUME_MISMATCH" for error in payload["errors"]))
        self.assertFalse(snapshot_path.exists())
        self.assertFalse((self.root / "out/master_stereo.wav").exists())

    def test_cue_outside_actual_wav_duration_is_track_specific(self) -> None:
        self._synthesize()
        dialogue_path = self.root / "media/dialogue_b.wav"
        loaded = read_wav(dialogue_path)
        write_wav(
            dialogue_path,
            loaded.samples[: int(1.0 * loaded.info.sample_rate)],
            loaded.info.sample_rate,
            loaded.info.bit_depth,
        )
        with self.assertRaises(BusinessFailure) as context:
            load_manifest(self.manifest_path, validate_audio=True)
        self.assertTrue(
            any(
                error.code == "E_CUE_OUT_OF_RANGE" and error.track == "dialogue_b"
                for error in context.exception.errors
            )
        )


if __name__ == "__main__":
    unittest.main()
