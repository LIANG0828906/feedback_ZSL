import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest


class SignalHandlingTests(unittest.TestCase):
    def test_sigint_completes_current_tile_and_allows_resume(self):
        workdir = tempfile.mkdtemp(prefix="brocade-signal-")
        try:
            scene = os.path.join(workdir, "scene.json")
            output = os.path.join(workdir, "frames")
            generate = subprocess.run(
                [sys.executable, "-m", "brocade_embroidery.cli",
                 "generate-scene", "--scene", scene, "--small"],
                cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                check=True, capture_output=True, text=True)
            self.assertEqual(generate.returncode, 0)
            process = subprocess.Popen(
                [sys.executable, "-m", "brocade_embroidery.cli",
                 "render", "--scene", scene, "--output", output,
                 "--frame", "0", "--allow-interrupt"],
                cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            time.sleep(0.35)
            process.send_signal(signal.SIGINT)
            stdout, stderr = process.communicate(timeout=20)
            self.assertEqual(process.returncode, 0, stderr.decode())
            recovery_path = os.path.join(
                output, "frame_00", "recovery.json")
            self.assertTrue(os.path.exists(recovery_path))
            with open(recovery_path, encoding="utf-8") as handle:
                recovery = json.load(handle)
            self.assertTrue(recovery["interrupted"])
            self.assertGreaterEqual(recovery["completed_tiles"], 0)
            self.assertLess(recovery["completed_tiles"],
                            recovery["total_tiles"])

            resumed = subprocess.run(
                [sys.executable, "-m", "brocade_embroidery.cli",
                 "render", "--scene", scene, "--output", output,
                 "--frame", "0", "--allow-interrupt"],
                cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                check=True, capture_output=True, text=True)
            self.assertEqual(resumed.returncode, 0)
            self.assertTrue(os.path.exists(
                os.path.join(output, "frame_00", "render.rgba.png")))
        finally:
            shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
