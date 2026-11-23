from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

from space_audio.cli import main as cli_main


def run_cli(argv: list[str]) -> int:
    with contextlib.redirect_stdout(io.StringIO()):
        return cli_main(argv)


class CliGeometryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="spatial-audio-cli-")
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_verify_propagates_non_default_geometry(self) -> None:
        workdir = self.root / "verify"
        exit_code = run_cli(
            [
                "verify",
                "--workdir",
                str(workdir),
                "--frames",
                "4",
                "--frame-len",
                "1536",
                "--hop",
                "768",
                "--window-len",
                "512",
            ]
        )
        self.assertEqual(exit_code, 0)
        report = json.loads(
            (workdir / "verification.json").read_text(encoding="utf-8")
        )
        self.assertTrue(report["passed"])
        analysis = report["config"]["analysis"]
        self.assertEqual(analysis["frame_len"], 1536)
        self.assertEqual(analysis["hop"], 768)
        self.assertEqual(analysis["window_len"], 512)
        realtime = report["three_paths"]["realtime"]
        self.assertEqual(realtime["committed_frames"], 4)
        self.assertEqual(realtime["total_frames"], 4)

    def test_run_consumes_manifest_analysis_profile(self) -> None:
        sample_dir = self.root / "sample"
        exit_code = run_cli(
            [
                "generate",
                "--workdir",
                str(sample_dir),
                "--frames",
                "4",
                "--frame-len",
                "1280",
                "--hop",
                "640",
                "--window-len",
                "320",
            ]
        )
        self.assertEqual(exit_code, 0)
        manifest = sample_dir / "studio_quad_manifest.json"
        self.assertTrue(manifest.is_file())
        report_path = self.root / "run_report.json"
        exit_code = run_cli(
            [
                "run",
                "--manifest",
                str(manifest),
                "--checkpoint",
                str(self.root / "run.sqlite"),
                "--report",
                str(report_path),
            ]
        )
        self.assertEqual(exit_code, 0)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertTrue(report["report_complete"])
        analysis = report["config"]["analysis"]
        self.assertEqual(analysis["frame_len"], 1280)
        self.assertEqual(analysis["hop"], 640)
        self.assertEqual(analysis["window_len"], 320)
        self.assertEqual(report["committed_frames"], 4)

    def test_run_cli_geometry_overrides_manifest_profile(self) -> None:
        sample_dir = self.root / "sample_override"
        exit_code = run_cli(
            [
                "generate",
                "--workdir",
                str(sample_dir),
                "--frames",
                "4",
                "--frame-len",
                "1280",
                "--hop",
                "640",
                "--window-len",
                "320",
            ]
        )
        self.assertEqual(exit_code, 0)
        manifest = sample_dir / "studio_quad_manifest.json"
        report_path = self.root / "override_report.json"
        exit_code = run_cli(
            [
                "run",
                "--manifest",
                str(manifest),
                "--checkpoint",
                str(self.root / "override.sqlite"),
                "--report",
                str(report_path),
                "--window-len",
                "256",
            ]
        )
        self.assertEqual(exit_code, 0)
        report = json.loads(report_path.read_text(encoding="utf-8"))
        self.assertTrue(report["report_complete"])
        analysis = report["config"]["analysis"]
        self.assertEqual(analysis["frame_len"], 1280)
        self.assertEqual(analysis["hop"], 640)
        self.assertEqual(analysis["window_len"], 256)


if __name__ == "__main__":
    unittest.main()
