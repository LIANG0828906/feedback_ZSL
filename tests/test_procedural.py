from __future__ import annotations

import unittest

import numpy as np

from shadowbox.procedural import generate_textures
from shadowbox.scene import validate_scene

from tests.helpers import small_scene


class ProceduralTextureTests(unittest.TestCase):
    def test_texture_generation_is_byte_deterministic(self) -> None:
        first_scene = validate_scene(small_scene())
        second_scene = validate_scene(small_scene())
        first = generate_textures(first_scene)
        second = generate_textures(second_scene)
        for left, right in zip(first.layers, second.layers):
            self.assertTrue(np.array_equal(left.fibers, right.fibers))
            self.assertTrue(np.array_equal(left.holes, right.holes))
            self.assertEqual(left.fiber_sha256, right.fiber_sha256)
            self.assertEqual(left.hole_sha256, right.hole_sha256)
            self.assertGreater(left.hole_count, 0)
            self.assertGreater(float(np.max(left.fibers) - np.min(left.fibers)), 0.35)
            self.assertGreater(int(np.count_nonzero(left.holes)), 0)
            self.assertLess(int(np.count_nonzero(left.holes)), left.holes.size)

    def test_textures_do_not_depend_on_layer_input_order(self) -> None:
        forward = small_scene()
        reversed_scene = small_scene()
        reversed_scene["layers"] = list(reversed(reversed_scene["layers"]))
        left = generate_textures(validate_scene(forward))
        right = generate_textures(validate_scene(reversed_scene))
        for layer_index in range(5):
            self.assertEqual(
                left.for_layer(layer_index).fiber_sha256,
                right.for_layer(layer_index).fiber_sha256,
            )
            self.assertEqual(
                left.for_layer(layer_index).hole_sha256,
                right.for_layer(layer_index).hole_sha256,
            )

    def test_same_color_adjacent_layers_remain_distinguishable(self) -> None:
        scene = small_scene()
        for layer in scene["layers"]:
            layer["scatter_color"] = [0.82, 0.075, 0.045]
        textures = generate_textures(validate_scene(scene))
        for layer_index in range(4):
            self.assertNotEqual(
                textures.for_layer(layer_index).fiber_sha256,
                textures.for_layer(layer_index + 1).fiber_sha256,
            )
            self.assertNotEqual(
                textures.for_layer(layer_index).hole_sha256,
                textures.for_layer(layer_index + 1).hole_sha256,
            )


if __name__ == "__main__":
    unittest.main()
