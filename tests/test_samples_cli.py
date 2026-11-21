import os
import tempfile
import unittest

from npr.canonical import read_json
from npr.cli import main
from npr.samples import PROFILES, generate_samples


class SampleAndCliTests(unittest.TestCase):
    def test_generator_is_byte_deterministic(self):
        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            ia = generate_samples(a)
            ib = generate_samples(b)
            self.assertEqual(ia, ib)
            for profile in PROFILES:
                name = f"scene_{profile}.json"
                with open(os.path.join(a, name), "rb") as fa, open(
                    os.path.join(b, name), "rb"
                ) as fb:
                    self.assertEqual(fa.read(), fb.read())

    def test_regression_cli_writes_baseline_and_trace(self):
        with tempfile.TemporaryDirectory() as workdir:
            rc = main(["regression", workdir])
            self.assertEqual(rc, 0)
            baseline = read_json(os.path.join(workdir, "baseline.json"))
            self.assertEqual([r["profile"] for r in baseline["runs"]],
                             ["tiny", "medium", "large"])
            for run in baseline["runs"]:
                self.assertEqual(run["final_version"], 4)
                self.assertEqual(len(run["first_manifest_hash"]), 64)
            trace = os.path.join(workdir, "run_medium", "trace.jsonl")
            self.assertTrue(os.path.exists(trace))
            with open(trace) as fh:
                lines = [line for line in fh if line.strip()]
            # 4 recomputes (initial, noop, outline change, shadow change) x 3 layers.
            self.assertEqual(len(lines), 12)

    def test_regression_is_reproducible_across_runs(self):
        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            main(["regression", a])
            main(["regression", b])
            for profile in PROFILES:
                ta = os.path.join(a, f"run_{profile}", "trace.jsonl")
                tb = os.path.join(b, f"run_{profile}", "trace.jsonl")
                with open(ta, "rb") as fa, open(tb, "rb") as fb:
                    self.assertEqual(fa.read(), fb.read())
            with open(os.path.join(a, "baseline.json"), "rb") as fa, open(
                os.path.join(b, "baseline.json"), "rb"
            ) as fb:
                self.assertEqual(fa.read(), fb.read())

    def test_preview_cli_renders_png(self):
        with tempfile.TemporaryDirectory() as workdir:
            main(["regression", workdir])
            png = os.path.join(workdir, "preview.png")
            rc = main(["preview", os.path.join(workdir, "run_tiny"), png])
            self.assertEqual(rc, 0)
            with open(png, "rb") as fh:
                self.assertEqual(fh.read(8), b"\x89PNG\r\n\x1a\n")


if __name__ == "__main__":
    unittest.main()
