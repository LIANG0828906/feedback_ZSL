from __future__ import annotations

import unittest

import numpy as np

from gallery_louvers.patterns import build_patterns
from gallery_louvers.projection import ProjectionSystem
from gallery_louvers.render import render_block

from .helpers import small_scene


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = small_scene()
        cls.patterns = build_patterns(cls.scene)
        cls.projection = ProjectionSystem(cls.scene, cls.patterns)

    def test_block_repeat_is_byte_identical(self) -> None:
        first = render_block(self.scene, self.patterns, self.projection, 1, 4)
        second = render_block(self.scene, self.patterns, self.projection, 1, 4)
        self.assertEqual(first.rgba.tobytes(), second.rgba.tobytes())
        self.assertEqual(first.owner_id.tobytes(), second.owner_id.tobytes())
        self.assertEqual(first.owner_layer.tobytes(), second.owner_layer.tobytes())
        self.assertEqual(first.stats, second.stats)

    def test_owner_ids_are_valid_and_seed_reconstructible(self) -> None:
        result = render_block(self.scene, self.patterns, self.projection, 0, 0)
        leaf_mask = result.owner_layer < 4
        owner = result.owner_id[leaf_mask]
        self.assertTrue(np.all(owner > 0))
        layers = owner // 10000
        leaves = (owner - 1) % 10000
        valid_keys = {(leaf.layer, leaf.leaf) for leaf in self.scene.leaves}
        for layer, leaf in zip(layers, leaves):
            self.assertIn((int(layer), int(leaf)), valid_keys)
        self.assertTrue(np.all(result.rgba[..., 3] >= 0.0))
        self.assertTrue(np.all(result.rgba[..., 3] <= 1.0))


if __name__ == "__main__":
    unittest.main()
