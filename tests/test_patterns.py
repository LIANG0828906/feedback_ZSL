from __future__ import annotations

import unittest

import numpy as np

from gallery_louvers.patterns import (
    build_patterns,
    extract_pattern_arrays,
    generate_leaf_pattern,
    read_atlas_header,
)
from gallery_louvers.util import sha256_bytes

from .helpers import small_scene


class PatternTests(unittest.TestCase):
    def test_repeat_generation_is_byte_identical(self) -> None:
        scene = small_scene()
        first = build_patterns(scene)
        second = build_patterns(scene)
        self.assertEqual(first.digest, second.digest)
        self.assertEqual(first.atlas_bytes, second.atlas_bytes)

    def test_leaf_pattern_identity_is_seed_and_id_only(self) -> None:
        first = generate_leaf_pattern(12345, 2, 7, (64, 64), (16, 8), "TEST-V1")
        second = generate_leaf_pattern(12345, 2, 7, (64, 64), (16, 8), "TEST-V1")
        self.assertEqual(first.aperture.tobytes(), second.aperture.tobytes())
        self.assertEqual(first.microsurface.tobytes(), second.microsurface.tobytes())
        self.assertEqual(first.hole_centers.tobytes(), second.hole_centers.tobytes())

    def test_atlas_entry_decodes_to_original(self) -> None:
        scene = small_scene()
        patterns = build_patterns(scene)
        header = read_atlas_header(patterns.atlas_bytes)
        self.assertEqual(len(header["entries"]), len(scene.leaves))
        aperture, micro, centers = extract_pattern_arrays(
            patterns.atlas_bytes, 1, 3
        )
        original = patterns[(1, 3)]
        self.assertEqual(sha256_bytes(aperture.tobytes()), sha256_bytes(original.aperture.tobytes()))
        self.assertEqual(sha256_bytes(micro.tobytes()), sha256_bytes(original.microsurface.tobytes()))
        self.assertEqual(sha256_bytes(centers.tobytes()), sha256_bytes(original.hole_centers.tobytes()))

    def test_adjacent_leaves_are_distinguishable(self) -> None:
        scene = small_scene()
        patterns = build_patterns(scene)
        for layer in scene.layers:
            leaves = sorted(scene.leaves_by_layer[layer.layer], key=lambda item: item.leaf)
            for left, right in zip(leaves, leaves[1:]):
                difference = np.mean(
                    np.abs(
                        patterns[left.key].aperture.astype(np.float64)
                        - patterns[right.key].aperture.astype(np.float64)
                    )
                )
                self.assertGreater(float(difference), 0.005)


if __name__ == "__main__":
    unittest.main()
