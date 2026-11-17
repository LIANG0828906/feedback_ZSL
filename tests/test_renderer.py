"""End-to-end tests for the willow lantern soft-raster renderer.

Runs the CLI as a subprocess (stdlib unittest only, no third-party test
deps).  Run with:  python -m unittest discover -s tests -v
"""

import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from willow_renderer import sample_scene  # noqa: E402


def run_cli(args, env_extra=None, cwd=ROOT):
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        [sys.executable, "-m", "willow_renderer"] + args,
        cwd=cwd, env=env, capture_output=True, text=True)


class WorkspaceMixin:
    def setUp(self):
        self.workdir = tempfile.mkdtemp(prefix="willow_test_")
        self.scene_path = os.path.join(self.workdir, "scene.json")
        sample_scene.write_sample_scene(
            self.scene_path, seed=20261118, image_size=128, tile_size=32)
        with open(self.scene_path, "r", encoding="utf-8") as fh:
            self.scene = json.load(fh)

    def tearDown(self):
        shutil.rmtree(self.workdir, ignore_errors=True)

    def write_scene(self, scene=None, name="scene.json"):
        scene = self.scene if scene is None else scene
        path = os.path.join(self.workdir, name)
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(scene, fh)
        return path

    def outdir(self, name):
        return os.path.join(self.workdir, name)

    def render(self, scene_path, outdir, *extra):
        return run_cli(["render", "--scene", scene_path, "--outdir", outdir,
                        *extra])


class TestValidation(WorkspaceMixin, unittest.TestCase):
    def assert_business_error(self, scene, expected_code):
        path = self.write_scene(scene, "bad_scene.json")
        proc = self.render(path, self.outdir("out_bad"))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        payload = json.loads(proc.stderr)
        self.assertEqual(payload["error"]["code"], expected_code)
        self.assertIn("field", payload["error"])
        return payload

    def test_mesh_index_out_of_range(self):
        scene = json.loads(json.dumps(self.scene))
        scene["layers"][0]["mesh"]["triangles"][0][0] = 10 ** 6
        payload = self.assert_business_error(scene, "MESH_INDEX_OUT_OF_RANGE")
        self.assertEqual(payload["error"]["layer_id"], 0)

    def test_triangle_degenerate(self):
        scene = json.loads(json.dumps(self.scene))
        mesh = scene["layers"][1]["mesh"]
        mesh["vertices"][mesh["triangles"][0][1]] = \
            list(mesh["vertices"][mesh["triangles"][0][0]])
        payload = self.assert_business_error(scene, "TRIANGLE_DEGENERATE")
        self.assertEqual(payload["error"]["layer_id"], 1)

    def test_transmittance_out_of_range(self):
        scene = json.loads(json.dumps(self.scene))
        scene["layers"][2]["transmittance"] = 1.5
        payload = self.assert_business_error(scene, "TRANSMITTANCE_OUT_OF_RANGE")
        self.assertEqual(payload["error"]["layer_id"], 2)
        self.assertIn("transmittance", payload["error"]["field"])

    def test_duplicate_layer_id(self):
        scene = json.loads(json.dumps(self.scene))
        scene["layers"][1]["layer_id"] = scene["layers"][0]["layer_id"]
        self.assert_business_error(scene, "DUPLICATE_LAYER_ID")

    def test_duplicate_hole_id(self):
        scene = json.loads(json.dumps(self.scene))
        holes = scene["layers"][0]["holes"]
        holes[1]["hole_id"] = holes[0]["hole_id"]
        payload = self.assert_business_error(scene, "DUPLICATE_HOLE_ID")
        self.assertEqual(payload["error"]["layer_id"], 0)

    def test_lamp_inside_geometry(self):
        scene = json.loads(json.dumps(self.scene))
        layer = scene["layers"][0]
        scene["lamps"][0]["position"] = [0.0, 0.0, layer["plane_z"]]
        payload = self.assert_business_error(scene, "LAMP_INSIDE_GEOMETRY")
        self.assertEqual(payload["error"]["layer_id"], 0)

    def test_missing_scene_file(self):
        proc = self.render(os.path.join(self.workdir, "nope.json"),
                           self.outdir("out_nope"))
        self.assertEqual(proc.returncode, 3)
        self.assertEqual(json.loads(proc.stderr)["error"]["code"], "SCENE_IO_ERROR")

    def test_malformed_json(self):
        path = os.path.join(self.workdir, "broken.json")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write("{not json")
        proc = self.render(path, self.outdir("out_broken"))
        self.assertEqual(proc.returncode, 3)
        self.assertEqual(json.loads(proc.stderr)["error"]["code"],
                         "SCENE_PARSE_ERROR")


