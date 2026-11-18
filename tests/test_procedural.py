from __future__ import annotations

import copy
import unittest

import numpy as np

from bamboo_render.procedural import generate_textures
from bamboo_render.sample_scene import make_sample_scene
from bamboo_render.scene import load_scene_data


class ProceduralTextureTests(unittest.TestCase):
    def test_repeat_and_layer_order_are_byte_identical(self) -> None:
        raw = make_sample_scene(image_width=32, image_height=24, chunk_width=16, chunk_height=12)
        first = generate_textures(load_scene_data(copy.deepcopy(raw)).spec)
        second = generate_textures(load_scene_data(copy.deepcopy(raw)).spec)
        reversed_raw = copy.deepcopy(raw)
        reversed_raw["layers"].reverse()
        for layer in reversed_raw["layers"]:
            layer["mesh"]["triangles"].reverse()
            layer["mesh"]["strand_ids"].reverse()
        third = generate_textures(load_scene_data(reversed_raw).spec)
        for layer_number in (1, 2, 3):
            np.testing.assert_array_equal(
                first.layers[layer_number].fiber_u8,
                second.layers[layer_number].fiber_u8,
            )
            np.testing.assert_array_equal(
                first.layers[layer_number].hole_u8,
                third.layers[layer_number].hole_u8,
            )
            self.assertEqual(
                first.layers[layer_number].fiber_sha256,
                third.layers[layer_number].fiber_sha256,
            )

    def test_fiber_and_hole_masks_are_nonempty_and_layer_specific(self) -> None:
        raw = make_sample_scene(image_width=32, image_height=24, chunk_width=16, chunk_height=12)
        textures = generate_textures(load_scene_data(raw).spec)
        hole_fractions = []
        for layer_number in (1, 2, 3):
            texture = textures.layers[layer_number]
            self.assertEqual(texture.fiber_u8.shape, (192, 192))
            self.assertGreater(int(texture.fiber_u8.max()), int(texture.fiber_u8.min()))
            self.assertGreater(int(texture.hole_u8.max()), 0)
            self.assertLess(int(texture.hole_u8.min()), 255)
            hole_fraction = float(np.mean(texture.hole_u8 >= 128))
            self.assertGreater(hole_fraction, 0.05)
            self.assertLess(hole_fraction, 0.6)
            hole_fractions.append(hole_fraction)
        self.assertGreater(len({round(value, 4) for value in hole_fractions}), 1)
        self.assertFalse(
            np.array_equal(
                textures.layers[1].fiber_u8,
                textures.layers[2].fiber_u8,
            )
        )
        self.assertFalse(
            np.allclose(
                textures.layers[1].identity_tint,
                textures.layers[3].identity_tint,
            )
        )


if __name__ == "__main__":
    unittest.main()
