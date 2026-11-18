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

from guqin_master.alignment import align_channels
from guqin_master.checkpoint import verify_partial_blocks
from guqin_master.decoupling import decouple_channels
from guqin_master.manifest import load_and_validate_channels, load_manifest
from guqin_master.mastering import build_gain_envelopes
from guqin_master.synth import write_demo_project
from guqin_master.wavio import read_wav_mono, write_wav


def _run_cli(
    arguments: list[str],
    environment: dict[str, str] | None = None,
) -> tuple[subprocess.CompletedProcess[str], dict | None]:
    merged = os.environ.copy()
    if environment:
        merged.update(environment)
    process = subprocess.run(
        [sys.executable, "-m", "guqin_master", *arguments],
        text=True,
        capture_output=True,
        env=merged,
        check=False,
    )
    try:
        payload = json.loads(process.stdout)
    except json.JSONDecodeError:
        payload = None
    return process, payload


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.demo = cls.root / "demo"
        write_demo_project(cls.demo)
        cls.baseline = cls.root / "baseline"
        process, payload = _run_cli(
            [
                "run",
                "--project",
                str(cls.demo / "manifest.json"),
                "--out",
                str(cls.baseline),
                "--quiet-progress",
            ]
        )
        if process.returncode != 0 or payload is None:
            raise RuntimeError(
                f"baseline render failed: {process.returncode}: {process.stdout} {process.stderr}"
            )
        cls.report = json.loads(
            (cls.baseline / "master_report.json").read_text(encoding="utf-8")
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def test_synthesis_is_byte_deterministic(self) -> None:
        other = self.root / "demo_repeat"
        write_demo_project(other)
        self.assertEqual(
            (self.demo / "manifest.json").read_bytes(),
            (other / "manifest.json").read_bytes(),
        )
        for channel in load_manifest(self.demo / "manifest.json").channels:
            self.assertEqual(
                (self.demo / channel.path).read_bytes(),
                (other / channel.path).read_bytes(),
                channel.name,
            )

    def test_decoupling_alignment_and_compliance(self) -> None:
        project = load_manifest(self.demo / "manifest.json")
        raw, _ = load_and_validate_channels(project)
        decoupled = decouple_channels(project, raw)
        for event in decoupled.events:
            self.assertTrue(event.detected, event.target)
            self.assertGreaterEqual(
                event.attenuation_db,
                project.decoupling.minimum_attenuation_db,
                event.target,
            )
            self.assertIsNotNone(event.unrelated_peak_loss_db)
            self.assertLessEqual(
                event.unrelated_peak_loss_db,
                project.decoupling.max_unrelated_peak_loss_db,
            )

        modified = set(decoupled.modified_targets)
        for channel in project.channels:
            if channel.name not in modified:
                np.testing.assert_array_equal(
                    raw[channel.name], decoupled.channels[channel.name]
                )
        for leak in project.crosstalk:
            start = int(round(leak.start_seconds * project.sample_rate))
            stop = int(round(leak.end_seconds * project.sample_rate))
            target_before = raw[leak.target]
            target_after = decoupled.channels[leak.target]
            np.testing.assert_array_equal(target_before[:start], target_after[:start])
            np.testing.assert_array_equal(target_before[stop:], target_after[stop:])
            self.assertGreater(
                float(np.max(np.abs(target_before[start:stop] - target_after[start:stop]))),
                0.0,
            )

        aligned = align_channels(project, decoupled.channels)
        for item in aligned.alignments:
            self.assertLessEqual(max(abs(value) for value in item.residual_samples), 1)
            self.assertEqual(item.applied_shift_samples, -item.estimated_offset_samples)

        self.assertLessEqual(
            abs(self.report["loudness"]["delta_lu"]), 0.5
        )
        self.assertLessEqual(
            self.report["true_peak"]["max_dbtp"],
            self.report["true_peak"]["limit_dbtp"] + 1e-6,
        )
        self.assertEqual(self.report["audio"]["samples"], project.total_samples)
        self.assertEqual(self.report["audio"]["channels"], 2)
        self.assertTrue(self.report["limiter"]["sample_count_preserved"])
        self.assertEqual(self.report["limiter"]["processing_delay_samples"], 0)
        self.assertEqual(len(self.report["segments"]), len(project.segments))
        contributions = self.report["contributions"]["normalized_energy_fraction"]
        self.assertAlmostEqual(sum(contributions.values()), 1.0, places=10)
        self.assertTrue(all(value >= 0 for value in contributions.values()))

        _, windows = build_gain_envelopes(project)
        boundary = project.segments[0].end_seconds * project.sample_rate
        index = int(round(boundary))
        total = sum(window[index] for window in windows.values())
        self.assertAlmostEqual(total, 1.0, places=10)

        master, info = read_wav_mono(self.baseline / "master.wav", "master")
        self.assertEqual(info.channels, 2)
        self.assertEqual(info.bit_depth, project.bit_depth)
        self.assertEqual(info.frames, project.total_samples)
        self.assertTrue(np.all(np.isfinite(master)))

    def test_repeat_render_is_byte_identical(self) -> None:
        repeat = self.root / "repeat_output"
        process, payload = _run_cli(
            [
                "run",
                "--project",
                str(self.demo / "manifest.json"),
                "--out",
                str(repeat),
                "--quiet-progress",
            ]
        )
        self.assertEqual(process.returncode, 0, process.stdout)
        self.assertEqual(payload["status"], "COMPLETE")
        self.assertEqual(
            (self.baseline / "master.wav").read_bytes(),
            (repeat / "master.wav").read_bytes(),
        )
        self.assertEqual(
            (self.baseline / "master_report.json").read_bytes(),
            (repeat / "master_report.json").read_bytes(),
        )

    def test_graceful_interrupt_then_resume_is_byte_identical(self) -> None:
        output = self.root / "resumed_output"
        interrupted, interrupted_payload = _run_cli(
            [
                "run",
                "--project",
                str(self.demo / "manifest.json"),
                "--out",
                str(output),
                "--quiet-progress",
            ],
            {"GUQIN_TEST_INTERRUPT_AFTER_BLOCKS": "11"},
        )
        self.assertEqual(interrupted.returncode, 0, interrupted.stdout)
        self.assertEqual(interrupted_payload["status"], "INTERRUPTED")
        self.assertFalse((output / "master.wav").exists())
        recovery = json.loads(
            (output / "state" / "recovery_report.json").read_text(encoding="utf-8")
        )
        self.assertEqual(recovery["completed_blocks"], 11)
        self.assertEqual(recovery["business_code"], "OK")

        resumed, resumed_payload = _run_cli(
            [
                "run",
                "--project",
                str(self.demo / "manifest.json"),
                "--out",
                str(output),
                "--quiet-progress",
            ]
        )
        self.assertEqual(resumed.returncode, 0, resumed.stdout)
        self.assertEqual(resumed_payload["status"], "COMPLETE")
        self.assertFalse((output / "state" / "recovery_report.json").exists())
        self.assertEqual(
            (self.baseline / "master.wav").read_bytes(),
            (output / "master.wav").read_bytes(),
        )
        compare, compare_payload = _run_cli(
            [
                "compare",
                "--left",
                str(self.baseline / "master.wav"),
                "--right",
                str(output / "master.wav"),
            ]
        )
        self.assertEqual(compare.returncode, 0, compare.stdout)
        self.assertTrue(compare_payload["byte_identical"])

    def test_resume_manifest_mismatch_discards_progress(self) -> None:
        project_directory = self.root / "mismatch_project"
        shutil.copytree(self.demo, project_directory)
        output = self.root / "mismatch_output"
        interrupted, _ = _run_cli(
            [
                "run",
                "--project",
                str(project_directory / "manifest.json"),
                "--out",
                str(output),
                "--quiet-progress",
            ],
            {"GUQIN_TEST_INTERRUPT_AFTER_BLOCKS": "7"},
        )
        self.assertEqual(interrupted.returncode, 0)
        manifest_path = project_directory / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["target_loudness_lufs"] = -16.2
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        process, payload = _run_cli(
            [
                "run",
                "--project",
                str(manifest_path),
                "--out",
                str(output),
                "--quiet-progress",
            ]
        )
        self.assertEqual(process.returncode, 4, process.stdout)
        self.assertEqual(payload["business_code"], "E_RESUME_MISMATCH")
        self.assertTrue(payload["progress_discarded"])
        self.assertFalse((output / "state" / "progress.json").exists())
        self.assertFalse((output / "state" / "master.partial.f64").exists())

    def test_resume_partial_difference_reports_block_interval_and_channel(self) -> None:
        project_directory = self.root / "diff_project"
        shutil.copytree(self.demo, project_directory)
        output = self.root / "diff_output"
        interrupted, _ = _run_cli(
            [
                "run",
                "--project",
                str(project_directory / "manifest.json"),
                "--out",
                str(output),
                "--quiet-progress",
            ],
            {"GUQIN_TEST_INTERRUPT_AFTER_BLOCKS": "6"},
        )
        self.assertEqual(interrupted.returncode, 0)
        partial = output / "state" / "master.partial.f64"
        payload = bytearray(partial.read_bytes())
        payload[37] ^= 0x01
        partial.write_bytes(payload)
        process, error = _run_cli(
            [
                "run",
                "--project",
                str(project_directory / "manifest.json"),
                "--out",
                str(output),
                "--quiet-progress",
            ]
        )
        self.assertEqual(process.returncode, 5, process.stdout)
        self.assertEqual(error["business_code"], "E_RESUME_DIFF")
        self.assertIn(error["channel"], {"L", "R"})
        self.assertEqual(error["block_index"], 0)
        self.assertEqual(error["sample_range"], [0, 4096])

    def test_zero_block_snapshot_discards_uncommitted_partial_tail(self) -> None:
        partial = self.root / "uncommitted_tail.f64"
        partial.write_bytes(bytes(4096 * 16))
        snapshot = {
            "completed_blocks": 0,
            "total_blocks": 118,
        }
        verify_partial_blocks(
            snapshot,
            partial,
            block_size=4096,
            total_samples=480000,
        )
        self.assertEqual(partial.stat().st_size, 0)

    def test_detection_failure_is_structured(self) -> None:
        project_directory = self.root / "detection_project"
        shutil.copytree(self.demo, project_directory)
        project = load_manifest(project_directory / "manifest.json")
        channel = project.channels[3]
        samples, _ = read_wav_mono(project_directory / channel.path, channel.name)
        reference = int(round(project.reference_times[2] * project.sample_rate))
        point = reference + channel.start_offset_samples
        samples[max(0, point - 3000) : point + 3000] = 0.0
        write_wav(
            project_directory / channel.path,
            samples,
            project.sample_rate,
            project.bit_depth,
        )
        process, payload = _run_cli(
            [
                "run",
                "--project",
                str(project_directory / "manifest.json"),
                "--out",
                str(self.root / "detection_output"),
                "--quiet-progress",
            ]
        )
        self.assertEqual(process.returncode, 3, process.stdout)
        self.assertEqual(payload["business_code"], "E_DETECTION_FAILED")
        self.assertEqual(payload["channel"], channel.name)

    def test_invalid_project_cases_are_structured(self) -> None:
        missing = self.root / "invalid_missing"
        shutil.copytree(self.demo, missing)
        first_manifest = json.loads(
            (missing / "manifest.json").read_text(encoding="utf-8")
        )
        (missing / first_manifest["channels"][0]["path"]).unlink()

        wrong_rate = self.root / "invalid_rate"
        shutil.copytree(self.demo, wrong_rate)
        project = load_manifest(wrong_rate / "manifest.json")
        samples, _ = read_wav_mono(
            wrong_rate / project.channels[0].path, project.channels[0].name
        )
        write_wav(
            wrong_rate / project.channels[0].path,
            samples,
            44100,
            project.bit_depth,
        )

        wrong_channels = self.root / "invalid_channels"
        shutil.copytree(self.demo, wrong_channels)
        project = load_manifest(wrong_channels / "manifest.json")
        samples, _ = read_wav_mono(
            wrong_channels / project.channels[0].path, project.channels[0].name
        )
        write_wav(
            wrong_channels / project.channels[0].path,
            np.column_stack((samples, samples)),
            project.sample_rate,
            project.bit_depth,
        )

        duplicate_pitch = self.root / "invalid_duplicate"
        shutil.copytree(self.demo, duplicate_pitch)
        manifest_path = duplicate_pitch / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["channels"][1]["pitch"] = manifest["channels"][0]["pitch"]
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")

        unsorted = self.root / "invalid_unsorted"
        shutil.copytree(self.demo, unsorted)
        manifest_path = unsorted / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["reference_times_seconds"][-1], manifest["reference_times_seconds"][
            -2
        ] = (
            manifest["reference_times_seconds"][-2],
            manifest["reference_times_seconds"][-1],
        )
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")

        cases = [
            ("missing", missing),
            ("sample_rate", wrong_rate),
            ("channels", wrong_channels),
            ("duplicate_pitch", duplicate_pitch),
            ("unsorted", unsorted),
        ]
        for label, directory in cases:
            with self.subTest(label=label):
                process, payload = _run_cli(
                    [
                        "run",
                        "--project",
                        str(directory / "manifest.json"),
                        "--out",
                        str(self.root / f"invalid_out_{label}"),
                        "--quiet-progress",
                    ]
                )
                self.assertEqual(process.returncode, 2, process.stdout)
                self.assertEqual(payload["business_code"], "E_INVALID_PROJECT")
                self.assertIn("channel", payload)
                self.assertFalse(
                    (self.root / f"invalid_out_{label}" / "master.wav").exists()
                )


if __name__ == "__main__":
    unittest.main()