class TestDeterminism(WorkspaceMixin, unittest.TestCase):
    def test_two_runs_byte_identical(self):
        out_a, out_b = self.outdir("a"), self.outdir("b")
        self.assertEqual(self.render(self.scene_path, out_a).returncode, 0)
        self.assertEqual(self.render(self.scene_path, out_b).returncode, 0)
        for name in ("rgba.png", "owner.png", "landings.json"):
            with open(os.path.join(out_a, name), "rb") as fh:
                data_a = fh.read()
            with open(os.path.join(out_b, name), "rb") as fh:
                data_b = fh.read()
            self.assertEqual(data_a, data_b, f"{name} differs between runs")

    def test_texture_tables_regenerate_identically(self):
        from willow_renderer import texture
        tables_a = texture.build_all_tables(self.scene)
        tables_b = texture.build_all_tables(self.scene)
        for lid in tables_a:
            self.assertTrue(np.array_equal(tables_a[lid].color_mod,
                                           tables_b[lid].color_mod))
            self.assertTrue(np.array_equal(tables_a[lid].hole_mask,
                                           tables_b[lid].hole_mask))

    def test_outputs_in_range_and_owner_outermost(self):
        from PIL import Image
        out = self.outdir("range")
        self.assertEqual(self.render(self.scene_path, out).returncode, 0)
        rgba = np.asarray(Image.open(os.path.join(out, "rgba.png")))
        owner = np.asarray(Image.open(os.path.join(out, "owner.png")))
        self.assertEqual(rgba.dtype, np.uint8)
        valid = set(range(len(self.scene["layers"]))) | {254, 255}
        self.assertTrue(set(np.unique(owner).tolist()) <= valid)


