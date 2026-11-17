from __future__ import annotations

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
import wave
from pathlib import Path

import numpy as np

from bangzi_align.analysis import (
    analyze_alignment,
    separation_gain_arrays,
)
from bangzi_align.constants import E_RESUME_MISMATCH
from bangzi_align.dsp import finalize_master, integrated_loudness, true_peak_dbtp
from bangzi_align.manifest import load_manifest, load_sources
from bangzi_align.synth import generate_demo


PROJECT_ROOT = Path(__file__).resolve().parents[1]


def run_cli(*args: str, timeout: int = 90) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "bangzi_align", *args],
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory(prefix="bangzi-e2e-")
        cls.root = Path(cls._temporary.name)
        cls.dataset = cls.root / "dataset"
        cls.manifest_path = generate_demo(cls.dataset, seed=237)
        cls.baseline = cls.root / "baseline"
        result = run_cli(
            "render",
            "--manifest",
            str(cls.manifest_path),
            "--out-dir",
            str(cls.baseline),
            "--block-samples",
            "4096",
        )
        if result.returncode != 0:
            raise RuntimeError(result.stderr)
        cls.report = json.loads((cls.baseline / "master_report.json").read_text())
        cls.manifest = load_manifest(cls.manifest_path)
        cls.sources = load_sources(cls.manifest)
        cls.plan = analyze_alignment(cls.manifest, cls.sources)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def test_alignment_and_master_length(self) -> None:
        self.assertLessEqual(self.report["alignment"]["max_abs_hit_error_samples"], 1)
        self.assertLessEqual(
            self.report["alignment"]["max_abs_bangqiang_entry_error_samples"], 2
        )
        with wave.open(str(self.baseline / "master.wav"), "rb") as handle:
            frames = handle.getnframes()
            channels = handle.getnchannels()
            bit_depth = handle.getsampwidth() * 8
        self.assertEqual(channels, 2)
        self.assertEqual(bit_depth, 24)
        self.assertEqual(frames, self.report["total_samples"])
        self.assertEqual(frames, max(self.plan.aligned_lengths.values()))
        self.assertEqual(
            self.report["total_samples"], self.report["longest_channel_samples"]
        )

    def test_segmented_alignment_has_no_cross_segment_leak(self) -> None:
        expected_offsets = tuple(
            -(
                self.manifest.bangzi.start_offset_samples
                + int(
                    np.random.default_rng(
                        np.random.SeedSequence(
                            [self.manifest.seed & 0xFFFFFFFF, 101, index]
                        )
                    ).integers(-180, 181)
                )
            )
            for index in range(len(self.manifest.segments))
        )
        self.assertEqual(self.plan.bangzi_offsets_samples, expected_offsets)
        for alignment in self.plan.segments:
            self.assertLessEqual(alignment.max_abs_hit_error_samples, 1)
            for channel in self.manifest.channels:
                expected = (
                    alignment.bangzi_correction_samples
                    + self.manifest.bangzi.start_offset_samples
                    - channel.start_offset_samples
                )
                self.assertEqual(
                    alignment.channel_shifts_samples[channel.id], expected
                )

    def test_bangqiang_separation_window_and_energy(self) -> None:
        for segment, entry in zip(self.plan.segments, self.plan.entries):
            self.assertLessEqual(abs(entry.error_samples), 2)
            separation = entry.separation
            self.assertLess(separation.laosheng_energy_after, separation.laosheng_energy_before)
            self.assertGreater(separation.bangqiang_energy_after, separation.bangqiang_energy_before)
            self.assertGreater(separation.bangqiang_share_after, separation.bangqiang_share_before)
            targets = np.arange(
                segment.segment.start_sample - 1_000,
                segment.segment.end_sample + 1_000,
                dtype=np.int64,
            )
            laosheng_gain, bangqiang_gain = separation_gain_arrays(
                targets,
                *segment.segment.separation_window_samples,
            )
            outside = (
                (targets < segment.segment.separation_window_samples[0])
                | (targets > segment.segment.separation_window_samples[1])
            )
            self.assertTrue(np.all(laosheng_gain[outside] == 1.0))
            self.assertTrue(np.all(bangqiang_gain[outside] == 1.0))

    def test_loudness_and_true_peak_compliance(self) -> None:
        loudness = self.report["loudness"]
        self.assertLessEqual(abs(loudness["measured_lufs"] - loudness["target_lufs"]), 0.5)
        self.assertTrue(loudness["loudness_compliant"])
        self.assertLessEqual(
            loudness["measured_true_peak_dbtp"],
            loudness["true_peak_max_dbtp"] + 1e-6,
        )
        self.assertTrue(loudness["true_peak_compliant"])
        self.assertEqual(loudness["limiter_delay_samples"], 0)
        self.assertTrue(loudness["limiter_length_preserved"])
        for segment in self.report["segments"]:
            self.assertIn("master_lufs", segment)
            self.assertIn("master_true_peak_dbtp", segment)

    def test_limiter_branch(self) -> None:
        sample_rate = 48_000
        time = np.arange(sample_rate * 5, dtype=np.float64) / sample_rate
        signal = np.column_stack(
            [
                0.04 * np.sin(2.0 * np.pi * 220.0 * time),
                0.04 * np.sin(2.0 * np.pi * 221.0 * time),
            ]
        )
        for index in range(0, signal.shape[0], 12_000):
            signal[index, 0] += 0.70
            signal[index, 1] += 0.65
        result = finalize_master(signal, sample_rate, -16.0, -1.0)
        self.assertEqual(result.signal.shape, signal.shape)
        self.assertLessEqual(abs(result.measured_lufs - (-16.0)), 0.5)
        self.assertLessEqual(result.measured_true_peak_dbtp, -1.0 + 1e-6)
        self.assertGreater(result.limiter_triggered_samples, 0)
        self.assertGreater(result.peak_reduction_db, 0.0)

    def test_report_is_stable_and_has_no_absolute_paths_or_timestamps(self) -> None:
        self.assertEqual(
            list(self.report.keys()),
            [
                "schema",
                "status",
                "manifest_sha256",
                "seed",
                "sample_rate",
                "bit_depth",
                "output",
                "total_samples",
                "longest_channel_samples",
                "alignment",
                "crossfade",
                "segments",
                "loudness",
                "render",
            ],
        )
        serialized = json.dumps(self.report, ensure_ascii=False)
        self.assertNotIn(str(self.root), serialized)
        self.assertNotIn(str(PROJECT_ROOT), serialized)
        self.assertNotIn("timestamp", serialized.lower())
        self.assertEqual(
            [item["ban_style"] for item in self.report["segments"]],
            ["慢板", "流水", "垛板"],
        )

    def test_block_size_does_not_change_master_bytes(self) -> None:
        other = self.root / "block-1000"
        result = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(other),
            "--block-samples",
            "1000",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sha256(self.baseline / "master.wav"), sha256(other / "master.wav"))

    @unittest.skipUnless(hasattr(signal, "SIGKILL"), "SIGKILL unavailable")
    def test_sigkill_then_resume_is_byte_identical(self) -> None:
        target = self.root / "kill-resume"
        command = [
            sys.executable,
            "-m",
            "bangzi_align",
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(target),
            "--block-samples",
            "512",
            "--force",
        ]
        process = subprocess.Popen(
            command,
            cwd=PROJECT_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        snapshot = target / "progress.json"
        deadline = time.monotonic() + 30.0
        observed = None
        while time.monotonic() < deadline and process.poll() is None:
            time.sleep(0.005)
            if snapshot.exists():
                try:
                    payload = json.loads(snapshot.read_text())
                    observed = payload
                    if payload["blocks_done"] >= 30:
                        break
                except (OSError, json.JSONDecodeError):
                    pass
        self.assertIsNone(process.poll(), "render completed before it could be killed")
        self.assertIsNotNone(observed)
        os.kill(process.pid, signal.SIGKILL)
        process.communicate(timeout=20)
        committed = json.loads(snapshot.read_text())
        self.assertGreater(committed["blocks_done"], 0)
        self.assertLess(committed["blocks_done"], committed["total_blocks"])
        resumed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(target),
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            (self.baseline / "master.wav").read_bytes(),
            (target / "master.wav").read_bytes(),
        )
        final_report = json.loads((target / "master_report.json").read_text())
        self.assertTrue(final_report["render"]["resumed_from_snapshot"])

    @unittest.skipUnless(hasattr(signal, "SIGINT"), "SIGINT unavailable")
    def test_sigint_commits_current_block_and_exits_zero(self) -> None:
        target = self.root / "interrupt-resume"
        command = [
            sys.executable,
            "-m",
            "bangzi_align",
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(target),
            "--block-samples",
            "512",
            "--force",
        ]
        process = subprocess.Popen(
            command,
            cwd=PROJECT_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        snapshot = target / "progress.json"
        deadline = time.monotonic() + 30.0
        while time.monotonic() < deadline and process.poll() is None:
            time.sleep(0.005)
            if snapshot.exists():
                try:
                    payload = json.loads(snapshot.read_text())
                    if payload["blocks_done"] >= 25:
                        break
                except (OSError, json.JSONDecodeError):
                    pass
        self.assertIsNone(process.poll(), "render completed before SIGINT")
        os.kill(process.pid, signal.SIGINT)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertEqual(json.loads(stdout)["status"], "interrupted")
        self.assertTrue((target / "recovery_report.json").is_file())
        recovery = json.loads((target / "recovery_report.json").read_text())
        committed = json.loads(snapshot.read_text())
        self.assertEqual(recovery["blocks_committed"], committed["blocks_done"])
        resumed = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(target),
            "--block-samples",
            "512",
        )
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        self.assertEqual(
            (self.baseline / "master.wav").read_bytes(),
            (target / "master.wav").read_bytes(),
        )

    def test_resume_manifest_digest_mismatch_discards_progress(self) -> None:
        case = self.root / "digest-mismatch"
        shutil.copytree(self.baseline, case)
        original = self.manifest_path.read_bytes()
        try:
            self.manifest_path.write_bytes(original + b"\n")
            result = run_cli(
                "render",
                "--manifest",
                str(self.manifest_path),
                "--out-dir",
                str(case),
                "--block-samples",
                "4096",
            )
        finally:
            self.manifest_path.write_bytes(original)
        self.assertEqual(result.returncode, 3, result.stderr)
        diagnostic = json.loads(result.stderr)
        self.assertEqual(diagnostic["diagnostics"][0]["code"], E_RESUME_MISMATCH)
        self.assertFalse((case / "progress.json").exists())
        self.assertFalse((case / "master.wav").exists())

    def test_resume_block_offset_mismatch_is_rejected(self) -> None:
        case = self.root / "block-mismatch"
        shutil.copytree(self.baseline, case)
        snapshot_path = case / "progress.json"
        snapshot = json.loads(snapshot_path.read_text())
        snapshot["block_samples"] = 999
        snapshot_path.write_text(json.dumps(snapshot), encoding="utf-8")
        result = run_cli(
            "render",
            "--manifest",
            str(self.manifest_path),
            "--out-dir",
            str(case),
            "--block-samples",
            "4096",
        )
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertFalse(snapshot_path.exists())
        self.assertFalse((case / "master.wav").exists())


if __name__ == "__main__":
    unittest.main()

