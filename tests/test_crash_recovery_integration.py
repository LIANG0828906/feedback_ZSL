import json
import os
import subprocess
import sys
import tempfile
import unittest

from npr.canonical import read_json, write_json_atomic
from npr.params import OutlineParams, ShadowParams, SmearParams
from npr.pipeline import Pipeline
from npr.scene import parse_scene
from tests.test_scene import square_scene


def scene():
    doc = square_scene("a", 0.0, 0.0, 6.0)
    doc["paths"].append(square_scene("b", 6.0, 0.0, 6.0)["paths"][0])
    return parse_scene(doc)


class CrashRecoveryIntegration(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.workdir = os.path.join(self.tmp.name, "work")
        self.scene_path = os.path.join(self.tmp.name, "scene.json")
        write_json_atomic(self.scene_path, scene().doc)
        pipeline = Pipeline(self.workdir)
        self.manifest1, _ = pipeline.recompute(
            scene(), OutlineParams(), ShadowParams(), SmearParams()
        )

    def tearDown(self):
        self.tmp.cleanup()

    def test_hard_kill_during_recompute_then_disk_only_resume(self):
        env = dict(os.environ)
        env["PYTHONPATH"] = os.getcwd() + os.pathsep + env.get("PYTHONPATH", "")
        crash = subprocess.run(
            [
                sys.executable,
                "-m",
                "npr.crash_demo",
                "crash",
                self.workdir,
                self.scene_path,
                "1",
                "2",
            ],
            env=env,
            capture_output=True,
            text=True,
        )
        self.assertEqual(crash.returncode, 9, crash.stderr)

        # The manifest must still describe only the fully committed v1.
        manifest = read_json(os.path.join(self.workdir, "store", "manifest.json"))
        self.assertEqual(manifest["version"], 1)

        # Orphaned v2 directory may exist with partial/temp chunks.
        v2 = os.path.join(self.workdir, "store", "chunks", "v2")
        self.assertTrue(os.path.isdir(v2))

        resume = subprocess.run(
            [
                sys.executable,
                "-m",
                "npr.crash_demo",
                "resume",
                self.workdir,
                self.scene_path,
            ],
            env=env,
            capture_output=True,
            text=True,
        )
        self.assertEqual(resume.returncode, 0, resume.stderr)
        result = json.loads(resume.stdout)
        self.assertEqual(result["recovered_version"], 1)
        self.assertEqual(result["completed_version"], 2)
        self.assertEqual(result["layer_count"], 3)
        self.assertFalse(os.path.exists(v2 + ".tmp"))

        # Recovered state passes the hash-chain validation again.
        pipeline = Pipeline(self.workdir)
        recovered, missing = pipeline.recover()
        self.assertEqual(missing, [])
        self.assertEqual(recovered["version"], 2)
        for layer in (0, 1, 2):
            strokes = pipeline.store.read_layer_strokes(2, layer)
            self.assertTrue(strokes)


if __name__ == "__main__":
    unittest.main()
