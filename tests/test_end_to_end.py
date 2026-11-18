from __future__ import annotations

import contextlib
import io
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from mastering.cli import main
from mastering.engine import master_project
from mastering.errors import ProjectRejected
from mastering.fixture import generate_fixture
from mastering.limiter import true_peak_dbtp
from mastering.loudness import integrated_loudness
from mastering.project import load_project
from mastering.resample import apply_integer_shift, render_track
from mastering.wav import read_wav, read_wav_header, write_wav_atomic


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.fixture = generate_fixture(
            cls.root / "fixture",
            duration_seconds=6.0,
            target_lufs=-14.0,
        )
        cls.master_path = cls.root / "master.wav"
        cls.report_path = cls.root / "report.json"
        cls.report = master_project(
            cls.fixture["manifest"],
            cls.fixture["automation"],
            cls.fixture["config"],
            cls.master_path,
            cls.report_path,
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_master_header_report_and_acceptance_values(self) -> None:
        header = read_wav_header(self.master_path)
        self.assertEqual(header.frame_count, 6 * 48000)
        self.assertEqual(header.sample_rate, 48000)
        self.assertEqual(header.channels, 2)
        self.assertEqual(header.bits_per_sample, 24)
        self.assertEqual(
            header.data_bytes,
            header.frame_count * header.channels * 3,
        )
        self.assertEqual(self.report["project"]["output_frames"], header.frame_count)
        self.assertEqual(self.report["wav"]["data_bytes"], header.data_bytes)
        self.assertEqual(self.report["wav"]["frame_count"], header.frame_count)
        self.assertEqual(
            self.report["wav"]["sha256"], self.report["verification"]["artifact_sha256"]
        )
        self.assertTrue(self.report["verification"]["wav_header_matches_report"])
        self.assertTrue(self.report["verification"]["output_frame_count_preserved"])
        self.assertLessEqual(self.report["limiter"]["output_true_peak_dbtp"], -1.0)
        calibration = self.report["loudness"]["calibration"]
        self.assertLessEqual(abs(calibration["deviation_lu"]), 0.5)
        final_loudness = self.report["loudness"]["final_master"]["integrated_lufs"]
        self.assertLessEqual(abs(final_loudness - (-14.0)), 0.5)
        self.assertGreater(self.report["limiter"]["triggered_frames"], 0)
        decoded = read_wav(self.master_path).samples
        decoded_dbtp, _ = true_peak_dbtp(decoded, 4)
        self.assertLessEqual(decoded_dbtp, -1.0)
        decoded_loudness = integrated_loudness(decoded, 48000).integrated_lufs
        self.assertAlmostEqual(decoded_loudness, final_loudness, places=5)

    def test_all_sync_pulses_align_and_drift_anchor_mapping_is_bounded(self) -> None:
        self.assertEqual(len(self.report["tracks"]), 11)
        for track in self.report["tracks"]:
            self.assertTrue(track["sync"]["detected"], track["id"])
            self.assertTrue(track["sync"]["aligned"], track["id"])
            self.assertLessEqual(
                track["sync"]["alignment_residual_samples"], 1, track["id"]
            )
            self.assertTrue(
                track["drift"]["verification"]["within_two_output_samples"],
                track["id"],
            )
        drifting = [track for track in self.report["tracks"] if track["drift"]["enabled"]]
        self.assertEqual({track["id"] for track in drifting}, {
            "kick",
            "snare",
            "overheads",
            "bass",
            "room",
        })

    def test_automation_reports_smoothed_segments(self) -> None:
        tracks = {track["id"]: track for track in self.report["tracks"]}
        self.assertEqual(
            tracks["lead_vocal"]["automation"]["sharp_segments_smoothed"], 2
        )
        self.assertAlmostEqual(
            tracks["lead_vocal"]["automation"]["total_smoothing_seconds"], 0.48
        )
        self.assertEqual(tracks["kick"]["automation"]["sharp_segments_smoothed"], 1)
        self.assertEqual(
            tracks["lead_vocal"]["automation"]["smoothing_region_count"], 2
        )

    def test_same_input_and_format_is_byte_identical_and_report_is_deterministic(self) -> None:
        second_master = self.root / "master_second.wav"
        second_report = self.root / "report_second.json"
        master_project(
            self.fixture["manifest"],
            self.fixture["automation"],
            self.fixture["config"],
            second_master,
            second_report,
        )
        self.assertEqual(self.master_path.read_bytes(), second_master.read_bytes())
        first = json.loads(self.report_path.read_text(encoding="utf-8"))
        second = json.loads(second_report.read_text(encoding="utf-8"))
        first["wav"].pop("path")
        second["wav"].pop("path")
        self.assertEqual(first, second)

    def test_sync_detection_failure_warns_and_continues(self) -> None:
        copy_root = self.root / "detection_failure"
        if copy_root.exists():
            shutil.rmtree(copy_root)
        shutil.copytree(self.fixture["directory"], copy_root)
        manifest = json.loads((copy_root / "manifest.json").read_text(encoding="utf-8"))
        keys = next(track for track in manifest["tracks"] if track["id"] == "keys")
        keys_path = copy_root / keys["file"]
        data = read_wav(keys_path)
        muted = data.samples.copy()
        muted[: int(round(0.045 * data.sample_rate))] = 0.0
        write_wav_atomic(
            keys_path,
            muted,
            data.sample_rate,
            "pcm",
            data.header.bits_per_sample,
        )
        output = copy_root / "master.wav"
        report = master_project(
            copy_root / "manifest.json",
            copy_root / "automation.json",
            copy_root / "output_config.json",
            output,
            copy_root / "report.json",
        )
        self.assertTrue(output.is_file())
        self.assertEqual(len(report["tracks"]), 11)
        keys_report = next(track for track in report["tracks"] if track["id"] == "keys")
        self.assertFalse(keys_report["sync"]["detected"])
        self.assertFalse(keys_report["sync"]["aligned"])
        self.assertEqual(keys_report["sync"]["applied_offset_samples"], 0)
        self.assertTrue(
            any(
                warning["code"] == "DETECTION_FAILED"
                and warning.get("track") == "keys"
                for warning in report["warnings"]
            )
        )

    def test_header_mismatch_rejects_whole_project_without_master(self) -> None:
        root = self.root / "bad_header"
        if root.exists():
            shutil.rmtree(root)
        shutil.copytree(self.fixture["directory"], root)
        manifest_path = root / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["tracks"][0]["sample_rate"] = 44100
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        output = root / "must_not_exist.wav"
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            exit_code = main(
                [
                    "--manifest",
                    str(manifest_path),
                    "--automation",
                    str(root / "automation.json"),
                    "--config",
                    str(root / "output_config.json"),
                    "--output",
                    str(output),
                    "--report",
                    str(root / "must_not_exist_report.json"),
                ]
            )
        self.assertEqual(exit_code, 2)
        self.assertFalse(output.exists())
        payload = json.loads(stdout.getvalue())
        self.assertFalse(payload["success"])
        issue = payload["error"]["issues"][0]
        self.assertEqual(issue["code"], "SAMPLE_RATE_MISMATCH")
        self.assertEqual(issue["track"], "kick")

    def test_invalid_automation_rejects_before_processing(self) -> None:
        root = self.root / "bad_automation"
        if root.exists():
            shutil.rmtree(root)
        shutil.copytree(self.fixture["directory"], root)
        automation = json.loads((root / "automation.json").read_text(encoding="utf-8"))
        automation["tracks"]["lead_vocal"]["nodes"][2][1] = 99.0
        (root / "automation.json").write_text(json.dumps(automation), encoding="utf-8")
        output = root / "must_not_exist.wav"
        with self.assertRaises(ProjectRejected) as raised:
            master_project(
                root / "manifest.json",
                root / "automation.json",
                root / "output_config.json",
                output,
                root / "must_not_exist_report.json",
            )
        self.assertFalse(output.exists())
        self.assertTrue(
            any(
                issue.code == "INVALID_AUTOMATION_CURVE"
                and issue.track == "lead_vocal"
                for issue in raised.exception.issues
            )
        )

    def test_loaded_project_uses_declared_duration_for_exact_output_count(self) -> None:
        project = load_project(
            self.fixture["manifest"],
            self.fixture["automation"],
            self.fixture["config"],
        )
        self.assertEqual(project.output_frames, 6 * 48000)
        self.assertEqual(project.config.bits_per_sample, 24)
        self.assertEqual(project.config.true_peak_dbtp, -1.0)

    def test_drift_anchor_marker_content_after_sync_shift(self) -> None:
        project = load_project(
            self.fixture["manifest"],
            self.fixture["automation"],
            self.fixture["config"],
        )
        shifts = {
            track["id"]: track["sync"]["applied_offset_samples"]
            for track in self.report["tracks"]
        }
        for track in project.tracks:
            if not track.drift_map.enabled:
                continue
            source = read_wav(track.resolved_path)
            rendered = render_track(
                source.samples,
                source.sample_rate,
                project.config.sample_rate,
                project.output_frames,
                track.drift_map,
                project.config.channels,
                pan=track.pan,
            )
            rendered = apply_integer_shift(rendered, shifts[track.track_id])
            for anchor in track.drift_map.anchors[1:-1]:
                target = int(round(anchor.target_seconds * project.config.sample_rate))
                radius = 5
                window = rendered[target - radius : target + radius + 1, 0]
                detected = target - radius + int(np.argmax(np.abs(window)))
                self.assertGreater(float(np.max(np.abs(window))), 0.2)
                self.assertLessEqual(abs(detected - target), 2, track.track_id)

    def test_44k96k_and_16bit_float32_outputs(self) -> None:
        cases = (
            ("pcm_s16", 44100, 16, 1),
            ("float32", 96000, 32, 3),
        )
        for output_format, sample_rate, bits, format_code in cases:
            with self.subTest(output_format=output_format, sample_rate=sample_rate):
                root = self.root / f"format_{output_format}_{sample_rate}"
                if root.exists():
                    shutil.rmtree(root)
                shutil.copytree(self.fixture["directory"], root)
                config = json.loads(
                    (root / "output_config.json").read_text(encoding="utf-8")
                )
                config["output"]["sample_rate"] = sample_rate
                config["output"]["format"] = output_format
                (root / "output_config.json").write_text(
                    json.dumps(config), encoding="utf-8"
                )
                master = root / "master_format.wav"
                report = master_project(
                    root / "manifest.json",
                    root / "automation.json",
                    root / "output_config.json",
                    master,
                    root / "report_format.json",
                )
                header = read_wav_header(master)
                self.assertEqual(header.sample_rate, sample_rate)
                self.assertEqual(header.bits_per_sample, bits)
                self.assertEqual(header.format_code, format_code)
                self.assertEqual(header.frame_count, round(6.0 * sample_rate))
                self.assertLessEqual(report["limiter"]["output_true_peak_dbtp"], -1.0)
                self.assertEqual(header.frame_count, report["project"]["output_frames"])


if __name__ == "__main__":
    unittest.main()

