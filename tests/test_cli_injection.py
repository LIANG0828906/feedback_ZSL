from __future__ import annotations

import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from mediaforge.injection import InjectionConfig
from mediaforge.jsonutil import sha256_bytes
from mediaforge.truth import compute_truth_fingerprint

from .support import ROOT, SAMPLES


COMBINED_INJECTION = {
    "metadata_conflict": {"track_id": "video_main", "language": "ja-JP"},
    "timestamp_inversion": {"track_id": "video_main", "frame_index": 2},
    "missing_track": {"track_id": "audio_main"},
    "corrupt_segment": {"track_id": "audio_main", "byte_offset": 73},
    "parameter_version": "inject-combined-p1",
}


def _write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


class CommandLineInjectionTests(unittest.TestCase):
    def setUp(self) -> None:
        self._temp = tempfile.TemporaryDirectory(prefix="mf-cli-inject-")
        self.scratch = Path(self._temp.name)
        self.checkpoints = self.scratch / "checkpoints"
        self.environment = {
            **os.environ,
            "TMPDIR": str(self.scratch),
            "QT_QPA_PLATFORM": "offscreen",
        }

    def tearDown(self) -> None:
        self._temp.cleanup()

    def _run_pack(self, sample: Path, injection: Path | None, *, segmented: bool) -> subprocess.CompletedProcess[str]:
        artifact = self.scratch / "artifact.mfz"
        command = [
            sys.executable,
            "-m",
            "mediaforge",
            "pack",
            "--sample",
            str(sample),
            "--source-version",
            "v3",
            "--target",
            "v3",
            "--out",
            str(artifact),
        ]
        if segmented:
            command.extend(["--segmented", "--checkpoint", str(self.checkpoints)])
        if injection is not None:
            command.extend(["--injection", str(injection)])
        return subprocess.run(
            command,
            cwd=ROOT,
            capture_output=True,
            text=True,
            env=self.environment,
        )

    def test_without_input_corruption_keeps_every_other_fault(self) -> None:
        config = InjectionConfig.from_dict(COMBINED_INJECTION)
        self.assertEqual(
            config.active,
            ("conflict", "order", "missing_track", "corrupt"),
        )
        stripped = config.without_input_corruption()
        self.assertIsNone(stripped.corrupt_track)
        self.assertEqual(
            stripped.active,
            ("conflict", "order", "missing_track"),
        )
        self.assertEqual(config.corrupt_track, "audio_main")

    def test_segmented_corrupt_only_still_fails_at_segment_stage(self) -> None:
        injection = SAMPLES / "injections" / "corrupt_segment.json"
        result = self._run_pack(SAMPLES / "g01_neon_city", injection, segmented=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        payload = json.loads(result.stdout)
        self.assertFalse(payload["ok"])
        failures = payload["result"]["failures"]
        self.assertEqual([item["code"] for item in failures], ["E_CORRUPT"])
        self.assertEqual(failures[0]["track_id"], "audio_main")
        self.assertFalse((self.scratch / "artifact.mfz").exists())

    def test_combined_segmented_injection_reports_metadata_conflict(self) -> None:
        config_path = self.scratch / "injection.json"
        _write_json(
            config_path,
            {
                "metadata_conflict": COMBINED_INJECTION["metadata_conflict"],
                "corrupt_segment": COMBINED_INJECTION["corrupt_segment"],
                "parameter_version": COMBINED_INJECTION["parameter_version"],
            },
        )
        result = self._run_pack(SAMPLES / "g01_neon_city", config_path, segmented=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        error = json.loads(result.stderr)["error"]
        self.assertEqual(error["code"], "E_CONFLICT")
        self.assertEqual(error["field_path"], "$.tracks[0].language")
        self.assertFalse((self.scratch / "artifact.mfz").exists())

    def test_combined_segmented_injection_reports_timestamp_inversion(self) -> None:
        config_path = self.scratch / "injection.json"
        _write_json(
            config_path,
            {
                "timestamp_inversion": COMBINED_INJECTION["timestamp_inversion"],
                "corrupt_segment": COMBINED_INJECTION["corrupt_segment"],
                "parameter_version": COMBINED_INJECTION["parameter_version"],
            },
        )
        result = self._run_pack(SAMPLES / "g01_neon_city", config_path, segmented=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        error = json.loads(result.stderr)["error"]
        self.assertEqual(error["code"], "E_ORDER")
        self.assertIn("frames[2].pts_ticks", error["field_path"])

    def test_combined_segmented_injection_reports_missing_track(self) -> None:
        config_path = self.scratch / "injection.json"
        _write_json(
            config_path,
            {
                "missing_track": COMBINED_INJECTION["missing_track"],
                "corrupt_segment": COMBINED_INJECTION["corrupt_segment"],
                "parameter_version": COMBINED_INJECTION["parameter_version"],
            },
        )
        result = self._run_pack(SAMPLES / "g01_neon_city", config_path, segmented=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        error = json.loads(result.stderr)["error"]
        self.assertEqual(error["code"], "E_TRACK")
        self.assertEqual(error["field_path"], "$.tracks[1]")

    def test_all_four_injections_together_are_not_silently_dropped(self) -> None:
        config_path = self.scratch / "injection.json"
        _write_json(config_path, COMBINED_INJECTION)
        result = self._run_pack(SAMPLES / "g01_neon_city", config_path, segmented=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        error = json.loads(result.stderr)["error"]
        self.assertIn(error["code"], {"E_CONFLICT", "E_ORDER", "E_TRACK", "E_CORRUPT"})
        self.assertNotIn("NameError", result.stderr)


class NonPngCoverCommandLineTests(unittest.TestCase):
    def setUp(self) -> None:
        self._temp = tempfile.TemporaryDirectory(prefix="mf-cli-cover-")
        self.scratch = Path(self._temp.name)
        self.sample = self.scratch / "g01_neon_city"
        shutil.copytree(SAMPLES / "g01_neon_city", self.sample)
        self.environment = {
            **os.environ,
            "TMPDIR": str(self.scratch),
            "QT_QPA_PLATFORM": "offscreen",
        }

    def tearDown(self) -> None:
        self._temp.cleanup()

    def test_pack_with_non_png_cover_returns_e_corrupt(self) -> None:
        cover = b"NOT-A-PNG-FILE"
        metadata_path = self.sample / "metadata" / "meta_v3.json"
        metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
        metadata["resources"]["cover"]["base64"] = base64.b64encode(cover).decode()
        metadata_path.write_text(json.dumps(metadata), encoding="utf-8")

        truth_path = self.sample / "truth.json"
        truth = json.loads(truth_path.read_text(encoding="utf-8"))
        truth["cover_sha256"] = sha256_bytes(cover)
        truth["fingerprint"] = compute_truth_fingerprint(truth)
        truth_path.write_text(json.dumps(truth), encoding="utf-8")

        artifact = self.scratch / "cover.mfz"
        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "mediaforge",
                "pack",
                "--sample",
                str(self.sample),
                "--source-version",
                "v3",
                "--target",
                "v3",
                "--out",
                str(artifact),
            ],
            cwd=ROOT,
            capture_output=True,
            text=True,
            env=self.environment,
        )
        self.assertEqual(result.returncode, 2, result.stdout)
        error = json.loads(result.stderr)["error"]
        self.assertEqual(error["code"], "E_CORRUPT")
        self.assertEqual(error["field_path"], "$.cover")
        self.assertNotIn("NameError", result.stderr)
        self.assertFalse(artifact.exists())


if __name__ == "__main__":
    unittest.main()
