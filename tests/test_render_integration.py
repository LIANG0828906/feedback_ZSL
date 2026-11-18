from __future__ import annotations

import hashlib
import json
import copy
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np
from PIL import Image

from veilrender.sample_scene import make_sample_scene, write_scene
from veilrender.geometry import build_layer_geometries
from veilrender.projection import build_projection_report
from veilrender.texture import generate_weave_texture, texture_digest

ROOT = Path(__file__).resolve().parents[1]


def _run(*args: str, timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "veilrender", *args],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class RenderIntegrationTests(unittest.TestCase):
    def test_projection_moves_with_layer_and_light_geometry(self) -> None:
        scene = make_sample_scene(
            width=32,
            height=20,
            tile_width=16,
            tile_height=10,
            camera_samples=1,
        )
        baseline = build_projection_report(
            scene, build_layer_geometries(scene), "baseline"
        )
        baseline_item = next(
            item
            for item in baseline["landings"]
            if item["id"] == "L-key:2:ground:000"
        )

        moved_layer = copy.deepcopy(scene)
        for vertex in moved_layer["layers"][1]["mesh"]["vertices"]:
            vertex[2] += 0.18
        moved_by_layer = build_projection_report(
            moved_layer, build_layer_geometries(moved_layer), "layer-moved"
        )
        layer_item = next(
            item
            for item in moved_by_layer["landings"]
            if item["id"] == "L-key:2:ground:000"
        )
        self.assertNotEqual(baseline_item["landing"], layer_item["landing"])

        moved_light = copy.deepcopy(scene)
        moved_light["lights"][0]["position"][0] += 0.4
        moved_by_light = build_projection_report(
            moved_light, build_layer_geometries(moved_light), "light-moved"
        )
        light_item = next(
            item
            for item in moved_by_light["landings"]
            if item["id"] == "L-key:2:ground:000"
        )
        self.assertNotEqual(baseline_item["landing"], light_item["landing"])

    def test_render_artifacts_and_repeatability(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "scene.json"
            write_scene(
                make_sample_scene(
                    width=96,
                    height=64,
                    tile_width=32,
                    tile_height=32,
                    camera_samples=2,
                ),
                scene_path,
            )
            first = root / "first"
            second = root / "second"
            first_run = _run(
                "render",
                "--scene",
                str(scene_path),
                "--output-dir",
                str(first),
                "--no-resume",
                "--quiet",
            )
            second_run = _run(
                "render",
                "--scene",
                str(scene_path),
                "--output-dir",
                str(second),
                "--no-resume",
                "--quiet",
            )
            self.assertEqual(first_run.returncode, 0, first_run.stderr)
            self.assertEqual(second_run.returncode, 0, second_run.stderr)
            for name in ("rgba.png", "layer_ownership.png", "projection_landings.json", "report.json"):
                self.assertEqual(_sha(first / name), _sha(second / name), name)

            rgba = np.asarray(Image.open(first / "rgba.png").convert("RGBA"), dtype=np.uint8)
            self.assertTrue(np.all(rgba[..., :3] <= rgba[..., 3:4]))
            self.assertLess(int(rgba[..., :3].max()), 255)
            report = json.loads((first / "report.json").read_text(encoding="utf-8"))
            projection = json.loads(
                (first / "projection_landings.json").read_text(encoding="utf-8")
            )
            self.assertEqual(report["snapshot"]["commit_count"], 6)
            self.assertGreater(report["lattice_projection_sample_count"], 0)
            self.assertTrue(report["projection"]["monotonic_with_receiver_distance"])
            self.assertTrue(report["projection"]["monotonic_with_source_distance"])
            self.assertGreater(len(report["layers"]), 0)
            self.assertEqual(
                len(report["layers"]),
                len(projection["lattice_layers"]) + 4,
            )
            self.assertGreater(len(projection["landings"]), 0)
            for layer in report["layers"]:
                self.assertAlmostEqual(
                    layer["transmission_ratio"] + layer["reflection_ratio"],
                    1.0,
                    places=7,
                )
            for landing in projection["landings"]:
                if not landing["inside_receiver"]:
                    self.assertFalse(landing["occluded"])
            for texture in report["textures"]:
                expected = generate_weave_texture(
                    texture["seed"], texture["resolution"]
                )
                self.assertEqual(texture["sha256"], texture_digest(expected))

            owner = np.asarray(Image.open(first / "layer_ownership.png").convert("RGBA"))
            self.assertGreater(int(owner[..., 3].max()), 0)
            self.assertGreater(int(owner[..., 2].max()), 0)
            dominant = (owner[..., 0].astype(np.uint16) << 8) | owner[..., 1]
            self.assertLessEqual(
                int(dominant.max()),
                len(report["layers"]),
            )

    def test_projection_penumbra_samples_are_monotone_and_stable(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "scene.json"
            write_scene(
                make_sample_scene(
                    width=64,
                    height=40,
                    tile_width=32,
                    tile_height=20,
                    camera_samples=1,
                ),
                scene_path,
            )
            output = root / "out"
            result = _run(
                "render",
                "--scene",
                str(scene_path),
                "--output-dir",
                str(output),
                "--quiet",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            projection_path = output / "projection_landings.json"
            first_bytes = projection_path.read_bytes()
            projection = json.loads(first_bytes)
            receiver_widths = [
                item["penumbra_width"]
                for item in projection["penumbra_audit"]["receiver_distance_series"]
            ]
            source_widths = [
                item["penumbra_width"]
                for item in projection["penumbra_audit"]["source_distance_series"]
            ]
            self.assertEqual(receiver_widths, sorted(receiver_widths))
            self.assertEqual(source_widths, sorted(source_widths))
            for item in projection["penumbra_audit"]["receiver_distance_series"]:
                self.assertGreaterEqual(len(item["edge_profile"]), 9)
                luma = [sample["luma"] for sample in item["edge_profile"]]
                self.assertGreater(luma[-1], luma[0])
            rerun = _run(
                "render",
                "--scene",
                str(scene_path),
                "--output-dir",
                str(output),
                "--quiet",
            )
            self.assertEqual(rerun.returncode, 0, rerun.stderr)
            self.assertEqual(first_bytes, projection_path.read_bytes())


if __name__ == "__main__":
    unittest.main()

