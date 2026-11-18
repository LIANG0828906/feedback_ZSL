from __future__ import annotations

import base64
import copy
import hashlib
import json
import os
import signal
import struct
import subprocess
import sys
import tempfile
import time
import unittest
import zlib
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from dome_render.projection import build_projection_report, build_shadow_maps, penumbra_trend_report
from dome_render.sample_scene import build_sample_scene
from dome_render.scene import load_scene
from dome_render.snapshot import _checksum, _encode_array, block_color_summary
from dome_render.stars import generate_star_field


def _read_png_rgba(path: Path) -> np.ndarray:
    data = path.read_bytes()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("not PNG")
    position = 8
    width = height = bit_depth = color_type = None
    idat = bytearray()
    while position < len(data):
        length = struct.unpack(">I", data[position : position + 4])[0]
        kind = data[position + 4 : position + 8]
        payload = data[position + 8 : position + 8 + length]
        position += 12 + length
        if kind == b"IHDR":
            width, height, bit_depth, color_type, _, _, _ = struct.unpack(">IIBBBBB", payload)
        elif kind == b"IDAT":
            idat.extend(payload)
        elif kind == b"IEND":
            break
    if color_type != 6 or bit_depth not in (8, 16):
        raise ValueError("unexpected PNG format")
    raw = zlib.decompress(bytes(idat))
    channels = 4
    bytes_per_channel = 2 if bit_depth == 16 else 1
    stride = width * channels * bytes_per_channel
    rows = []
    for y in range(height):
        start = y * (stride + 1)
        if raw[start] != 0:
            raise ValueError("test reader only supports filter 0")
        row = raw[start + 1 : start + 1 + stride]
        dtype = ">u2" if bit_depth == 16 else "u1"
        rows.append(np.frombuffer(row, dtype=dtype).reshape((1, width, channels)))
    return np.concatenate(rows, axis=0)


class DomeRenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp = tempfile.TemporaryDirectory(prefix="dome-render-tests-")
        cls.temp_path = Path(cls.temp.name)
        cls.scene = build_sample_scene(
            width=80,
            height=48,
            block_size=(40, 24),
            supersample=1,
            shadow_samples=8,
            projection_density=1,
        )
        cls.scene_path = cls.temp_path / "base.json"
        cls.scene_path.write_text(json.dumps(cls.scene, indent=2), encoding="utf-8")
        cls.base_out = cls.temp_path / "base-out"
        cls.base_result = cls.run_cli("render", "--scene", str(cls.scene_path), "--out", str(cls.base_out), "--quiet")
        if cls.base_result.returncode != 0:
            raise RuntimeError(cls.base_result.stdout + cls.base_result.stderr)
        cls.base_rgba = (cls.base_out / "rgba.png").read_bytes()
        cls.base_layer = (cls.base_out / "layer_map.png").read_bytes()
        cls.base_projection = (cls.base_out / "projections.json").read_bytes()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp.cleanup()

    @classmethod
    def run_cli(cls, *args: str, env: dict[str, str] | None = None, timeout: float = 90.0) -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment.pop("DOME_TEST_BLOCK_DELAY_MS", None)
        if env:
            environment.update(env)
        return subprocess.run(
            [sys.executable, "-m", "dome_render", *args],
            cwd=ROOT,
            env=environment,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def _write_scene(self, scene: dict, name: str) -> Path:
        path = self.temp_path / name
        path.write_text(json.dumps(scene, indent=2, sort_keys=True), encoding="utf-8")
        return path

    def test_validation_errors_are_structured(self) -> None:
        cases: list[tuple[str, object, str]] = []

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["ribs"][0]["mesh"]["triangles"][0][0] = 99999
        cases.append(("mesh_index", scene, "E_MESH_INDEX_OUT_OF_RANGE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["ribs"][0]["mesh"]["triangles"][0] = [0, 0, 0]
        cases.append(("degenerate", scene, "E_TRIANGLE_DEGENERATE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["ribs"][0]["material"]["reflectance"] = 1.5
        cases.append(("reflectance", scene, "E_NUMBER_RANGE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["star_boards"][0]["material"]["transmission"] = -0.01
        cases.append(("transmission", scene, "E_NUMBER_RANGE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][1]["index"] = 0
        cases.append(("duplicate_layer", scene, "E_LAYER_INDEX_DUPLICATE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][2]["index"] = 4
        cases.append(("missing_layer", scene, "E_LAYER_INDEX_MISSING"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["ribs"][1]["id"] = 0
        cases.append(("duplicate_rib", scene, "E_RIB_ID_DUPLICATE"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["layers"][0]["ribs"][2]["id"] = 5
        cases.append(("missing_rib", scene, "E_RIB_ID_MISSING"))

        scene = copy.deepcopy(self.scene)
        scene["lights"][0]["direction"] = [0.0, 0.0, 1.2]
        cases.append(("non_unit_light", scene, "E_DIRECTION_NOT_UNIT"))

        scene = copy.deepcopy(self.scene)
        scene["lights"][0]["direction"] = [1.0, 0.0, 0.0]
        cases.append(("parallel_light", scene, "E_LIGHT_PARALLEL_LAYER"))

        scene = copy.deepcopy(self.scene)
        scene["camera"]["target"] = scene["camera"]["position"]
        cases.append(("bad_camera", scene, "E_CAMERA_LOOK"))

        scene = copy.deepcopy(self.scene)
        rib_vertices = np.asarray(scene["dome"]["layers"][0]["ribs"][0]["mesh"]["vertices"])[:4]
        scene["camera"]["position"] = np.mean(rib_vertices, axis=0).tolist()
        scene["camera"]["target"] = [0.0, 0.0, 5.0]
        cases.append(("camera_inside", scene, "E_CAMERA_INSIDE_GEOMETRY"))

        scene = copy.deepcopy(self.scene)
        scene["dome"]["inner_wall"]["plane"]["normal"] = [0.0, 0.0, 0.0]
        cases.append(("wall_plane", scene, "E_WALL_PLANE"))

        for name, scene_value, expected_code in cases:
            with self.subTest(name=name):
                path = self._write_scene(scene_value, f"invalid-{name}.json")
                result = self.run_cli("validate", "--scene", str(path))
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                payload = json.loads(result.stdout)
                self.assertFalse(payload["ok"])
                codes = {error["code"] for error in payload["errors"]}
                self.assertIn(expected_code, codes)
                for error in payload["errors"]:
                    self.assertIn("location", error)
                    self.assertIn("field", error)

    def test_star_table_is_seeded_and_recomputable(self) -> None:
        first = generate_star_field(123456789, 64, 48)
        second = generate_star_field(123456789, 64, 48)
        other = generate_star_field(123456790, 64, 48)
        self.assertEqual(first.texture_digest, second.texture_digest)
        self.assertTrue(np.array_equal(first.texture, second.texture))
        self.assertTrue(np.array_equal(first.positions, second.positions))
        self.assertTrue(np.array_equal(first.brightness, second.brightness))
        self.assertNotEqual(first.texture_digest, other.texture_digest)
        self.assertEqual(first.count, generate_star_field(123456789, 64, 48).count)

    def test_fresh_runs_and_stable_report(self) -> None:
        out1 = self.temp_path / "determinism-a"
        out2 = self.temp_path / "determinism-b"
        result1 = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out1), "--quiet")
        result2 = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out2), "--quiet")
        self.assertEqual(result1.returncode, 0, result1.stderr)
        self.assertEqual(result2.returncode, 0, result2.stderr)
        for name in ("rgba.png", "layer_map.png", "projections.json", "report.json"):
            self.assertEqual((out1 / name).read_bytes(), (out2 / name).read_bytes(), name)
        report_bytes = (out1 / "report.json").read_bytes()
        repeat = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out1), "--quiet")
        self.assertEqual(repeat.returncode, 0, repeat.stderr)
        self.assertEqual(report_bytes, (out1 / "report.json").read_bytes())

        report = json.loads(report_bytes)
        self.assertEqual(
            list(report.keys()),
            ["schema_version", "status", "scene", "render", "layers", "wall", "projection", "blocks", "recovery", "image_digest", "layer_map_digest"],
        )
        self.assertNotIn(str(ROOT), report_bytes.decode("utf-8"))
        self.assertNotIn("timestamp", report_bytes.decode("utf-8").lower())
        self.assertEqual(report["blocks"]["completed_count"], report["blocks"]["total_count"])
        self.assertEqual(report["status"], "complete")
        self.assertGreater(report["projection"]["sample_count"], 0)
        self.assertGreater(report["projection"]["accepted_count"], 0)
        for layer in report["layers"]:
            self.assertGreater(layer["visible_fragments"], 0)
            self.assertTrue(0.0 <= layer["average_transmission"] <= 1.0)
            self.assertGreater(layer["stars"][0]["star_count"], 0)
            self.assertAlmostEqual(layer["transmission_ratio"] + layer["reflection_ratio"], 1.0, places=7)

        rgba = _read_png_rgba(out1 / "rgba.png")
        layer_map = _read_png_rgba(out1 / "layer_map.png")
        self.assertEqual(rgba.shape, (48, 80, 4))
        self.assertEqual(layer_map.shape, (48, 80, 4))
        self.assertGreater(int(np.max(rgba[..., 3])), 0)
        self.assertGreater(len(np.unique(layer_map[..., 0])), 1)

    def test_render_independent_of_layer_and_triangle_order(self) -> None:
        reordered = copy.deepcopy(self.scene)
        reordered["dome"]["layers"].reverse()
        for layer in reordered["dome"]["layers"]:
            for rib in layer["ribs"]:
                rib["mesh"]["triangles"].reverse()
            for board in layer["star_boards"]:
                board["mesh"]["triangles"].reverse()
        path = self._write_scene(reordered, "reordered.json")
        out = self.temp_path / "reordered-out"
        result = self.run_cli("render", "--scene", str(path), "--out", str(out), "--quiet")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.base_rgba, (out / "rgba.png").read_bytes())
        self.assertEqual(self.base_layer, (out / "layer_map.png").read_bytes())
        self.assertEqual(self.base_projection, (out / "projections.json").read_bytes())

    def test_projection_shift_and_penumbra_trends(self) -> None:
        base = load_scene(self.scene_path)
        base_shadows = build_shadow_maps(base)
        base_report = build_projection_report(base, base_shadows)
        key = ("star_board", 1, 0, 0)
        base_width = base_shadows[key].penumbra_width

        gap_scene_path = self._write_scene(
            build_sample_scene(width=48, height=32, block_size=(24, 16), supersample=1, shadow_samples=8, projection_density=1, layer_gap=0.9),
            "gap.json",
        )
        source_scene_path = self._write_scene(
            build_sample_scene(
                width=48,
                height=32,
                block_size=(24, 16),
                supersample=1,
                shadow_samples=8,
                projection_density=1,
                light_source_distance=7.5,
            ),
            "source.json",
        )
        gap_scene = load_scene(gap_scene_path)
        source_scene = load_scene(source_scene_path)
        gap_width = build_shadow_maps(gap_scene)[key].penumbra_width
        source_width = build_shadow_maps(source_scene)[key].penumbra_width
        self.assertGreater(gap_width, base_width)
        self.assertGreater(source_width, base_width)

        trend = penumbra_trend_report(base)["comparisons"]
        by_name = {row["scenario"]: row for row in trend}
        self.assertGreater(by_name["gap_x2"]["penumbra_width_world"], by_name["base"]["penumbra_width_world"])
        self.assertGreater(by_name["source_plus_2"]["penumbra_width_world"], by_name["base"]["penumbra_width_world"])
        self.assertGreater(by_name["gap_x2"]["edge_transition_10_90_px"], by_name["base"]["edge_transition_10_90_px"])
        self.assertGreater(by_name["source_plus_2"]["edge_transition_10_90_px"], by_name["base"]["edge_transition_10_90_px"])
        self.assertTrue(all("samples" in row and len(row["samples"]) >= 5 for row in trend))

        changed_scene_data = build_sample_scene(width=48, height=32, block_size=(24, 16), supersample=1, shadow_samples=8, projection_density=1)
        changed_scene_data["lights"][0]["direction"] = self._normalized([0.72, 0.08, 0.69])
        changed_path = self._write_scene(changed_scene_data, "direction.json")
        changed = load_scene(changed_path)
        changed_report = build_projection_report(changed, build_shadow_maps(changed))

        def find_entry(report: dict) -> dict:
            for light in report["lights"]:
                if light["light_id"] != 0:
                    continue
                for entry in light["entries"]:
                    if (
                        entry["source"]["layer"] == 0
                        and entry["source"]["rib_id"] == 0
                        and entry["receiver"]["kind"] == "star_board"
                        and entry["receiver"]["layer"] == 1
                    ):
                        return entry
            raise AssertionError("projection entry not found")

        base_entry = find_entry(base_report)
        changed_entry = find_entry(changed_report)
        self.assertGreater(float(np.linalg.norm(np.asarray(base_entry["world"]) - np.asarray(changed_entry["world"]))), 1.0e-5)
        repeat_report = build_projection_report(base, build_shadow_maps(base))
        self.assertEqual(json.dumps(base_report, sort_keys=True), json.dumps(repeat_report, sort_keys=True))
        self.assertTrue(all(shadow.digest for shadow in base_shadows.values()))
        self.assertTrue(all(np.min(shadow.coverage) >= 0.0 and np.max(shadow.coverage) <= 1.0 for shadow in base_shadows.values()))

        outside_scene_data = build_sample_scene(width=48, height=32, block_size=(24, 16), supersample=1, shadow_samples=8, projection_density=1)
        outside_scene_data["lights"][0]["direction"] = self._normalized([0.95, 0.10, 0.25])
        outside_path = self._write_scene(outside_scene_data, "outside.json")
        outside_scene = load_scene(outside_path)
        outside_report = build_projection_report(outside_scene, build_shadow_maps(outside_scene))
        light_zero = next(light for light in outside_report["lights"] if light["light_id"] == 0)
        self.assertGreater(light_zero["outside_count"], 0)
        rejected = next(entry for entry in light_zero["entries"] if not entry["accepted"])
        self.assertIsNone(rejected["shadow_gray"])

    @staticmethod
    def _normalized(values: list[float]) -> list[float]:
        vector = np.asarray(values, dtype=np.float64)
        return [float(value) for value in vector / np.linalg.norm(vector)]

    def _wait_for_snapshot(self, out: Path, process: subprocess.Popen[str], minimum: int = 1, timeout: float = 10.0) -> int:
        deadline = time.time() + timeout
        last = 0
        while time.time() < deadline:
            if process.poll() is not None:
                break
            snapshot = out / "snapshot.json"
            if snapshot.exists():
                try:
                    last = int(json.loads(snapshot.read_text(encoding="utf-8")).get("completed_count", 0))
                except (json.JSONDecodeError, OSError):
                    last = 0
                if last >= minimum:
                    return last
            time.sleep(0.01)
        raise AssertionError(f"snapshot did not reach {minimum} blocks; last={last}")

    def test_forced_kill_resume_is_byte_identical(self) -> None:
        out = self.temp_path / "kill-resume"
        environment = os.environ.copy()
        environment["DOME_TEST_BLOCK_DELAY_MS"] = "120"
        process = subprocess.Popen(
            [sys.executable, "-m", "dome_render", "render", "--scene", str(self.scene_path), "--out", str(out), "--quiet"],
            cwd=ROOT,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        try:
            completed = self._wait_for_snapshot(out, process, minimum=1)
            process.kill()
            process.communicate(timeout=10)
        finally:
            if process.poll() is None:
                process.kill()
        self.assertGreaterEqual(completed, 1)
        resumed = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out), "--quiet")
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        self.assertEqual(self.base_rgba, (out / "rgba.png").read_bytes())
        self.assertEqual(self.base_layer, (out / "layer_map.png").read_bytes())
        self.assertEqual(self.base_projection, (out / "projections.json").read_bytes())
        report = json.loads((out / "report.json").read_text(encoding="utf-8"))
        self.assertEqual(report["recovery"]["recovery_count"], 1)
        self.assertTrue(report["recovery"]["snapshot_used"])
        self.assertGreaterEqual(len(report["recovery"]["resumed_block_ids"]), 1)

    @unittest.skipUnless(os.name == "posix", "SIGINT delivery test is POSIX-specific")
    def test_sigint_finishes_block_and_exits_zero(self) -> None:
        out = self.temp_path / "sigint"
        environment = os.environ.copy()
        environment["DOME_TEST_BLOCK_DELAY_MS"] = "120"
        process = subprocess.Popen(
            [sys.executable, "-m", "dome_render", "render", "--scene", str(self.scene_path), "--out", str(out), "--quiet"],
            cwd=ROOT,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        completed = self._wait_for_snapshot(out, process, minimum=1)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=15)
        self.assertEqual(process.returncode, 0, stdout + stderr)
        recovery = json.loads((out / "recovery.json").read_text(encoding="utf-8"))
        snapshot = json.loads((out / "snapshot.json").read_text(encoding="utf-8"))
        self.assertEqual(recovery["status"], "interrupted")
        self.assertGreaterEqual(recovery["completed_block_count"], completed)
        self.assertEqual(snapshot["completed_count"], recovery["completed_block_count"])
        resumed = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out), "--quiet")
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        self.assertEqual(self.base_rgba, (out / "rgba.png").read_bytes())
        self.assertEqual(self.base_layer, (out / "layer_map.png").read_bytes())
        self.assertEqual(self.base_projection, (out / "projections.json").read_bytes())

    def test_corrupt_and_mismatched_snapshots_are_discarded(self) -> None:
        out = self.temp_path / "corrupt"
        first = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out), "--quiet")
        self.assertEqual(first.returncode, 0, first.stderr)
        (out / "snapshot.json").write_text("{not valid json", encoding="utf-8")
        corrupt = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out), "--quiet")
        self.assertEqual(corrupt.returncode, 0, corrupt.stdout + corrupt.stderr)
        report = json.loads((out / "report.json").read_text(encoding="utf-8"))
        self.assertTrue(report["recovery"]["snapshot_discarded"])
        self.assertEqual(report["recovery"]["recovery_count"], 0)
        self.assertEqual(self.base_rgba, (out / "rgba.png").read_bytes())

        changed = copy.deepcopy(self.scene)
        changed["renderer"]["exposure"] = float(changed["renderer"]["exposure"]) + 0.17
        changed_path = self._write_scene(changed, "changed-scene.json")
        fresh_changed = self.temp_path / "changed-fresh"
        changed_result = self.run_cli("render", "--scene", str(changed_path), "--out", str(fresh_changed), "--quiet")
        self.assertEqual(changed_result.returncode, 0, changed_result.stderr)
        mismatched = self.run_cli("render", "--scene", str(changed_path), "--out", str(out), "--quiet")
        self.assertEqual(mismatched.returncode, 0, mismatched.stdout + mismatched.stderr)
        report = json.loads((out / "report.json").read_text(encoding="utf-8"))
        self.assertTrue(report["recovery"]["snapshot_discarded"])
        self.assertEqual((fresh_changed / "rgba.png").read_bytes(), (out / "rgba.png").read_bytes())
        self.assertEqual((fresh_changed / "projections.json").read_bytes(), (out / "projections.json").read_bytes())

    def test_tampered_valid_snapshot_reports_structured_difference(self) -> None:
        out = self.temp_path / "tamper"
        environment = os.environ.copy()
        environment["DOME_TEST_BLOCK_DELAY_MS"] = "120"
        process = subprocess.Popen(
            [sys.executable, "-m", "dome_render", "render", "--scene", str(self.scene_path), "--out", str(out), "--quiet"],
            cwd=ROOT,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        completed = self._wait_for_snapshot(out, process, minimum=1)
        process.kill()
        process.communicate(timeout=15)
        self.assertGreaterEqual(completed, 1)

        snapshot = json.loads((out / "snapshot.json").read_text(encoding="utf-8"))
        entry = snapshot["completed_blocks"][0]
        raw = zlib.decompress(base64.b64decode(entry["rgba_zlib_b64"]))
        rgba = np.frombuffer(raw, dtype="<u2").reshape(entry["height"], entry["width"], 4).copy()
        rgba[0, 0, 0] = np.uint16(int(rgba[0, 0, 0]) ^ 1)
        entry["rgba_zlib_b64"] = _encode_array(rgba, "<u2")
        entry["rgba_sha256"] = hashlib.sha256(rgba.astype("<u2", copy=False).tobytes()).hexdigest()
        layer = np.frombuffer(zlib.decompress(base64.b64decode(entry["layer_zlib_b64"])), dtype="u1").reshape(entry["height"], entry["width"], 4)
        entry["color_summary"] = block_color_summary(rgba, layer)
        snapshot["snapshot_sha256"] = _checksum(snapshot)
        (out / "snapshot.json").write_text(json.dumps(snapshot, sort_keys=True, separators=(",", ":")), encoding="utf-8")

        result = self.run_cli("render", "--scene", str(self.scene_path), "--out", str(out), "--quiet")
        self.assertEqual(result.returncode, 5, result.stdout + result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["error"]["code"], "E_RESUME_MISMATCH")
        differences = json.loads((out / "differences.json").read_text(encoding="utf-8"))
        self.assertEqual(differences["error"]["code"], "E_RESUME_MISMATCH")
        first_difference = differences["error"]["differences"][0]
        self.assertIn("block_id", first_difference)
        self.assertEqual(len(first_difference["pixel"]), 2)
        self.assertIn("channel", first_difference)
        self.assertIn("stored", first_difference)
        self.assertIn("expected", first_difference)


if __name__ == "__main__":
    unittest.main()
