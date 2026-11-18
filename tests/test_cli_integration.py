import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

from kiln_glass.cli import main
from kiln_glass.scene import build_default_scene, write_scene


class CliIntegrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="kiln_cli_")
        self.scene_path = os.path.join(self.tmp, "scene.json")
        write_scene(build_default_scene(), self.scene_path)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_validate_exit_codes(self):
        self.assertEqual(
            main(["validate", "--scene", self.scene_path]), 0
        )
        bad = json.load(open(self.scene_path, encoding="utf-8"))
        bad["view_angles"] = [0.1, 0.1]
        bad_path = os.path.join(self.tmp, "bad.json")
        write_scene(bad, bad_path)
        report = os.path.join(self.tmp, "val.json")
        code = main(["validate", "--scene", bad_path, "--report", report])
        self.assertEqual(code, 2)
        payload = json.load(open(report, encoding="utf-8"))
        self.assertEqual(payload["status"], "validation_failed")
        self.assertTrue(payload["issues"][0]["code"])

    def test_render_and_compare_cli(self):
        out_a = os.path.join(self.tmp, "a")
        out_b = os.path.join(self.tmp, "b")
        self.assertEqual(
            main(["render", "--scene", self.scene_path, "--output", out_a]), 0
        )
        self.assertEqual(
            main(["render", "--scene", self.scene_path, "--output", out_b]), 0
        )
        report = os.path.join(self.tmp, "diff.json")
        self.assertEqual(
            main([
                "compare", "--reference", out_a, "--candidate", out_b,
                "--report", report,
            ]),
            0,
        )
        self.assertEqual(json.load(open(report))["status"], "identical")

    def test_scheduled_interrupt_exit_code_zero_and_resume(self):
        out = os.path.join(self.tmp, "interrupt")
        code = main([
            "render", "--scene", self.scene_path, "--output", out,
            "--interrupt-after-tiles", "5",
        ])
        self.assertEqual(code, 0)
        self.assertTrue(
            os.path.exists(os.path.join(out, "progress_snapshot.json"))
        )
        recovery = json.load(open(os.path.join(out, "recovery_report.json")))
        self.assertEqual(recovery["status"], "resumable")
        self.assertEqual(
            main(["render", "--scene", self.scene_path, "--output", out]), 0
        )
        reference = os.path.join(self.tmp, "reference")
        self.assertEqual(
            main([
                "render", "--scene", self.scene_path, "--output", reference
            ]),
            0,
        )
        diff_report = os.path.join(self.tmp, "resume_diff.json")
        self.assertEqual(
            main([
                "compare", "--reference", reference, "--candidate", out,
                "--report", diff_report,
            ]),
            0,
        )

    def test_sigint_completes_current_tile_and_exits_zero(self):
        env = dict(os.environ)
        out = os.path.join(self.tmp, "sigint")
        cmd = [
            sys.executable, "-m", "kiln_glass", "render",
            "--scene", self.scene_path, "--output", out,
            "--handle-sigint",
        ]
        proc = subprocess.Popen(
            cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env
        )
        time.sleep(2.0)
        proc.send_signal(signal.SIGINT)
        stdout, stderr = proc.communicate(timeout=60)
        self.assertEqual(proc.returncode, 0, stderr.decode())
        self.assertIn(b"graceful interrupt", stdout)
        self.assertTrue(
            os.path.exists(os.path.join(out, "progress_snapshot.json"))
        )
        self.assertTrue(
            os.path.exists(os.path.join(out, "recovery_report.json"))
        )


if __name__ == "__main__":
    unittest.main()
