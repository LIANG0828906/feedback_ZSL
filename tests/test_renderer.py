import json
import shutil
import subprocess
import sys
import unittest
from pathlib import Path

from space_audio.cli import main
from space_audio.demo import build_demo_scene, write_demo_scene
from space_audio.report import compare_master_files
from space_audio.synthesis import generate_all_dry_sources


class RendererCliTest(unittest.TestCase):
    def setUp(self) -> None:
        self.root = Path(__file__).parent / "_tmp_renderer"
        shutil.rmtree(self.root, ignore_errors=True)
        self.root.mkdir(parents=True, exist_ok=True)
        self.scene_path = write_demo_scene(self.root / "scene.json")
        self.scene = build_demo_scene()
        generate_all_dry_sources(self.scene, self.root)
        self.full = self.root / "full"
        self.assertEqual(
            main(["render", "--scene", str(self.scene_path), "--work-dir", str(self.full),
                  "--skip-generation"]),
            0,
        )

    def test_full_render_report_stable(self) -> None:
        report = json.loads((self.full / "report.json").read_text())
        self.assertEqual(report["master"]["total_samples"], 216000)
        self.assertEqual(len(report["frames"]), report["schema_version"] and 241)
        self.assertNotIn("absolute", json.dumps(report))

    def test_hard_kill_resume_byte_identical(self) -> None:
        killed = self.root / "killed"
        with self.assertRaises(SystemExit) as context:
            main(["render", "--scene", str(self.scene_path), "--work-dir", str(killed),
                  "--skip-generation", "--kill-after-block", "6"])
        self.assertEqual(context.exception.code, 99)
        self.assertEqual(
            main(["render", "--scene", str(self.scene_path), "--work-dir", str(killed),
                  "--skip-generation"]),
            0,
        )
        comparison = compare_master_files(self.full / "master.wav", killed / "master.wav")
        self.assertTrue(comparison["identical"])

    def test_resume_mismatch_discards_progress(self) -> None:
        import copy

        killed = self.root / "mismatch"
        with self.assertRaises(SystemExit):
            main(["render", "--scene", str(self.scene_path), "--work-dir", str(killed),
                  "--skip-generation", "--kill-after-block", "5"])
        mutated = copy.deepcopy(self.scene)
        mutated["project"]["seed"] += 1
        mutated_path = self.root / "scene_mutated.json"
        mutated_path.write_text(json.dumps(mutated))
        code = main(["render", "--scene", str(mutated_path), "--work-dir", str(killed),
                     "--skip-generation"])
        self.assertEqual(code, 0)
        self.assertTrue((killed / "resume_mismatch.json").is_file())
        payload = json.loads((killed / "resume_mismatch.json").read_text())
        self.assertEqual(payload["code"], "E_RESUME_MISMATCH")

    def test_report_has_no_timestamp_or_absolute_path(self) -> None:
        text = (self.full / "report.json").read_text()
        self.assertNotIn(str(self.root.absolute()), text)
        report = json.loads(text)
        for frame in report["frames"][:5]:
            self.assertIn("reverb", frame)
            self.assertIn("itd_s", frame["sources"][0])


if __name__ == "__main__":
    unittest.main()