class TestInterruptResume(WorkspaceMixin, unittest.TestCase):
    def test_resume_matches_clean_run(self):
        clean = self.outdir("clean")
        self.assertEqual(self.render(self.scene_path, clean).returncode, 0)

        intr = self.outdir("intr")
        env = {"WILLOW_TILE_DELAY_MS": "120"}
        proc = subprocess.Popen(
            [sys.executable, "-m", "willow_renderer", "render",
             "--scene", self.scene_path, "--outdir", intr],
            cwd=ROOT, env={**os.environ, **env},
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        time.sleep(0.8)
        proc.send_signal(signal.SIGTERM)
        stdout, _ = proc.communicate(timeout=60)
        # Graceful interrupt: current tile committed, recovery report, exit 0.
        self.assertEqual(proc.returncode, 0, stdout)
        status = json.loads(stdout)
        self.assertEqual(status["status"], "interrupted")
        self.assertTrue(os.path.isfile(os.path.join(intr, "recovery_report.json")))
        self.assertTrue(os.path.isfile(os.path.join(intr, "_snapshot.json")))

        proc2 = self.render(self.scene_path, intr)
        self.assertEqual(proc2.returncode, 0, proc2.stderr)
        status2 = json.loads(proc2.stdout)
        self.assertEqual(status2["status"], "completed")
        self.assertTrue(len(status2["resumed_from_tiles"]) >= 1)

        for name in ("rgba.png", "owner.png", "landings.json"):
            with open(os.path.join(clean, name), "rb") as fh:
                expected = fh.read()
            with open(os.path.join(intr, name), "rb") as fh:
                actual = fh.read()
            self.assertEqual(expected, actual,
                             f"resumed {name} differs from clean run")


class TestSnapshotIntegrity(WorkspaceMixin, unittest.TestCase):
    def _make_partial_snapshot(self, outdir):
        env = {"WILLOW_TILE_DELAY_MS": "120"}
        proc = subprocess.Popen(
            [sys.executable, "-m", "willow_renderer", "render",
             "--scene", self.scene_path, "--outdir", outdir],
            cwd=ROOT, env={**os.environ, **env},
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        time.sleep(0.8)
        proc.send_signal(signal.SIGTERM)
        proc.communicate(timeout=60)
        self.assertEqual(proc.returncode, 0)

    def test_corrupt_snapshot_recomputed_by_default(self):
        clean = self.outdir("clean")
        self.assertEqual(self.render(self.scene_path, clean).returncode, 0)
        out = self.outdir("corrupt")
        self._make_partial_snapshot(out)
        with open(os.path.join(out, "_partial_rgba.npy"), "r+b") as fh:
            fh.seek(200)
            fh.write(b"\xff" * 64)
        proc = self.render(self.scene_path, out)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        status = json.loads(proc.stdout)
        self.assertIsNotNone(status["discarded_snapshot_reason"])
        with open(os.path.join(clean, "rgba.png"), "rb") as fh:
            expected = fh.read()
        with open(os.path.join(out, "rgba.png"), "rb") as fh:
            actual = fh.read()
        self.assertEqual(expected, actual)

    def test_corrupt_snapshot_strict_exit_code(self):
        out = self.outdir("strict")
        self._make_partial_snapshot(out)
        with open(os.path.join(out, "_snapshot.json"), "w") as fh:
            fh.write("{corrupted")
        proc = self.render(self.scene_path, out, "--strict-snapshot")
        self.assertEqual(proc.returncode, 5, proc.stderr)
        payload = json.loads(proc.stderr)
        self.assertEqual(payload["error"]["code"], "SNAPSHOT_CORRUPT")

    def test_scene_digest_mismatch_discards_snapshot(self):
        out = self.outdir("stale")
        self._make_partial_snapshot(out)
        stale_scene = json.loads(json.dumps(self.scene))
        stale_scene["layers"][0]["transmittance"] = 0.9
        stale_path = self.write_scene(stale_scene, "stale_scene.json")
        proc = self.render(stale_path, out)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        status = json.loads(proc.stdout)
        self.assertEqual(status["resumed_from_tiles"], [])
        self.assertEqual(status["discarded_snapshot_reason"],
                         "scene digest mismatch")
        fresh = self.outdir("fresh")
        self.assertEqual(self.render(stale_path, fresh).returncode, 0)
        with open(os.path.join(fresh, "rgba.png"), "rb") as fh:
            expected = fh.read()
        with open(os.path.join(out, "rgba.png"), "rb") as fh:
            actual = fh.read()
        self.assertEqual(expected, actual)


class TestVerifyMode(WorkspaceMixin, unittest.TestCase):
    def test_verify_match_and_mismatch(self):
        out = self.outdir("ref")
        self.assertEqual(self.render(self.scene_path, out).returncode, 0)
        ref_rgba = os.path.join(out, "rgba.png")
        ref_owner = os.path.join(out, "owner.png")
        ref_land = os.path.join(out, "landings.json")

        out2 = self.outdir("same")
        proc = self.render(self.scene_path, out2,
                           "--expect-rgba", ref_rgba,
                           "--expect-owner", ref_owner,
                           "--expect-landings", ref_land)
        self.assertEqual(proc.returncode, 0, proc.stderr)

        other_scene = json.loads(json.dumps(self.scene))
        other_scene["layers"][0]["transmittance"] = 0.95
        other_path = self.write_scene(other_scene, "other_scene.json")
        out3 = self.outdir("diff")
        proc = self.render(other_path, out3, "--expect-rgba", ref_rgba)
        self.assertEqual(proc.returncode, 4, proc.stderr)
        payload = json.loads(proc.stderr)
        self.assertEqual(payload["error"]["code"], "VERIFY_MISMATCH")
        pixel = payload["error"]["diff"]["pixels"][0]
        for key in ("tile_id", "x", "y", "channel", "expected", "actual"):
            self.assertIn(key, pixel)


class TestProjectionReport(WorkspaceMixin, unittest.TestCase):
    def test_landings_and_penumbra_trend(self):
        out = self.outdir("proj")
        self.assertEqual(self.render(self.scene_path, out).returncode, 0)
        with open(os.path.join(out, "landings.json"), "r", encoding="utf-8") as fh:
            landings = json.load(fh)
        lamp_ids = {landing["lamp_id"] for landing in landings}
        self.assertEqual(lamp_ids, {0, 1, 2})
        self.assertTrue(all("x" in landing and "y" in landing
                            and "in_range" in landing for landing in landings))
        # Out-of-range landings are recorded but produce no spot.
        self.assertTrue(any(not landing["in_range"] for landing in landings))

        with open(os.path.join(out, "report.json"), "r", encoding="utf-8") as fh:
            report = json.load(fh)
        measurements = report["penumbra_measurements"]
        self.assertTrue(len(measurements) >= 6)
        groups = {}
        for m in measurements:
            key = (m["lamp_id"], m["hole_id"], m["from_layer"])
            groups.setdefault(key, []).append(m)
        checked = 0
        for group in groups.values():
            group.sort(key=lambda m: m["air_gap"] + m["lamp_distance"])
            for a, b in zip(group, group[1:]):
                if b["nominal_penumbra"] > a["nominal_penumbra"] + 0.05:
                    width_a = a["edge_width_left"] + a["edge_width_right"]
                    width_b = b["edge_width_left"] + b["edge_width_right"]
                    self.assertGreaterEqual(
                        width_b + 1e-9, width_a,
                        "penumbra must widen with gap/distance")
                    checked += 1
        self.assertTrue(checked >= 3)
        # Per-layer transmission/reflection ratios present and sane.
        for entry in report["per_layer_contributions"]:
            self.assertGreaterEqual(entry["transmission_ratio"], 0.0)
            self.assertGreaterEqual(entry["reflection_ratio"], 0.0)
            self.assertAlmostEqual(entry["transmission_ratio"]
                                   + entry["reflection_ratio"], 1.0, places=6)


if __name__ == "__main__":
    unittest.main()
