from __future__ import annotations

import copy
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import numpy as np

from aquarium_render.canonical import canonical_bytes
from aquarium_render.errors import SceneValidationError
from aquarium_render.particles import build_particle_table
from aquarium_render.pipeline import run_pipeline
from aquarium_render.png import decode_png_rgba, png_rgba_bytes
from aquarium_render.projection import (
    build_projection_geometry,
    build_shadow_map,
    penumbra_width,
    polygon_coverage,
)
from aquarium_render.scene import build_sample_scene, validate_scene, write_scene
from aquarium_render.verify import compare_run_directories, enforce_reference

ROOT = Path(__file__).resolve().parents[1]


def small_scene():
    raw = build_sample_scene()
    raw["render"].update(
        {
            "width": 48,
            "height": 36,
            "block_width": 16,
            "block_height": 12,
            "shadow_map_resolution": 48,
            "shadow_samples": 4,
        }
    )
    raw["camera"]["sample_count"] = 3
    raw["camera"]["start_deg"] = -18.0
    raw["camera"]["end_deg"] = 18.0
    for layer in raw["layers"]:
        if layer["kind"] == "water":
            layer["particle_resolution"] = [8, 4]
    return raw


class ValidationTests(unittest.TestCase):
    def test_business_errors_include_codes_and_component_ids(self):
        raw = build_sample_scene()
        raw["layers"][0]["mesh"]["triangles"][0][0] = 999999
        raw["layers"][1]["mesh"]["triangles"][1] = [0, 0, 0]
        raw["layers"][2]["thickness"] = 0.0
        raw["layers"][2]["transmission"] = 1.4
        raw["layers"][3]["absorption_coefficient"] = [-0.01, 0.2, 0.3]
        raw["layers"][3]["particle_strength"] = "not-a-number"
        raw["layers"][4]["layer_index"] = "one"
        raw["fish"][0]["fish_id"] = raw["layers"][0]["component_id"]
        raw["lights"][0]["position"] = [0.70, 0.0, 0.85]
        with self.assertRaises(SceneValidationError) as caught:
            validate_scene(raw)
        errors = caught.exception.errors
        codes = {error.code for error in errors}
        self.assertIn("AQ-GEO-001", codes)
        self.assertIn("AQ-GEO-002", codes)
        self.assertIn("AQ-MAT-001", codes)
        self.assertIn("AQ-MAT-002", codes)
        self.assertIn("AQ-WAT-001", codes)
        self.assertIn("AQ-ID-001", codes)
        self.assertIn("AQ-LGT-001", codes)
        self.assertTrue(all(error.component_id for error in errors))

    def test_cli_invalid_scene_exits_two_without_rendering(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "bad.json"
            raw = build_sample_scene()
            raw["layers"][0]["mesh"]["triangles"][0][1] = 100000
            path.write_bytes(canonical_bytes(raw))
            output = Path(temporary) / "out"
            env = dict(os.environ)
            env["PYTHONPATH"] = str(ROOT / "src")
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "aquarium_render",
                    "render",
                    "--scene",
                    str(path),
                    "--out",
                    str(output),
                ],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=30,
            )
            self.assertEqual(completed.returncode, 2)
            payload = json.loads(completed.stderr)
            self.assertEqual(payload["status"], "error")
            self.assertEqual(payload["errors"][0]["code"], "AQ-GEO-001")
            self.assertFalse(output.exists() and any(output.iterdir()))


