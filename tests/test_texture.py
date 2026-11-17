from __future__ import annotations

import unittest

import numpy as np

from serene_render.scene import make_sample_scene, parse_scene
from serene_render.texture import generate_weave_texture, sample_weave


class TextureTests(unittest.TestCase):
    def tearDown(self) -> None:
        generate_weave_texture.cache_clear()

    def test_byte_identical_for_same_seed_and_resolution(self) -> None:
        generate_weave_texture.cache_clear()
        first = generate_weave_texture(1301, (128, 128))
        first_transmission = first.transmission.copy()
        first_color = first.color.copy()
        generate_weave_texture.cache_clear()
        second = generate_weave_texture(1301, (128, 128))
        self.assertEqual(first.digest, second.digest)
        np.testing.assert_array_equal(first_transmission, second.transmission)
        np.testing.assert_array_equal(first_color, second.color)

    def test_traversal_order_does_not_change_tables(self) -> None:
        generate_weave_texture.cache_clear()
        a = generate_weave_texture(31, (64, 80))
        b = generate_weave_texture(99, (48, 56))
        a_bytes = a.transmission.tobytes(), a.color.tobytes()
        generate_weave_texture.cache_clear()
        b2 = generate_weave_texture(99, (48, 56))
        a2 = generate_weave_texture(31, (64, 80))
        self.assertEqual(a.digest, a2.digest)
        self.assertEqual(b.digest, b2.digest)
        self.assertEqual(a_bytes[0], a2.transmission.tobytes())
        self.assertEqual(a_bytes[1], a2.color.tobytes())

    def test_different_seeds_are_distinguishable(self) -> None:
        first = generate_weave_texture(101, (96, 96))
        second = generate_weave_texture(102, (96, 96))
        self.assertNotEqual(first.digest, second.digest)
        self.assertFalse(np.array_equal(first.transmission, second.transmission))
        self.assertFalse(np.array_equal(first.color, second.color))

    def test_texture_modulates_both_transmission_and_tint(self) -> None:
        scene = parse_scene(make_sample_scene())
        layer = scene.layers[0]
        texture = generate_weave_texture(
            int(layer.material["pattern_seed"]),
            tuple(layer.material["texture_resolution"]),
        )
        transmission, color = sample_weave(texture, layer, layer.vertices)
        self.assertGreater(float(transmission.max() - transmission.min()), 0.02)
        self.assertGreater(float(color.min()), 0.0)
        self.assertGreater(float(color.max() - color.min()), 0.005)
        self.assertLessEqual(float(transmission.max()), 1.25)
        self.assertLessEqual(float(color.max()), 1.12)


if __name__ == "__main__":
    unittest.main()
