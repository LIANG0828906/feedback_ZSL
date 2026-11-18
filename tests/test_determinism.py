from __future__ import annotations

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

from elytron.optics import build_film_tables, film_linear_rgb
from elytron.render import build_context, enumerate_blocks, render_block
from elytron.scene import canonical_bytes, generate_scene, validate_scene

ROOT = Path(__file__).resolve().parents[1]


class DeterminismTests(unittest.TestCase):
    def test_scene_generation_is_byte_stable(self) -> None:
        first = generate_scene(254)
        second = generate_scene(254)
        self.assertEqual(canonical_bytes(first), canonical_bytes(second))
        self.assertEqual(validate_scene(first).scene_digest, validate_scene(second).scene_digest)

    def test_tables_ignore_triangle_traversal_order(self) -> None:
        scene = generate_scene(254)
        first_bundle = validate_scene(scene)
        first = build_film_tables(first_bundle)

        reordered = copy.deepcopy(scene)
        permutation = np.random.default_rng(91).permutation(len(scene["mesh"]["triangles"]))
        reordered["mesh"]["triangles"] = [
            scene["mesh"]["triangles"][int(index)] for index in permutation
        ]
        reordered["mesh"]["face_ids"] = [
            scene["mesh"]["face_ids"][int(index)] for index in permutation
        ]
        reordered["mesh"]["curvature_seeds"] = [
            scene["mesh"]["curvature_seeds"][int(index)] for index in permutation
        ]
        second_bundle = validate_scene(reordered)
        second = build_film_tables(second_bundle)

        self.assertTrue(np.array_equal(first.thickness_delta, second.thickness_delta))
        self.assertTrue(np.array_equal(first.micro_normals, second.micro_normals))

    def test_repeated_table_generation_is_byte_identical(self) -> None:
        bundle = validate_scene(generate_scene(254))
        first = build_film_tables(bundle)
        second = build_film_tables(bundle)
        self.assertEqual(first.thickness_delta.tobytes(), second.thickness_delta.tobytes())
        self.assertEqual(first.micro_normals.tobytes(), second.micro_normals.tobytes())
        norms = np.linalg.norm(first.micro_normals[1:], axis=-1)
        self.assertLess(float(np.max(np.abs(norms - 1.0))), 1.0e-12)
        self.assertGreater(float(np.ptp(first.micro_normals[12, ..., 0])), 0.0)

    def test_face_seed_controls_microstructure_table(self) -> None:
        scene = generate_scene(254)
        first_bundle = validate_scene(scene)
        first = build_film_tables(first_bundle)
        changed = copy.deepcopy(scene)
        changed["mesh"]["curvature_seeds"][17] += 1
        second = build_film_tables(validate_scene(changed))
        self.assertFalse(
            np.array_equal(
                first.micro_normals[18],
                second.micro_normals[18],
            )
        )
        self.assertFalse(
            np.array_equal(
                first.thickness_delta[18],
                second.thickness_delta[18],
            )
        )

    def test_thickness_modulates_interference_color(self) -> None:
        film = generate_scene(254)["film"]
        colors = film_linear_rgb(
            np.array([420.0, 440.0, 460.0], dtype=np.float64),
            np.array([0.8, 0.8, 0.8], dtype=np.float64),
            film,
        )
        changed = np.max(np.abs(np.diff(colors, axis=0)), axis=1)
        self.assertGreater(float(np.min(changed)), 1.0e-4)

    def test_reversing_light_order_does_not_change_tiles(self) -> None:
        scene = generate_scene(254)
        first_bundle = validate_scene(scene)
        first = build_context(first_bundle)
        spec = enumerate_blocks(scene)[4]
        first_tile = render_block(first, scene["cameras"][spec.camera_index], spec)

        reversed_scene = copy.deepcopy(scene)
        reversed_scene["lights"].reverse()
        second_bundle = validate_scene(reversed_scene)
        second = build_context(second_bundle)
        second_tile = render_block(
            second,
            reversed_scene["cameras"][spec.camera_index],
            spec,
        )
        self.assertTrue(np.array_equal(first_tile.rgba, second_tile.rgba))
        self.assertTrue(np.array_equal(first_tile.face_id, second_tile.face_id))

    def test_cli_table_export_is_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path = root / "scene.json"
            first_dir = root / "first"
            second_dir = root / "second"
            generate = subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "elytron_render.py"),
                    "generate",
                    "--seed",
                    "254",
                    "--out",
                    str(scene_path),
                ],
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=30,
            )
            self.assertEqual(generate.returncode, 0, generate.stderr)
            for destination in (first_dir, second_dir):
                result = subprocess.run(
                    [
                        sys.executable,
                        str(ROOT / "elytron_render.py"),
                        "dump-tables",
                        "--scene",
                        str(scene_path),
                        "--out",
                        str(destination),
                    ],
                    cwd=ROOT,
                    capture_output=True,
                    text=True,
                    timeout=30,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
            for name in ("thickness_delta_nm.npy", "micro_normals.npy", "manifest.json"):
                self.assertEqual(
                    (first_dir / name).read_bytes(),
                    (second_dir / name).read_bytes(),
                    name,
                )
            manifest = json.loads((first_dir / "manifest.json").read_text(encoding="utf-8"))
            self.assertEqual(manifest["face_count"], 320)
            self.assertEqual(manifest["table_size"], 9)


if __name__ == "__main__":
    unittest.main()