class ParticleTests(unittest.TestCase):
    def test_tables_are_seed_and_resolution_deterministic(self):
        first = build_particle_table(17041, "water-00", (18, 7))
        second = build_particle_table(17041, "water-00", (18, 7))
        self.assertEqual(first.payload_bytes, second.payload_bytes)
        self.assertEqual(first.sha256, second.sha256)
        different_seed = build_particle_table(17042, "water-00", (18, 7))
        different_resolution = build_particle_table(17041, "water-00", (19, 7))
        self.assertNotEqual(first.sha256, different_seed.sha256)
        self.assertNotEqual(first.sha256, different_resolution.sha256)
        self.assertEqual(first.resolution, (18, 7))

    def test_triangle_order_cannot_change_water_table(self):
        raw_a = build_sample_scene()
        raw_b = copy.deepcopy(raw_a)
        for layer in raw_b["layers"]:
            layer["mesh"]["triangles"].reverse()
            for triangle in layer["mesh"]["triangles"]:
                triangle[1], triangle[2] = triangle[2], triangle[1]
        scene_a = validate_scene(raw_a)
        scene_b = validate_scene(raw_b)
        table_a = build_particle_table(
            scene_a.water[0].particle_seed,
            scene_a.water[0].component_id,
            scene_a.water[0].particle_resolution,
        )
        table_b = build_particle_table(
            scene_b.water[0].particle_seed,
            scene_b.water[0].component_id,
            scene_b.water[0].particle_resolution,
        )
        self.assertEqual(table_a.payload_bytes, table_b.payload_bytes)


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.base = Path(cls.temporary.name)
        cls.raw = small_scene()
        cls.scene_path = cls.base / "scene.json"
        write_scene(cls.raw, cls.scene_path)
        cls.scene = validate_scene(cls.raw)
        cls.reference = cls.base / "reference"
        cls.result = run_pipeline(cls.scene, cls.reference)

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def test_completed_render_has_all_outputs(self):
        self.assertEqual(self.result.status, "complete")
        for angle in range(3):
            self.assertTrue((self.reference / f"angle_{angle:04d}_rgba.png").exists())
            self.assertTrue((self.reference / f"angle_{angle:04d}_ownership.png").exists())
            self.assertTrue((self.reference / f"angle_{angle:04d}_preview.png").exists())
        self.assertTrue((self.reference / "report.json").exists())
        snapshot = json.loads((self.reference / "progress.snapshot.json").read_text())
        self.assertTrue(snapshot["complete"])
        self.assertEqual(snapshot["scene_summary"]["renderer_revision"], "1.0.0-r4")
        self.assertEqual(len(snapshot["completed"]), 27)
        self.assertTrue(all(block["color_summary"]["sha256"] for block in snapshot["completed"]))

    def test_two_runs_are_byte_identical(self):
        second = self.base / "second"
        result = run_pipeline(self.scene, second)
        self.assertEqual(result.status, "complete")
        self.assertEqual(compare_run_directories(self.reference, second, self.scene), [])
        for angle in range(3):
            for suffix in ("rgba.png", "ownership.png", "preview.png"):
                first = (self.reference / f"angle_{angle:04d}_{suffix}").read_bytes()
                other = (second / f"angle_{angle:04d}_{suffix}").read_bytes()
                self.assertEqual(first, other)
        self.assertEqual(
            json.loads((self.reference / "report.json").read_text())["projections"],
            json.loads((second / "report.json").read_text())["projections"],
        )

    def test_rgba_is_premultiplied_and_not_clipped_white(self):
        for angle in range(3):
            image = decode_png_rgba(
                (self.reference / f"angle_{angle:04d}_rgba.png").read_bytes()
            )
            self.assertTrue(np.all(image[:, :, :3] <= image[:, :, 3:4]))
            self.assertLess(int(image[:, :, :3].max()), 250)
            white = np.all(image[:, :, :3] >= 245, axis=2)
            self.assertLess(float(np.mean(white)), 0.01)

    def test_layer_contribution_report_distinguishes_water(self):
        report = json.loads((self.reference / "report.json").read_text())
        water = [layer for layer in report["layers"] if layer["kind"] == "water"]
        self.assertEqual(len(water), 2)
        for layer in water:
            self.assertGreater(layer["transmission_contribution"], 0.0)
            self.assertGreater(layer["scattering_contribution"], 0.0)
            self.assertAlmostEqual(
                layer["transmission_ratio"] + layer["scattering_ratio"], 1.0, places=6
            )
        self.assertNotEqual(water[0]["scattering_ratio"], water[1]["scattering_ratio"])

    def test_projection_trends_and_water_layer(self):
        geometry = build_projection_geometry(self.scene)
        by_key = {
            (entry["light_id"], fish["fish_id"]): fish
            for entry in geometry["lights"]
            for fish in entry["fish"]
        }
        fish_ids = {"fish-01", "fish-02"}
        for light_id in ("key-warm", "fill-cool"):
            for fish_id in fish_ids:
                fish = by_key[(light_id, fish_id)]
                self.assertEqual(fish["receiver"]["status"], "valid")
                self.assertEqual(fish["water"]["status"], "valid")
        key_01 = np.asarray(by_key[("key-warm", "fish-01")]["receiver"]["centroid"])
        fill_01 = np.asarray(by_key[("fill-cool", "fish-01")]["receiver"]["centroid"])
        self.assertGreater(float(np.linalg.norm(key_01 - fill_01)), 0.15)

        light = self.scene.lights[0]
        target = np.asarray([0.0, 0.0, 0.185])
        widths = []
        for fish_point in (
            np.asarray([-0.8, 0.0, 0.4]),
            np.asarray([-1.4, 0.0, 0.4]),
            np.asarray([-2.0, 0.0, 0.4]),
        ):
            widths.append(penumbra_width(self.scene, light, fish_point, target))
        self.assertTrue(all(widths[i] < widths[i + 1] for i in range(len(widths) - 1)))

        for sample in by_key[("key-warm", "fish-01")]["profile_samples"]:
            self.assertGreaterEqual(sample["brightness"], 0.0)
            self.assertLessEqual(sample["brightness"], 1.0)
        brightness = [sample["brightness"] for sample in by_key[("key-warm", "fish-01")]["profile_samples"]]
        self.assertGreater(max(brightness) - min(brightness), 0.2)

    def test_projection_moves_with_fish_position(self):
        raw = copy.deepcopy(self.raw)
        for vertex in raw["fish"][0]["mesh"]["vertices"]:
            vertex[2] += 0.08
        moved_scene = validate_scene(raw)
        original = build_projection_geometry(self.scene)
        moved = build_projection_geometry(moved_scene)

        def centroid(geometry):
            for light in geometry["lights"]:
                if light["light_id"] == "key-warm":
                    for fish in light["fish"]:
                        if fish["fish_id"] == "fish-01":
                            return np.asarray(fish["receiver"]["centroid"])
            raise AssertionError("projection missing")

        self.assertGreater(float(np.linalg.norm(centroid(original) - centroid(moved))), 0.03)

    def test_shadow_map_is_byte_deterministic(self):
        geometry = build_projection_geometry(self.scene)
        light = self.scene.lights[0]
        first = build_shadow_map(self.scene, light, geometry)
        second = build_shadow_map(self.scene, light, geometry)
        self.assertEqual(first.tobytes(), second.tobytes())
        self.assertGreater(int(first.max()), 0)
        self.assertLess(int(first.min()), 255)
        self.assertGreater(int(np.unique(first).size), 8)

    def test_out_of_range_projection_contributes_no_occlusion(self):
        geometry = build_projection_geometry(self.scene)
        outside = [
            polygon
            for polygon in geometry["receiver_polygons"]
            if not polygon.valid
        ]
        self.assertTrue(outside)
        query = self.scene.receiver.outer_radius * 4.0
        coverage = polygon_coverage(
            outside[0],
            np.asarray([[query, query]]),
            self.scene.render.shadow_strength,
        )
        self.assertEqual(float(coverage[0]), 0.0)

    def test_resume_after_sigterm_matches_reference(self):
        out = self.base / "sigterm"
        env = dict(os.environ)
        env["PYTHONPATH"] = str(ROOT / "src")
        env["AQUARIUM_BLOCK_DELAY_MS"] = "35"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "aquarium_render",
                "render",
                "--scene",
                str(self.scene_path),
                "--out",
                str(out),
            ],
            cwd=ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        self._wait_for_blocks(out / "progress.snapshot.json", 4)
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=20)
        self.assertEqual(process.returncode, -signal.SIGTERM)
        snap = json.loads((out / "progress.snapshot.json").read_text())
        self.assertFalse(snap["complete"])
        result = run_pipeline(self.scene, out)
        self.assertEqual(result.status, "complete")
        enforce_reference(self.reference, out, self.scene)

    def test_sigint_commits_block_and_exits_zero(self):
        out = self.base / "sigint"
        env = dict(os.environ)
        env["PYTHONPATH"] = str(ROOT / "src")
        env["AQUARIUM_BLOCK_DELAY_MS"] = "90"
        process = subprocess.Popen(
            [
                sys.executable,
                "-m",
                "aquarium_render",
                "render",
                "--scene",
                str(self.scene_path),
                "--out",
                str(out),
            ],
            cwd=ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        self._wait_for_blocks(out / "progress.snapshot.json", 3)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=20)
        self.assertEqual(process.returncode, 0, stderr)
        recovery = json.loads((out / "recovery_report.json").read_text())
        self.assertEqual(recovery["status"], "interrupted")
        self.assertEqual(recovery["exit_code"], 0)
        snap = json.loads((out / "progress.snapshot.json").read_text())
        self.assertFalse(snap["complete"])
        result = run_pipeline(self.scene, out)
        self.assertEqual(result.status, "complete")
        enforce_reference(self.reference, out, self.scene)

    def test_seed_change_discards_snapshot_and_restarts(self):
        out = self.base / "seed-reset"
        partial_scene = validate_scene(self.raw)
        calls = {"value": 0}

        def interrupt_after_three_blocks() -> bool:
            calls["value"] += 1
            return calls["value"] >= 8

        partial = run_pipeline(
            partial_scene,
            out,
            interrupt_requested=interrupt_after_three_blocks,
        )
        self.assertEqual(partial.status, "interrupted")
        self.assertGreater(len(partial.snapshot.completed), 0)

        raw = copy.deepcopy(self.raw)
        raw["layers"][3]["particle_seed"] = 123456
        changed = validate_scene(raw)
        reset = run_pipeline(changed, out)
        self.assertEqual(reset.status, "complete")
        self.assertIsNotNone(reset.snapshot.reset_reason)
        fresh = self.base / "seed-reset-fresh"
        run_pipeline(changed, fresh)
        enforce_reference(fresh, out, changed)

    def test_corrupt_snapshot_restarts(self):
        out = self.base / "corrupt-reset"
        (out / "particle_tables").mkdir(parents=True)
        (out / "progress.snapshot.json").write_text('{"broken":', encoding="utf-8")
        result = run_pipeline(self.scene, out)
        self.assertEqual(result.status, "complete")
        self.assertIsNotNone(result.snapshot.reset_reason)
        enforce_reference(self.reference, out, self.scene)

    def test_structured_diff_reports_angle_block_pixel_channel(self):
        tampered = self.base / "tampered"
        shutil.copytree(self.reference, tampered)
        filename = "angle_0001_rgba.png"
        path = tampered / filename
        image = decode_png_rgba(path.read_bytes())
        image[6, 7, 1] = int(image[6, 7, 1]) ^ 0x5A
        path.write_bytes(png_rgba_bytes(image))
        diffs = compare_run_directories(self.reference, tampered, self.scene)
        self.assertTrue(diffs)
        image_diff = next(diff for diff in diffs if diff["scope"] == "rgba")
        self.assertEqual(image_diff["angle_index"], 1)
        self.assertEqual(image_diff["pixel"], [7, 6])
        self.assertIsNotNone(image_diff["block_id"])
        self.assertIn(image_diff["channel"], {"R", "G", "B", "A"})
        diff_file = tampered / "reference_diff.json"
        env = dict(os.environ)
        env["PYTHONPATH"] = str(ROOT / "src")
        completed = subprocess.run(
            [
                sys.executable,
                "-m",
                "aquarium_render",
                "compare",
                "--scene",
                str(self.scene_path),
                "--reference",
                str(self.reference),
                "--actual",
                str(tampered),
                "--output",
                str(diff_file),
            ],
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
            timeout=30,
        )
        self.assertEqual(completed.returncode, 1)
        payload = json.loads(completed.stderr)
        self.assertEqual(payload["code"], "AQ-RESUME-001")
        self.assertTrue(diff_file.exists())

    @staticmethod
    def _wait_for_blocks(path: Path, count: int, timeout: float = 20.0) -> None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            if path.exists():
                try:
                    document = json.loads(path.read_text())
                    if len(document.get("completed", [])) >= count:
                        return
                except (OSError, json.JSONDecodeError):
                    pass
            time.sleep(0.02)
        raise AssertionError(f"snapshot did not commit {count} blocks in time")


if __name__ == "__main__":
    unittest.main()

