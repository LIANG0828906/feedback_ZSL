from __future__ import annotations

import copy
import unittest

import numpy as np

from fogvol.imageio import rgba8
from fogvol.render import VolumetricRenderer
from fogvol.scene import parse_scene

from .common import LUMINANCE, compact_scene


def centroid(image: np.ndarray) -> tuple[float, float]:
    total = float(np.sum(image))
    height, width = image.shape
    x = float(np.sum(image.sum(axis=0) * np.arange(width)) / total)
    y = float(np.sum(image.sum(axis=1) * np.arange(height)) / total)
    return x, y


class RenderTests(unittest.TestCase):
    def test_floor_and_side_sheets_move_with_aim(self) -> None:
        raw = compact_scene()
        results = {}
        variants = {
            "floor_a": (0, [0.4, 0.75, 0.2]),
            "floor_b": (0, [-0.3, 0.8, 0.4]),
            "side_a": (1, [-0.9, 0.05, -0.4]),
            "side_b": (1, [-0.9, -0.35, 0.2]),
        }
        for name, (light_index, direction) in variants.items():
            variant = copy.deepcopy(raw)
            variant["lights"][light_index]["direction"] = direction
            scene = parse_scene(variant)
            renderer = VolumetricRenderer(scene)
            _, _, light_radiance = renderer.render_all(
                float(scene.render.step)
            )
            image = light_radiance[light_index] @ LUMINANCE
            results[name] = centroid(image)

        floor_shift = np.hypot(
            results["floor_a"][0] - results["floor_b"][0],
            results["floor_a"][1] - results["floor_b"][1],
        )
        side_shift = np.hypot(
            results["side_a"][0] - results["side_b"][0],
            results["side_a"][1] - results["side_b"][1],
        )
        self.assertGreater(floor_shift, 5.0)
        self.assertGreater(side_shift, 5.0)

    def test_brightness_monotonic_with_density_in_visible_band(self) -> None:
        raw = compact_scene()
        base_scene = parse_scene(raw)
        base_density = base_scene.volume.density
        maps = {}
        for scale in (0.3, 0.5, 0.7):
            variant = copy.deepcopy(raw)
            variant["volume"]["density"] = np.clip(
                base_density * scale, 0.0, 1.0
            ).reshape(-1).tolist()
            scene = parse_scene(variant)
            renderer = VolumetricRenderer(scene)
            _, _, light_radiance = renderer.render_all(
                float(scene.render.step)
            )
            maps[scale] = light_radiance @ LUMINANCE

        side = maps[0.3][1]
        y, x = np.unravel_index(int(np.argmax(side)), side.shape)
        samples = [float(maps[scale][1, y, x]) for scale in (0.3, 0.5, 0.7)]
        self.assertGreater(samples[0], 0.0)
        self.assertLess(samples[0], samples[1])
        self.assertLess(samples[1], samples[2])

    def test_step_convergence(self) -> None:
        scene = parse_scene(compact_scene())
        renderer = VolumetricRenderer(scene)
        fine_radiance, _, _ = renderer.render_all(
            float(scene.render.step)
        )
        half_radiance, _, _ = renderer.render_all(
            float(scene.render.step) * 0.5
        )
        fine_encoded, _ = renderer.tone_map(fine_radiance)
        half_encoded, _ = renderer.tone_map(half_radiance)
        fine_luminance = fine_encoded @ LUMINANCE
        half_luminance = half_encoded @ LUMINANCE
        visible = (fine_luminance > 1.0e-8) | (half_luminance > 1.0e-8)
        relative = np.abs(
            fine_luminance[visible] - half_luminance[visible]
        ) / np.maximum(
            0.5 * (fine_luminance[visible] + half_luminance[visible]),
            1.0e-5,
        )
        self.assertLessEqual(
            float(relative.mean()),
            scene.render.convergence_tolerance,
        )

    def test_chunk_partition_only_changes_roundoff(self) -> None:
        raw = compact_scene()
        outputs = []
        for chunk_size in ([20, 15], [7, 9], [31, 17]):
            variant = copy.deepcopy(raw)
            variant["render"]["chunk_size"] = chunk_size
            scene = parse_scene(variant)
            renderer = VolumetricRenderer(scene)
            outputs.append(renderer.render_all(float(scene.render.step)))
        for index in (1, 2):
            for channel in range(3):
                np.testing.assert_allclose(
                    outputs[0][channel],
                    outputs[index][channel],
                    rtol=0.0,
                    atol=1.0e-15,
                )

    def test_tone_mapping_caps_every_rgb_channel(self) -> None:
        raw = compact_scene()
        raw["render"]["tonemap"] = "linear"
        raw["render"]["gamma"] = 1.0
        raw["render"]["exposure"] = 100.0
        raw["render"]["channel_max"] = 0.35
        scene = parse_scene(raw)
        renderer = VolumetricRenderer(scene)
        encoded, stats = renderer.tone_map(
            np.full((3, 4, 3), 100.0, dtype=np.float64)
        )
        self.assertLessEqual(float(encoded.max()), 0.35)
        self.assertGreaterEqual(float(encoded.min()), 0.0)
        self.assertEqual(stats["clipped_pixels"], 12)
        self.assertEqual(stats["clipped_channels"], 36)
        opacity = np.full((3, 4), 0.5)
        pixels = rgba8(encoded, opacity)
        self.assertTrue(np.all(pixels[..., :3] <= 89))


if __name__ == "__main__":
    unittest.main()

