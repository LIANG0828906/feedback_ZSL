import filecmp
import json
import os
import tempfile
import unittest

import numpy as np

from pcwb.demo import run_demo
from pcwb.format import read_cloud
from pcwb.samples import generate_sample_set


class SampleTests(unittest.TestCase):
    def test_deterministic_sample_generation(self):
        with tempfile.TemporaryDirectory() as a, tempfile.TemporaryDirectory() as b:
            ma = generate_sample_set(a, sizes=(256, 512), seed=123)
            mb = generate_sample_set(b, sizes=(256, 512), seed=123)
            self.assertEqual(ma, mb)
            for sample in ma["samples"]:
                self.assertTrue(filecmp.cmp(
                    os.path.join(a, sample["name"]),
                    os.path.join(b, sample["name"]), shallow=False))
                xyz, rgb, _, sha = read_cloud(
                    os.path.join(a, sample["name"]))
                self.assertEqual(sha, sample["sha256"])
                self.assertEqual(xyz.shape[0], sample["point_count"])

    def test_samples_contain_empty_region(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest = generate_sample_set(tmp, sizes=(512,), seed=7)
            xyz, _, _, _ = read_cloud(
                os.path.join(tmp, manifest["samples"][0]["name"]))
            inside = np_none_in_cavity(xyz)
            self.assertTrue(inside)


def np_none_in_cavity(xyz, radius: float = 1.2) -> bool:
    import numpy as np
    return not np.any(np.sum(xyz ** 2, axis=1) <= radius ** 2)


class DemoReproTests(unittest.TestCase):
    def test_demo_byte_reproducible(self):
        with tempfile.TemporaryDirectory() as root:
            dir_a = os.path.join(root, "a")
            dir_b = os.path.join(root, "b")
            summary_a = run_demo(dir_a, seed=20261121)
            summary_b = run_demo(dir_b, seed=20261121)

            # trajectory logs must be byte-identical
            self.assertTrue(filecmp.cmp(
                os.path.join(dir_a, "trajectory.jsonl"),
                os.path.join(dir_b, "trajectory.jsonl"), shallow=False))
            # summaries identical
            self.assertEqual(summary_a, summary_b)
            # every committed version file byte-identical across runs
            va = sorted(os.listdir(os.path.join(dir_a, "store", "versions")))
            vb = sorted(os.listdir(os.path.join(dir_b, "store", "versions")))
            self.assertEqual(va, vb)
            for name in va:
                self.assertTrue(filecmp.cmp(
                    os.path.join(dir_a, "store", "versions", name),
                    os.path.join(dir_b, "store", "versions", name),
                    shallow=False), name)
            # rendered frames: the CPU backend is byte-exact; the EGL
            # llvmpipe backend may differ by +/-1 quantization on individual
            # pixels across separate GL contexts, so compare with a tight
            # tolerance while point data and logs stay byte-identical.
            fa = sorted(os.listdir(os.path.join(dir_a, "frames")))
            fb = sorted(os.listdir(os.path.join(dir_b, "frames")))
            self.assertEqual(fa, fb)
            for name in fa:
                pa = _read_ppm(os.path.join(dir_a, "frames", name))
                pb = _read_ppm(os.path.join(dir_b, "frames", name))
                self.assertEqual(pa.shape, pb.shape)
                self.assertTrue(
                    np.max(np.abs(pa.astype(np.int16)
                                  - pb.astype(np.int16))) <= 1, name)

    def test_demo_cpu_backend_byte_reproducible(self):
        with tempfile.TemporaryDirectory() as root:
            dir_a = os.path.join(root, "a")
            dir_b = os.path.join(root, "b")
            run_demo(dir_a, seed=20261121, backend="cpu")
            run_demo(dir_b, seed=20261121, backend="cpu")
            for name in sorted(os.listdir(os.path.join(dir_a, "frames"))):
                self.assertTrue(filecmp.cmp(
                    os.path.join(dir_a, "frames", name),
                    os.path.join(dir_b, "frames", name),
                    shallow=False), name)

    def test_demo_log_contents(self):
        with tempfile.TemporaryDirectory() as root:
            run_demo(root, seed=42)
            with open(os.path.join(root, "trajectory.jsonl"), "rb") as h:
                lines = [json.loads(line) for line in h if line.strip()]
            events = [line["event"] for line in lines]
            self.assertIn("selection", events)
            self.assertIn("commit", events)
            self.assertIn("edit_cancelled", events)
            self.assertIn("checkout", events)
            self.assertIn("diff", events)
            self.assertIn("frame", events)
            commits = [line for line in lines if line["event"] == "commit"]
            for line in commits:
                self.assertIn("input_hash", line)
                self.assertIn("output_hash", line)
                self.assertIn("operator", line)
            frames = [line for line in lines if line["event"] == "frame"]
            self.assertTrue(all("frame_ms" in f for f in frames))


if __name__ == "__main__":
    unittest.main()


def _read_ppm(path: str) -> np.ndarray:
    with open(path, "rb") as handle:
        assert handle.readline().strip() == b"P6"
        width, height = (int(v) for v in handle.readline().split())
        int(handle.readline())
        return np.frombuffer(
            handle.read(), dtype=np.uint8).reshape(height, width, 3)
