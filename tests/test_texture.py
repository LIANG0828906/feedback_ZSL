from __future__ import annotations

import unittest

import numpy as np

from celadon_museum.meshgen import make_scene
from celadon_museum.scene import validate_scene
from celadon_museum.texture import generate_layer_textures, sample_crackle


class TextureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scene = validate_scene(
            make_scene(
                width=96,
                height=64,
                tile_size=32,
                texture_resolution=64,
                radial_segments=16,
                rings=4,
            )
        )

    def test_tables_are_byte_stable_and_seed_dependent(self):
        layer = self.scene.layers[(0, 2)]
        first = generate_layer_textures(layer)
        second = generate_layer_textures(layer)
        self.assertTrue(np.array_equal(first.crackle, second.crackle))
        self.assertTrue(np.array_equal(first.carve, second.carve))
        self.assertEqual(first.crackle.tobytes(), second.crackle.tobytes())
        self.assertEqual(first.carve.tobytes(), second.carve.tobytes())
        self.assertEqual(first.crackle_digest, second.crackle_digest)
        other = generate_layer_textures(self.scene.layers[(1, 2)])
        self.assertFalse(np.array_equal(first.crackle, other.crackle))

    def test_crackle_is_independently_recomputable(self):
        layer = self.scene.layers[(2, 1)]
        textures = generate_layer_textures(layer)
        coordinates = [(0, 0), (1, 17), (31, 5), (63, 63), (22, 41)]
        for x, y in coordinates:
            self.assertEqual(
                float(textures.crackle[y, x]),
                sample_crackle(layer.seed, layer.texture_resolution, x, y),
            )
        junction_y, junction_x = np.argwhere(textures.junction_mask)[0]
        self.assertEqual(
            float(textures.crackle[junction_y, junction_x]),
            sample_crackle(
                layer.seed,
                layer.texture_resolution,
                int(junction_x),
                int(junction_y),
            ),
        )

    def test_junction_transition_is_counted(self):
        for key in sorted(self.scene.layers):
            textures = generate_layer_textures(self.scene.layers[key])
            self.assertGreater(textures.junction_samples, 0)
            self.assertEqual(
                textures.junction_samples,
                int(np.count_nonzero(textures.junction_mask)),
            )


if __name__ == "__main__":
    unittest.main()
