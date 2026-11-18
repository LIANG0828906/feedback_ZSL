from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from leather_raster.runner import run_render
from leather_raster.scene import validate_scene_object
from leather_raster.texture import fiber_texture

from .support import write_scene


def render_arrays(scene_path: Path, out_dir: Path) -> tuple[np.ndarray, np.ndarray]:
    scene = validate_scene_object(__import__("json").loads(scene_path.read_text(encoding="utf-8")))
    outcome = run_render(scene, out_dir, fresh=True)
    assert outcome.exit_code == 0
    rgba = np.fromfile(out_dir / "rgba.bin", dtype=np.uint8).reshape(scene.height, scene.width, 4)
    layers = np.fromfile(out_dir / "layer_map.bin", dtype=np.uint8).reshape(scene.height, scene.width)
    return rgba, layers


class DeterminismTests(unittest.TestCase):
    def test_texture_is_seeded_and_order_independent(self) -> None:
        u = np.linspace(0.0, 1.0, 97)
        v = np.linspace(1.0, 0.0, 97)
        first = fiber_texture(u, v, 1234567)
        second = fiber_texture(u, v, 1234567)
        other_seed = fiber_texture(u, v, 1234568)
        permutation = np.arange(len(u))[::-1]
        permuted = fiber_texture(u[permutation], v[permutation], 1234567)
        np.testing.assert_array_equal(first, second)
        np.testing.assert_array_equal(first[permutation], permuted)
        self.assertFalse(np.array_equal(first, other_seed))

    def test_two_full_runs_are_byte_identical(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, _ = write_scene(root, width=80, height=60, block_width=20, block_height=20)
            first = root / "first"
            second = root / "second"
            rgba_first, layers_first = render_arrays(scene_path, first)
            rgba_second, layers_second = render_arrays(scene_path, second)
            self.assertEqual((first / "rgba.bin").read_bytes(), (second / "rgba.bin").read_bytes())
            self.assertEqual((first / "layer_map.bin").read_bytes(), (second / "layer_map.bin").read_bytes())
            self.assertEqual(
                (first / "projection_points.json").read_bytes(),
                (second / "projection_points.json").read_bytes(),
            )
            np.testing.assert_array_equal(rgba_first, rgba_second)
            np.testing.assert_array_equal(layers_first, layers_second)

    def test_reordering_layer_array_does_not_change_pixels(self) -> None:
        import json

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=64, height=48, block_width=32, block_height=24)
            first = root / "normal"
            run_render(validate_scene_object(scene), first, fresh=True)

            reversed_scene = json.loads(json.dumps(scene))
            reversed_scene["layers"] = list(reversed(reversed_scene["layers"]))
            second = root / "reversed"
            run_render(validate_scene_object(reversed_scene), second, fresh=True)
            self.assertEqual((first / "rgba.bin").read_bytes(), (second / "rgba.bin").read_bytes())
            self.assertEqual((first / "layer_map.bin").read_bytes(), (second / "layer_map.bin").read_bytes())
            self.assertEqual(
                (first / "projection_points.json").read_bytes(),
                (second / "projection_points.json").read_bytes(),
            )

    def test_block_partition_does_not_change_pixels(self) -> None:
        import json

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=80, height=60, block_width=20, block_height=20)
            first = root / "small_blocks"
            run_render(validate_scene_object(scene), first, fresh=True)

            large_scene = json.loads(json.dumps(scene))
            large_scene["render"]["block_width"] = 80
            large_scene["render"]["block_height"] = 60
            second = root / "single_block"
            run_render(validate_scene_object(large_scene), second, fresh=True)
            self.assertEqual((first / "rgba.bin").read_bytes(), (second / "rgba.bin").read_bytes())
            self.assertEqual((first / "layer_map.bin").read_bytes(), (second / "layer_map.bin").read_bytes())

    def test_same_tint_neighbor_layers_use_seed_distinct_texture(self) -> None:
        import json

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=64, height=48, block_width=32, block_height=24)
            self.assertEqual(scene["layers"][0]["tint"], scene["layers"][1]["tint"])
            first = root / "seeded"
            second = root / "same_seed"
            run_render(validate_scene_object(scene), first, fresh=True)
            changed = json.loads(json.dumps(scene))
            changed["layers"][1]["fiber_seed"] = changed["layers"][0]["fiber_seed"]
            run_render(validate_scene_object(changed), second, fresh=True)
            self.assertNotEqual((first / "rgba.bin").read_bytes(), (second / "rgba.bin").read_bytes())

    def test_quality_guardrails(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scene_path, scene = write_scene(root, width=96, height=72, block_width=32, block_height=24)
            render_arrays(scene_path, root / "out")
            stats = __import__("json").loads((root / "out" / "stats.json").read_text(encoding="utf-8"))
            self.assertEqual(stats["image"]["fully_white_pixels"], 0)
            self.assertLess(stats["image"]["rgb_max"], 250)
            self.assertGreater(stats["image"]["mean_covered_saturation"], 0.35)
            for contribution in stats["layer_shading_contributions"]:
                self.assertGreaterEqual(contribution["transmission_ratio_percent"], 0.0)
                self.assertGreaterEqual(contribution["reflection_ratio_percent"], 0.0)
                self.assertAlmostEqual(
                    contribution["transmission_ratio_percent"]
                    + contribution["reflection_ratio_percent"],
                    100.0,
                    places=4,
                )


if __name__ == "__main__":
    unittest.main()

