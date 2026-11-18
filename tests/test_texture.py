from __future__ import annotations

import hashlib
import unittest

import numpy as np

from rosewindow.scene import parse_scene
from rosewindow.texture import TextureBank

from tests.helpers import small_scene_raw


class TextureTests(unittest.TestCase):
    def test_repeated_and_reordered_generation_is_byte_identical(self):
        scene = parse_scene(small_scene_raw())
        forward = TextureBank(scene.seed, scene.textures, scene.pieces)
        reverse = TextureBank(
            scene.seed, scene.textures, tuple(reversed(scene.pieces))
        )
        self.assertEqual(forward.digests(), reverse.digests())
        for piece in scene.pieces:
            left = forward.get(piece.serial)
            right = reverse.get(piece.serial)
            np.testing.assert_array_equal(left.transmission, right.transmission)
            np.testing.assert_array_equal(left.reflection, right.reflection)

    def test_same_colour_neighbours_have_distinct_tables(self):
        scene = parse_scene(small_scene_raw())
        bank = TextureBank(scene.seed, scene.textures, scene.pieces)
        blue_pieces = [
            piece for piece in scene.pieces if piece.quadrant == 0
        ]
        digests = {bank.get(piece.serial).digest() for piece in blue_pieces}
        self.assertEqual(len(digests), len(blue_pieces))
        self.assertGreater(len(blue_pieces), 1)

    def test_table_digest_is_stable(self):
        scene = parse_scene(small_scene_raw())
        first = TextureBank(scene.seed, scene.textures, scene.pieces)
        second = TextureBank(scene.seed, scene.textures, scene.pieces)
        self.assertEqual(first.digests(), second.digests())
        union = hashlib.sha256(
            "".join(
                f"{key}:{value};" for key, value in sorted(first.digests().items())
            ).encode("ascii")
        ).hexdigest()
        self.assertEqual(len(union), 64)


if __name__ == "__main__":
    unittest.main()
