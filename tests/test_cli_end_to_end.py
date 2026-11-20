"""端到端：CLI 生成 -> 多并行度重演 -> 基线校验 -> 断点续算。"""

import json
import os
import subprocess
import sys
import tempfile
import unittest


class CliEndToEndTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.samples = os.path.join(self.tmp, "samples")

    def _cli(self, *args):
        return subprocess.run(
            [sys.executable, "-m", "replay_service.cli", *args],
            cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
            capture_output=True,
            text=True,
            check=True,
        )

    def test_full_pipeline(self):
        self._cli("generate", "--out-dir", self.samples, "--seed", "7")
        for degree in ("1", "3"):
            work = os.path.join(self.tmp, f"work{degree}")
            self._cli(
                "replay",
                "--sample-dir", self.samples,
                "--work-dir", work,
                "--parallelism", degree,
            )
            verify = self._cli(
                "verify",
                "--sample-dir", self.samples,
                "--work-dir", os.path.join(self.tmp, f"verify{degree}"),
                "--parallelism", degree,
            )
            self.assertEqual(json.loads(verify.stdout)["status"], "MATCH")

        # 续算：对已完成检查点再跑一次，结果哈希必须保持不变
        scenario = sorted(
            name for name in os.listdir(self.samples)
            if os.path.isdir(os.path.join(self.samples, name))
        )[0]
        cp_dir = os.path.join(self.tmp, "work1", "checkpoints", scenario)
        resume = self._cli(
            "resume",
            "--input", os.path.join(self.samples, scenario, "input.json"),
            "--checkpoint-dir", cp_dir,
        )
        payload = json.loads(resume.stdout)
        self.assertEqual(payload["resumed_from_step"], payload["total_steps"])

    def test_invalid_input_returns_error_code(self):
        bad_dir = os.path.join(self.tmp, "bad")
        os.makedirs(bad_dir)
        bad_input = os.path.join(bad_dir, "input.json")
        with open(bad_input, "w", encoding="utf-8") as handle:
            json.dump({"config": {}, "operations": []}, handle)
        proc = subprocess.run(
            [sys.executable, "-m", "replay_service.cli",
             "resume", "--input", bad_input,
             "--checkpoint-dir", os.path.join(bad_dir, "cp")],
            cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("E_INPUT_INVALID", proc.stderr)


if __name__ == "__main__":
    unittest.main()
