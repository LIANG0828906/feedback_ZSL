from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
import wave
from pathlib import Path

import numpy as np

from drum_align.audio import read_wav
from drum_align.drift import DriftCurve
from drum_align.errors import BusinessError
from drum_align.fixture import generate_demo
from drum_align.pipeline import align_project
from drum_align.resample import sample_at_reference_positions
from drum_align.signal import detect_kicks, nearest_hit


REPO_ROOT = Path(__file__).resolve().parents[1]


class AlignmentAcceptanceTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temporary = tempfile.TemporaryDirectory(prefix="drum-align-test-")
        cls.root = Path(cls._temporary.name)
        cls.fixture = cls.root / "fixture"
        cls.project = generate_demo(cls.fixture, duration_seconds=26.0)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temporary.cleanup()

    def _outputs(self, name: str) -> tuple[Path, Path, Path]:
        directory = self.root / name
        directory.mkdir(parents=True, exist_ok=True)
        return (
            directory / "master.wav",
            directory / "report.json",
            directory / "state.json",
        )

    def test_detection_drift_alignment_and_phase(self) -> None:
        master, report_path, state = self._outputs("alignment")
        result = align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
        )
        self.assertEqual(result.status, "complete")
        self.assertTrue(master.exists())
        self.assertFalse(state.exists())

        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(report["status"], "complete")
        self.assertEqual(report["errors"], [])
        self.assertNotIn(str(self.root), report_path.read_text(encoding="utf-8"))
        with wave.open(str(master), "rb") as wav:
            self.assertEqual(wav.getframerate(), 48_000)
            self.assertEqual(wav.getnframes(), 1_248_000)

        reference = read_wav(self.fixture / "cam_a.wav", "cam_a", 48_000)
        reference_hits = detect_kicks(
            reference.mono(), 48_000, min_required=len(report["bar_starts_samples"])
        ).hits

        by_camera = {item["camera"]: item for item in report["cameras"]}
        self.assertFalse(by_camera["cam_b"]["phase_check"]["flagged"])
        self.assertTrue(by_camera["cam_c"]["phase_check"]["flagged"])
        self.assertEqual(
            [item["camera"] for item in report["quality_flags"]], ["cam_c"]
        )
        self.assertLess(by_camera["cam_c"]["effective_gain"], 0.5)

        for camera in ("cam_a", "cam_b", "cam_c"):
            camera_report = by_camera[camera]
            hits = camera_report["hits_samples"]
            self.assertGreaterEqual(
                camera_report["detected_hit_count"],
                len(report["bar_starts_samples"]),
            )
            self.assertEqual(
                len(camera_report["hit_intervals_samples"]), len(hits) - 1
            )
            if camera == "cam_a":
                continue
            curve_data = camera_report["drift_curve"]
            self.assertTrue(
                all(isinstance(value, int) for value in curve_data["offsets_samples"])
            )
            self.assertGreater(curve_data["ticks_samples"][0], 0)
            self.assertGreater(
                abs(curve_data["offsets_samples"][-1] - curve_data["offsets_samples"][0]),
                10,
            )

            aux = read_wav(self.fixture / f"{camera}.wav", camera, 48_000)
            curve = DriftCurve(
                ticks=list(curve_data["ticks_samples"]),
                offsets=list(curve_data["offsets_samples"]),
                bars=list(curve_data["bar_starts_samples"]),
                base_hits=list(curve_data["reference_hits_samples"]),
                aux_hits=list(curve_data["source_hits_samples"]),
                correlations=list(curve_data["local_correlations"]),
            )
            positions = np.arange(reference.frame_count, dtype=np.float64)
            aligned = sample_at_reference_positions(
                aux.samples, positions, curve, out_channels=1
            )[:, 0]
            aligned_hits = detect_kicks(
                aligned,
                48_000,
                min_required=len(report["bar_starts_samples"]),
            ).hits
            for tick in curve.ticks:
                base_hit = nearest_hit(reference_hits, tick, 4_800)
                aligned_hit = nearest_hit(aligned_hits, tick, 4_800)
                self.assertIsNotNone(base_hit)
                self.assertIsNotNone(aligned_hit)
                self.assertLessEqual(abs(int(aligned_hit) - int(base_hit)), 2)

    def test_repeated_run_is_byte_stable(self) -> None:
        master, report_path, state = self._outputs("stable")
        align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
        )
        first_master = master.read_bytes()
        first_report = report_path.read_bytes()
        align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
        )
        self.assertEqual(first_master, master.read_bytes())
        self.assertEqual(first_report, report_path.read_bytes())

    def test_interrupted_resume_is_byte_identical_to_oneshot(self) -> None:
        oneshot_master, oneshot_report, oneshot_state = self._outputs("oneshot")
        align_project(
            self.project,
            master_path=oneshot_master,
            report_path=oneshot_report,
            snapshot_path=oneshot_state,
            oneshot=True,
        )
        expected = oneshot_master.read_bytes()

        master, report_path, state = self._outputs("resume")
        interrupted = align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
            interrupt_after_blocks=3,
        )
        self.assertEqual(interrupted.status, "interrupted")
        self.assertFalse(master.exists())
        self.assertTrue(state.exists())
        partial_report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertEqual(partial_report["report_kind"], "recovery")
        self.assertEqual(partial_report["render"]["completed_blocks"], 3)
        snapshot = json.loads(state.read_text(encoding="utf-8"))
        self.assertEqual(snapshot["completed_blocks"], 3)
        self.assertEqual(snapshot["project_digest"], partial_report["project_digest"])
        self.assertEqual(set(snapshot["drift_curves"]), {"cam_b", "cam_c"})

        resumed = align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
        )
        self.assertEqual(resumed.status, "complete")
        self.assertEqual(resumed.report["resume_count"], 1)
        self.assertEqual(expected, master.read_bytes())
        self.assertFalse(state.exists())

    def test_resume_digest_mismatch_discards_old_progress(self) -> None:
        master, report_path, state = self._outputs("mismatch")
        align_project(
            self.project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
            interrupt_after_blocks=3,
        )
        old_state = json.loads(state.read_text(encoding="utf-8"))
        self.assertEqual(old_state["completed_blocks"], 3)

        changed_project = self.fixture / "mismatch_project.json"
        project_data = json.loads(self.project.read_text(encoding="utf-8"))
        project_data["render"]["block_samples"] = 8_192
        changed_project.write_text(
            json.dumps(project_data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        result = align_project(
            changed_project,
            master_path=master,
            report_path=report_path,
            snapshot_path=state,
            interrupt_after_blocks=1,
        )
        self.assertEqual(result.status, "interrupted")
        self.assertEqual(result.report["render"]["completed_blocks"], 1)
        self.assertEqual(result.report["resume_count"], 0)
        codes = [item["code"] for item in result.report["recovery_events"]]
        self.assertIn("E_RESUME_MISMATCH", codes)
        self.assertFalse(master.exists())
        new_state = json.loads(state.read_text(encoding="utf-8"))
        self.assertEqual(new_state["completed_blocks"], 1)

    def test_missing_track_cli_is_structured_and_nonzero(self) -> None:
        project_data = json.loads(self.project.read_text(encoding="utf-8"))
        for track in project_data["tracks"]:
            if track["camera"] == "cam_b":
                track["path"] = "missing_cam_b.wav"
        broken_project = self.fixture / "missing_project.json"
        broken_project.write_text(
            json.dumps(project_data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        master, report, state = self._outputs("missing")
        process = subprocess.run(
            [
                sys.executable,
                "-m",
                "drum_align",
                "render",
                "--project",
                str(broken_project),
                "--master",
                str(master),
                "--report",
                str(report),
                "--snapshot",
                str(state),
            ],
            cwd=REPO_ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertNotEqual(process.returncode, 0)
        payload = json.loads(process.stderr)
        self.assertEqual(payload["errors"][0]["code"], "E_TRACK_MISSING")
        self.assertEqual(payload["errors"][0]["camera"], "cam_b")
        self.assertIn("reason", payload["errors"][0])
        self.assertFalse(master.exists())

    def test_short_track_is_a_hard_error(self) -> None:
        short_path = self.fixture / "short_cam_b.wav"
        with wave.open(str(short_path), "wb") as wav:
            wav.setnchannels(1)
            wav.setsampwidth(2)
            wav.setframerate(48_000)
            wav.writeframes(np.zeros(1_000, dtype="<i2").tobytes())
        project_data = json.loads(self.project.read_text(encoding="utf-8"))
        for track in project_data["tracks"]:
            if track["camera"] == "cam_b":
                track["path"] = short_path.name
        broken_project = self.fixture / "short_project.json"
        broken_project.write_text(
            json.dumps(project_data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        with self.assertRaises(BusinessError) as raised:
            align_project(broken_project, oneshot=True)
        self.assertEqual(raised.exception.code, "E_TRACK_SHORT")
        self.assertEqual(raised.exception.camera, "cam_b")

    def test_bar_count_and_order_are_validated(self) -> None:
        project_data = json.loads(self.project.read_text(encoding="utf-8"))
        project_data["bar_starts_samples"] = [100, 90]
        project_data["min_bar_starts"] = 2
        broken = self.fixture / "bad_bars.json"
        broken.write_text(json.dumps(project_data), encoding="utf-8")
        with self.assertRaises(BusinessError) as raised:
            align_project(broken, oneshot=True)
        self.assertEqual(raised.exception.code, "E_BAR_STARTS_ORDER")

        project_data["bar_starts_samples"] = [100]
        project_data["min_bar_starts"] = 2
        broken.write_text(json.dumps(project_data), encoding="utf-8")
        with self.assertRaises(BusinessError) as raised:
            align_project(broken, oneshot=True)
        self.assertEqual(raised.exception.code, "E_BAR_STARTS_COUNT")


if __name__ == "__main__":
    unittest.main()
